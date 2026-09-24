package tgc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

// Bad — ошибка во входных данных (нет сообщения, нет картинки…).
type Bad struct{ Msg string }

func (e *Bad) Error() string { return e.Msg }

// MaxImageBytes — потолок картинки для модели.
const MaxImageBytes = 5 * 1024 * 1024

var imageFormats = map[string]string{"image/jpeg": "jpeg", "image/png": "png", "image/webp": "webp", "image/gif": "gif"}

// Image — картинка из сообщения, в памяти.
type Image struct {
	Data    []byte
	Format  string
	Kind    string
	Caption string
}

func largestPhotoSize(sizes []tg.PhotoSizeClass) (typ string, inline []byte, size int) {
	for _, s := range sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			if v.Size >= size {
				typ, inline, size = v.Type, nil, v.Size
			}
		case *tg.PhotoSizeProgressive:
			n := 0
			for _, x := range v.Sizes {
				n = max(n, x)
			}
			if n >= size {
				typ, inline, size = v.Type, nil, n
			}
		case *tg.PhotoCachedSize:
			if len(v.Bytes) >= size && typ == "" {
				typ, inline, size = v.Type, v.Bytes, len(v.Bytes)
			}
		}
	}
	return typ, inline, size
}

func (c *Conn) download(ctx context.Context, loc tg.InputFileLocationClass, w interface{ Write([]byte) (int, error) }) error {
	_, err := downloader.NewDownloader().Download(c.API, loc).Stream(ctx, w)
	return err
}

func photoLocation(p *tg.Photo) (tg.InputFileLocationClass, []byte) {
	typ, inline, _ := largestPhotoSize(p.Sizes)
	if inline != nil {
		return nil, inline
	}
	return &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: typ}, nil
}

func documentLocation(d *tg.Document, thumb string) *tg.InputDocumentFileLocation {
	return &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference, ThumbSize: thumb}
}

// FetchImage — изображение из сообщения в память; на диск ничего не пишем.
// Фото и картинки-файлы отдаём целиком, у видео, кружков и гифок — превью-кадр.
func (c *Conn) FetchImage(ctx context.Context, t Target, msgID int) (*Image, error) {
	m, _, err := c.Message(ctx, t, msgID)
	if err != nil {
		return nil, err
	}
	msg, ok := m.(*tg.Message)
	if !ok || m == nil {
		return nil, &Bad{fmt.Sprintf("Сообщения %d нет (удалено или id не из этого чата).", msgID)}
	}
	doc := DocumentOf(msg)
	mimeType := ""
	if f := File(msg); f != nil {
		mimeType = f.MIME
	}
	img := &Image{Caption: msg.Message}
	var buf bytes.Buffer
	switch {
	case PhotoOf(msg) != nil:
		img.Kind, img.Format = "photo", "jpeg"
		if size := photoSize(PhotoOf(msg)); size > MaxImageBytes {
			return nil, &Bad{fmt.Sprintf("Картинка слишком большая: %d КБ, предел %d КБ.", size/1024, MaxImageBytes/1024)}
		}
		loc, inline := photoLocation(PhotoOf(msg))
		if inline != nil {
			buf.Write(inline)
		} else if err := c.download(ctx, loc, &buf); err != nil {
			return nil, err
		}
	case doc != nil && imageFormats[mimeType] != "":
		img.Kind, img.Format = "image_file", imageFormats[mimeType]
		if isSticker(doc) {
			img.Kind = "sticker"
		}
		if doc.Size > MaxImageBytes {
			return nil, &Bad{fmt.Sprintf("Картинка слишком большая: %d КБ, предел %d КБ.", doc.Size/1024, MaxImageBytes/1024)}
		}
		if err := c.download(ctx, documentLocation(doc, ""), &buf); err != nil {
			return nil, err
		}
	case doc != nil && (isVideo(doc) || IsVideoNote(msg) || isGIF(doc) || isSticker(doc)):
		img.Kind, img.Format = "preview_frame", "jpeg"
		typ, inline, _ := largestPhotoSize(doc.Thumbs)
		switch {
		case inline != nil:
			buf.Write(inline)
		case typ != "":
			if err := c.download(ctx, documentLocation(doc, typ), &buf); err != nil {
				return nil, err
			}
		}
	default:
		label := MediaLabel(msg)
		if label == "" {
			label = "None"
		}
		return nil, &Bad{fmt.Sprintf("В сообщении %d нет изображения (media: %s).", msgID, label)}
	}
	if buf.Len() == 0 {
		return nil, &Bad{fmt.Sprintf("У сообщения %d нет превью, показать нечего.", msgID)}
	}
	img.Data = buf.Bytes()
	return img, nil
}

func safeName(name string) string {
	var b strings.Builder
	for _, ch := range name {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || strings.ContainsRune(" ._-()[]", ch) {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	keep := strings.Trim(b.String(), " .")
	if r := []rune(keep); len(r) > 120 {
		keep = string(r[:120])
	}
	if keep == "" {
		return "file"
	}
	return keep
}

// Downloaded — результат скачивания вложения.
type Downloaded struct {
	Path     string
	Name     string
	MIME     string
	Size     int64
	Duration *float64
	Cached   bool
}

// DownloadFile — скачать вложение целиком: документ, видео, кружок, голосовое, фото.
func (c *Conn) DownloadFile(ctx context.Context, t Target, msgID int, destDir string, maxBytes int64) (*Downloaded, error) {
	m, _, err := c.Message(ctx, t, msgID)
	if err != nil {
		return nil, err
	}
	msg, ok := m.(*tg.Message)
	if !ok || m == nil {
		return nil, &Bad{fmt.Sprintf("Сообщения %d нет (удалено или id не из этого чата).", msgID)}
	}
	f := File(msg)
	if f == nil {
		label := MediaLabel(msg)
		if label == "" {
			label = "None"
		}
		return nil, &Bad{fmt.Sprintf("В сообщении %d нет файла (media: %s).", msgID, label)}
	}
	if f.Size > maxBytes {
		return nil, &Bad{fmt.Sprintf("Файл %d МБ больше предела %d МБ (TG_MAX_DOWNLOAD_MB в .env).",
			f.Size/(1024*1024), maxBytes/(1024*1024))}
	}
	name := f.Name
	if name == "" {
		label := MediaLabel(msg)
		if label == "" {
			label = "file"
		}
		name = label + f.Ext
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, err
	}
	target := filepath.Join(destDir, fmt.Sprintf("%d-%s", msgID, safeName(name)))
	cached := false
	if st, err := os.Stat(target); err == nil && st.Size() == f.Size {
		cached = true
	}
	if !cached {
		tmp := target + ".part"
		out, err := os.Create(tmp)
		if err != nil {
			return nil, err
		}
		var derr error
		if p := PhotoOf(msg); p != nil {
			loc, inline := photoLocation(p)
			if inline != nil {
				_, derr = out.Write(inline)
			} else {
				derr = c.download(ctx, loc, out)
			}
		} else if d := DocumentOf(msg); d != nil {
			derr = c.download(ctx, documentLocation(d, ""), out)
		} else {
			derr = errors.New("нечего скачивать")
		}
		cerr := out.Close()
		if derr != nil || cerr != nil {
			_ = os.Remove(tmp)
			return nil, errors.Join(derr, cerr)
		}
		if err := os.Rename(tmp, target); err != nil {
			return nil, err
		}
	}
	st, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	return &Downloaded{Path: target, Name: name, MIME: f.MIME, Size: st.Size(), Duration: f.Duration, Cached: cached}, nil
}

package attach

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Фото Telegram показывает прямо в ленте, но отклоняет крупные PNG/JPEG
// (> 2560 px по длинной стороне) и файлы больше 10 МБ. Подходящие картинки
// подготавливаем автоматически: масштабируем до 2560 px (с учётом EXIF для
// JPEG), PNG переводим в JPEG на белом фоне; экстремально вытянутые (> 1:20) и
// гигантские (> 25 Мп) оставляем документами.
const (
	photoMaxSide             = 2560
	photoMaxSize             = 10 << 20
	photoMaxRatio            = 20
	photoMaxPixels           = 25_000_000
	photoJPEGQuality         = 88
	photoJPEGQualityFallback = 75
)

// photoDecodeMu сериализует декодирование и ресайз, чтобы параллельные
// черновики не множили пиковую память службы.
var photoDecodeMu sync.Mutex

// preparePhoto проверяет и при необходимости подготавливает картинку к отправке
// фотографией. Если файл не подходит под фото (другое расширение, битый файл,
// вытянутость > 1:20, > 64 Мп, или JPEG не уложился в 10 МБ), возвращает ok=false
// без изменения исходного снимка — файл уйдёт документом.
func preparePhoto(srcPath, dstDir, origName string, size int64) (outPath, outName string, outSize int64, ok bool, err error) {
	ext := strings.ToLower(filepath.Ext(origName))
	switch ext {
	case ".jpg", ".jpeg", ".png":
	default:
		return "", "", 0, false, nil
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return "", "", 0, false, nil
	}
	cfg, format, err := image.DecodeConfig(f)
	_ = f.Close()
	if err != nil || (format != "jpeg" && format != "png") {
		return "", "", 0, false, nil
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return "", "", 0, false, nil
	}
	if int64(cfg.Width) > photoMaxPixels/int64(cfg.Height) {
		return "", "", 0, false, nil
	}
	long, short := max(cfg.Width, cfg.Height), min(cfg.Width, cfg.Height)
	if int64(long) > int64(short)*photoMaxRatio {
		return "", "", 0, false, nil
	}

	// Небольшой JPEG сохраняем как есть — не теряем исходное качество и EXIF.
	if (ext == ".jpg" || ext == ".jpeg") && format == "jpeg" && long <= photoMaxSide && size <= photoMaxSize {
		return srcPath, origName, size, true, nil
	}

	orient := 1
	if format == "jpeg" {
		orient = readEXIFOrientation(srcPath)
	}

	var buf bytes.Buffer
	func() {
		photoDecodeMu.Lock()
		defer photoDecodeMu.Unlock()

		f, openErr := os.Open(srcPath)
		if openErr != nil {
			return
		}
		img, _, decErr := image.Decode(f)
		_ = f.Close()
		if decErr != nil {
			return
		}
		b := img.Bounds()
		if b.Dx() <= 0 || b.Dy() <= 0 || int64(b.Dx()) > photoMaxPixels/int64(b.Dy()) {
			return
		}

		rgba := flattenWhite(img)
		if orient >= 2 && orient <= 8 {
			rgba = applyOrientation(rgba, orient)
		}
		w, h := rgba.Bounds().Dx(), rgba.Bounds().Dy()
		if max(w, h) > photoMaxSide {
			newW, newH := fitPhotoSize(w, h, photoMaxSide)
			rgba = downscaleArea(rgba, newW, newH)
		}

		if encErr := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: photoJPEGQuality}); encErr != nil {
			buf.Reset()
			return
		}
		if int64(buf.Len()) > photoMaxSize {
			buf.Reset()
			if encErr := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: photoJPEGQualityFallback}); encErr != nil || int64(buf.Len()) > photoMaxSize {
				buf.Reset()
				return
			}
		}
	}()
	if buf.Len() == 0 {
		return "", "", 0, false, nil
	}

	outName = strings.TrimSuffix(origName, filepath.Ext(origName)) + ".jpg"
	outPath = filepath.Join(dstDir, outName)
	tmp, err := os.CreateTemp(dstDir, ".photo-*.tmp")
	if err != nil {
		return "", "", 0, false, err
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr != nil {
		_ = os.Remove(tmpPath)
		return "", "", 0, false, werr
	}
	if cerr != nil {
		_ = os.Remove(tmpPath)
		return "", "", 0, false, cerr
	}
	_ = os.Chmod(tmpPath, 0o600)

	sameDir := isSameDir(filepath.Dir(srcPath), dstDir)
	sameNameCI := strings.EqualFold(filepath.Base(srcPath), outName)
	if sameDir && sameNameCI {
		// На регистронезависимой ФС (Windows) удаляем исходник с тем же именем
		// (например, PHOTO.JPG → PHOTO.jpg) до Rename.
		_ = os.Remove(srcPath)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", "", 0, false, err
	}
	if sameDir && !sameNameCI {
		_ = os.Remove(srcPath)
	}
	return outPath, outName, int64(buf.Len()), true, nil
}

func isSameDir(a, b string) bool {
	fi1, err1 := os.Stat(a)
	fi2, err2 := os.Stat(b)
	if err1 == nil && err2 == nil {
		return os.SameFile(fi1, fi2)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func fitPhotoSize(w, h, maxSide int) (int, int) {
	if w >= h {
		newH := int(math.Round(float64(h) * float64(maxSide) / float64(w)))
		return maxSide, max(1, newH)
	}
	newW := int(math.Round(float64(w) * float64(maxSide) / float64(h)))
	return max(1, newW), maxSide
}

// flattenWhite сводит изображение в непрозрачный RGBA на белом фоне с началом в (0, 0).
func flattenWhite(img image.Image) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	if _, opaque := img.(*image.YCbCr); opaque {
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
		return dst
	}
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Over)
	return dst
}

// applyOrientation применяет все 8 стандартных значений EXIF Orientation (1..8).
func applyOrientation(src *image.RGBA, orient int) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	var dst *image.RGBA
	switch orient {
	case 2, 3, 4:
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	case 5, 6, 7, 8:
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	default:
		return src
	}

	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()
	for dy := range dh {
		dstRow := dy * dst.Stride
		for dx := range dw {
			var sx, sy int
			switch orient {
			case 2: // горизонтальное отражение
				sx, sy = w-1-dx, dy
			case 3: // поворот на 180°
				sx, sy = w-1-dx, h-1-dy
			case 4: // вертикальное отражение
				sx, sy = dx, h-1-dy
			case 5: // транспонирование (отражение по главной диагонали)
				sx, sy = dy, dx
			case 6: // поворот на 90° по часовой стрелке
				sx, sy = dy, h-1-dx
			case 7: // отражение по побочной диагонали
				sx, sy = w-1-dy, h-1-dx
			case 8: // поворот на 270° по часовой (90° против часовой)
				sx, sy = w-1-dy, dx
			}
			srcOff := sy*src.Stride + sx*4
			dstOff := dstRow + dx*4
			copy(dst.Pix[dstOff:dstOff+4], src.Pix[srcOff:srcOff+4])
		}
	}
	return dst
}

// downscaleArea уменьшает непрозрачный RGBA точным разделимым area-averaging
// (box filter) без выделения промежуточного полноразмерного буфера.
func downscaleArea(src *image.RGBA, dstW, dstH int) *image.RGBA {
	srcW, srcH := src.Bounds().Dx(), src.Bounds().Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))

	type xSpan struct {
		i0, i1 int
		x0, x1 float64
		invW   float64
	}
	scaleX := float64(srcW) / float64(dstW)
	scaleY := float64(srcH) / float64(dstH)

	spans := make([]xSpan, dstW)
	for dx := range dstW {
		x0 := float64(dx) * scaleX
		x1 := float64(dx+1) * scaleX
		if dx == dstW-1 || x1 > float64(srcW) {
			x1 = float64(srcW)
		}
		i0 := int(x0)
		i1 := min(int(math.Ceil(x1)), srcW)
		if i1 <= i0 {
			i1 = i0 + 1
		}
		spans[dx] = xSpan{i0: i0, i1: i1, x0: x0, x1: x1, invW: 1.0 / (x1 - x0)}
	}

	rowBuf := make([]float64, dstW*3)
	acc := make([]float64, dstW*3)
	cachedSY := -1

	scaleRow := func(sy int, out []float64) {
		rowOff := sy * src.Stride
		for dx, sp := range spans {
			var r, g, b float64
			for sx := sp.i0; sx < sp.i1; sx++ {
				lo := max(float64(sx), sp.x0)
				hi := min(float64(sx+1), sp.x1)
				w := hi - lo
				off := rowOff + sx*4
				r += float64(src.Pix[off]) * w
				g += float64(src.Pix[off+1]) * w
				b += float64(src.Pix[off+2]) * w
			}
			base := dx * 3
			out[base] = r * sp.invW
			out[base+1] = g * sp.invW
			out[base+2] = b * sp.invW
		}
	}

	for dy := range dstH {
		y0 := float64(dy) * scaleY
		y1 := float64(dy+1) * scaleY
		if dy == dstH-1 || y1 > float64(srcH) {
			y1 = float64(srcH)
		}
		iy0 := int(y0)
		iy1 := min(int(math.Ceil(y1)), srcH)
		if iy1 <= iy0 {
			iy1 = iy0 + 1
		}
		clear(acc)
		for sy := iy0; sy < iy1; sy++ {
			if sy != cachedSY {
				scaleRow(sy, rowBuf)
				cachedSY = sy
			}
			lo := max(float64(sy), y0)
			hi := min(float64(sy+1), y1)
			wy := hi - lo
			for i, v := range rowBuf {
				acc[i] += v * wy
			}
		}
		invH := 1.0 / (y1 - y0)
		dstOff := dy * dst.Stride
		for dx := range dstW {
			base := dx * 3
			off := dstOff + dx*4
			dst.Pix[off] = clampByte(acc[base] * invH)
			dst.Pix[off+1] = clampByte(acc[base+1] * invH)
			dst.Pix[off+2] = clampByte(acc[base+2] * invH)
			dst.Pix[off+3] = 255
		}
	}
	return dst
}

func clampByte(v float64) uint8 {
	n := int(math.Round(v))
	if n < 0 {
		return 0
	}
	if n > 255 {
		return 255
	}
	return uint8(n)
}

// readEXIFOrientation читает тег Orientation (0x0112) из APP1/Exif JPEG-файла.
// При отсутствии EXIF или любой ошибке возвращает 1 (без поворота).
func readEXIFOrientation(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	return parseEXIFOrientation(f)
}

func parseEXIFOrientation(r io.Reader) int {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr[0] != 0xFF || hdr[1] != 0xD8 {
		return 1
	}
	var b [1]byte
	for {
		// Ищем маркер 0xFF.
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 1
		}
		if b[0] != 0xFF {
			return 1
		}
		// Пропускаем возможные заполняющие 0xFF.
		var marker byte
		for {
			if _, err := io.ReadFull(r, b[:]); err != nil {
				return 1
			}
			if b[0] != 0xFF {
				marker = b[0]
				break
			}
		}
		// Конец заголовков (SOS/EOI).
		if marker == 0xDA || marker == 0xD9 {
			return 1
		}
		// Маркеры без длины (SOI, RST0..RST7, TEM).
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		var lenBuf [2]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(lenBuf[:]))
		if segLen < 2 {
			return 1
		}
		payloadLen := segLen - 2
		if marker != 0xE1 {
			if _, err := io.CopyN(io.Discard, r, int64(payloadLen)); err != nil {
				return 1
			}
			continue
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return 1
		}
		if orient := parseTIFFOrientation(payload); orient >= 1 && orient <= 8 {
			return orient
		}
	}
}

// parseTIFFOrientation разбирает содержимое APP1 ("Exif\x00\x00" + TIFF IFD0).
// Возвращает 0, если это не EXIF-сегмент или тег 0x0112 не найден.
func parseTIFFOrientation(payload []byte) int {
	if len(payload) < 14 || !bytes.Equal(payload[:6], []byte("Exif\x00\x00")) {
		return 0
	}
	tiff := payload[6:]
	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(tiff[2:4]) != 42 {
		return 0
	}
	ifd0u := uint64(bo.Uint32(tiff[4:8]))
	if ifd0u < 8 || ifd0u > uint64(len(tiff)-2) {
		return 0
	}
	ifd0 := int(ifd0u)
	count := int(bo.Uint16(tiff[ifd0 : ifd0+2]))
	pos := ifd0 + 2
	for range count {
		if pos+12 > len(tiff) {
			break
		}
		tag := bo.Uint16(tiff[pos : pos+2])
		typ := bo.Uint16(tiff[pos+2 : pos+4])
		cnt := bo.Uint32(tiff[pos+4 : pos+8])
		if tag == 0x0112 && cnt == 1 {
			var val int
			switch typ {
			case 3: // SHORT
				val = int(bo.Uint16(tiff[pos+8 : pos+10]))
			case 4: // LONG
				val = int(bo.Uint32(tiff[pos+8 : pos+12]))
			}
			if val >= 1 && val <= 8 {
				return val
			}
		}
		pos += 12
	}
	return 0
}

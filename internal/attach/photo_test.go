package attach

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

func setupAttachEnv(t *testing.T) *config.Settings {
	t.Helper()
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	return &config.Settings{MaxFileMB: 50}
}

func writePNGImage(t *testing.T, dir, name string, img image.Image) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJPEGImage(t *testing.T, dir, name string, img image.Image, orient int, bo binary.ByteOrder) string {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if orient != 0 {
		data = injectEXIFOrientation(data, orient, bo)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// injectEXIFOrientation вставляет минимальный сегмент APP1/Exif с тегом 0x0112 сразу после SOI.
func injectEXIFOrientation(jpegBytes []byte, orient int, bo binary.ByteOrder) []byte {
	var tiff bytes.Buffer
	if bo == binary.LittleEndian {
		tiff.WriteString("II")
	} else {
		tiff.WriteString("MM")
	}
	_ = binary.Write(&tiff, bo, uint16(42))     // TIFF magic
	_ = binary.Write(&tiff, bo, uint32(8))      // offset to IFD0
	_ = binary.Write(&tiff, bo, uint16(1))      // 1 entry in IFD0
	_ = binary.Write(&tiff, bo, uint16(0x0112)) // tag: Orientation
	_ = binary.Write(&tiff, bo, uint16(3))      // type: SHORT
	_ = binary.Write(&tiff, bo, uint32(1))      // count: 1
	_ = binary.Write(&tiff, bo, uint16(orient)) // value (left-aligned in 4 bytes)
	_ = binary.Write(&tiff, bo, uint16(0))      // padding
	_ = binary.Write(&tiff, bo, uint32(0))      // next IFD offset = 0

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var app1 bytes.Buffer
	app1.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&app1, binary.BigEndian, uint16(len(payload)+2))
	app1.Write(payload)

	var out bytes.Buffer
	out.Write(jpegBytes[:2]) // SOI
	out.Write(app1.Bytes())
	out.Write(jpegBytes[2:])
	return out.Bytes()
}

// pngHeaderOnly создаёт минимальный PNG с валидным IHDR (без пикселей) —
// DecodeConfig его читает, а Decode падает.
func pngHeaderOnly(t *testing.T, dir, name string, w, h uint32) string {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	_ = binary.Write(&ihdr, binary.BigEndian, w)
	_ = binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // 8-bit RGB
	_ = binary.Write(&buf, binary.BigEndian, uint32(13))
	buf.Write(ihdr.Bytes())
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))
	// Повреждённый/обрезанный IDAT после валидного IHDR.
	buf.Write([]byte{0, 0, 0, 4, 'I', 'D', 'A', 'T', 0xde, 0xad})
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeFile(t *testing.T, path string) (image.Image, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, format, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return img, format
}

func TestPhotoResizeLargePNG(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	src := pngFile(t, dir, "v5-01-zakaz.png", 2162, 3192)
	origBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	snaps, err := Snapshot(s, "d-large-png", []string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	snap := snaps[0]
	if snap.Kind != outbox.KindPhoto {
		t.Fatalf("Kind=%q, want %q", snap.Kind, outbox.KindPhoto)
	}
	if snap.Name != "v5-01-zakaz.jpg" || filepath.Base(snap.Path) != "v5-01-zakaz.jpg" {
		t.Fatalf("ожидали v5-01-zakaz.jpg, получили name=%q path=%q", snap.Name, snap.Path)
	}
	// Старая .png-копия в папке снимка должна быть удалена.
	oldCopy := filepath.Join(filepath.Dir(snap.Path), "v5-01-zakaz.png")
	if _, err := os.Stat(oldCopy); !os.IsNotExist(err) {
		t.Fatalf("исходная копия .png не удалена: %v", err)
	}
	st, err := os.Stat(snap.Path)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Size != st.Size() {
		t.Fatalf("snap.Size=%d != disk size=%d", snap.Size, st.Size())
	}

	img, format := decodeFile(t, snap.Path)
	if format != "jpeg" {
		t.Fatalf("format=%q, want jpeg", format)
	}
	b := img.Bounds()
	if b.Dx() != 1734 || b.Dy() != 2560 {
		t.Fatalf("размеры=%dx%d, ждали 1734x2560", b.Dx(), b.Dy())
	}

	// Исходный файл на диске не изменён.
	afterBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origBytes, afterBytes) {
		t.Fatal("исходный PNG на диске был изменён")
	}
}

func TestPhotoPNGTransparencyWhiteBackground(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	for y := range 200 {
		for x := 100; x < 200; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 255, A: 255})
		}
		// Левая половина (x < 100) остаётся полностью прозрачной (0,0,0,0).
	}
	src := writePNGImage(t, dir, "transparent.png", img)

	snaps, err := Snapshot(s, "d-alpha", []string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0].Kind != outbox.KindPhoto {
		t.Fatalf("Kind=%q, want photo", snaps[0].Kind)
	}
	out, format := decodeFile(t, snaps[0].Path)
	if format != "jpeg" {
		t.Fatalf("format=%q, want jpeg", format)
	}
	r, g, b, _ := out.At(25, 100).RGBA()
	if r>>8 < 245 || g>>8 < 245 || b>>8 < 245 {
		t.Fatalf("прозрачный фон стал не белым: rgb=(%d,%d,%d)", r>>8, g>>8, b>>8)
	}
	r, g, b, _ = out.At(175, 100).RGBA()
	if b>>8 < 230 || r>>8 > 25 || g>>8 > 25 {
		t.Fatalf("непрозрачный синий исказился: rgb=(%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

func TestPhotoSmallJPEGPassthrough(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for y := range 600 {
		for x := range 800 {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 100, A: 255})
		}
	}
	src := writeJPEGImage(t, dir, "small.jpg", img, 0, nil)
	origBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	snaps, err := Snapshot(s, "d-small-jpg", []string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	snap := snaps[0]
	if snap.Kind != outbox.KindPhoto || snap.Name != "small.jpg" {
		t.Fatalf("unexpected snap: %+v", snap)
	}
	snapBytes, err := os.ReadFile(snap.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origBytes, snapBytes) {
		t.Fatal("небольшой JPEG был перекодирован, хотя должен был сохраниться побитово")
	}
}

func TestPhotoLargeJPEGWithEXIFOrientation6(t *testing.T) {
	for _, tc := range []struct {
		name string
		bo   binary.ByteOrder
	}{
		{"little-endian", binary.LittleEndian},
		{"big-endian", binary.BigEndian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupAttachEnv(t)
			dir := t.TempDir()
			// Исходник 3200x2400: красный квадрат в левом нижнем углу (x<400, y>=2000).
			// После поворота на 90° CW (Orientation=6) этот угол должен оказаться в левом верхнем!
			img := image.NewRGBA(image.Rect(0, 0, 3200, 2400))
			for y := range 2400 {
				for x := range 3200 {
					c := color.RGBA{R: 255, G: 255, B: 255, A: 255}
					if x < 400 && y >= 2000 {
						c = color.RGBA{R: 220, G: 20, B: 20, A: 255}
					}
					img.SetRGBA(x, y, c)
				}
			}
			src := writeJPEGImage(t, dir, "phone.jpg", img, 6, tc.bo)

			snaps, err := Snapshot(s, "d-exif6", []string{src}, false)
			if err != nil {
				t.Fatal(err)
			}
			snap := snaps[0]
			if snap.Kind != outbox.KindPhoto || snap.Name != "phone.jpg" {
				t.Fatalf("unexpected snap: %+v", snap)
			}
			out, format := decodeFile(t, snap.Path)
			if format != "jpeg" {
				t.Fatalf("format=%q, want jpeg", format)
			}
			b := out.Bounds()
			if b.Dx() != 1920 || b.Dy() != 2560 {
				t.Fatalf("размеры=%dx%d, ждали 1920x2560", b.Dx(), b.Dy())
			}
			// Проверяем, что красный квадрат переехал в левый верхний угол.
			r, g, bl, _ := out.At(100, 100).RGBA()
			if r>>8 < 180 || g>>8 > 60 || bl>>8 > 60 {
				t.Fatalf("левый верхний угол не красный после поворота: rgb=(%d,%d,%d)", r>>8, g>>8, bl>>8)
			}
			// А остальные углы остались белыми.
			for _, pt := range []image.Point{{1820, 100}, {100, 2460}, {1820, 2460}} {
				r, g, bl, _ := out.At(pt.X, pt.Y).RGBA()
				if r>>8 < 230 || g>>8 < 230 || bl>>8 < 230 {
					t.Fatalf("угол %v не белый: rgb=(%d,%d,%d)", pt, r>>8, g>>8, bl>>8)
				}
			}
		})
	}
}

func TestEXIFAllOrientations(t *testing.T) {
	// 3x2 с уникальными значениями R в каждом пикселе:
	// [10, 20, 30]
	// [40, 50, 60]
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	vals := [2][3]uint8{{10, 20, 30}, {40, 50, 60}}
	for y := range 2 {
		for x := range 3 {
			src.SetRGBA(x, y, color.RGBA{R: vals[y][x], G: 0, B: 0, A: 255})
		}
	}

	want := map[int][][]uint8{
		1: {{10, 20, 30}, {40, 50, 60}},
		2: {{30, 20, 10}, {60, 50, 40}},
		3: {{60, 50, 40}, {30, 20, 10}},
		4: {{40, 50, 60}, {10, 20, 30}},
		5: {{10, 40}, {20, 50}, {30, 60}},
		6: {{40, 10}, {50, 20}, {60, 30}},
		7: {{60, 30}, {50, 20}, {40, 10}},
		8: {{30, 60}, {20, 50}, {10, 40}},
	}

	for orient := 1; orient <= 8; orient++ {
		got := applyOrientation(src, orient)
		exp := want[orient]
		if got.Bounds().Dy() != len(exp) || got.Bounds().Dx() != len(exp[0]) {
			t.Fatalf("orient %d: size=%dx%d, want %dx%d", orient, got.Bounds().Dx(), got.Bounds().Dy(), len(exp[0]), len(exp))
		}
		for y, row := range exp {
			for x, wv := range row {
				if gv := got.RGBAAt(x, y).R; gv != wv {
					t.Errorf("orient %d (%d,%d): got %d, want %d", orient, x, y, gv, wv)
				}
			}
		}
		// Заодно проверяем парсер EXIF на обоих порядках байт.
		for _, bo := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			var buf bytes.Buffer
			_ = jpeg.Encode(&buf, src, nil)
			withEXIF := injectEXIFOrientation(buf.Bytes(), orient, bo)
			if parsed := parseEXIFOrientation(bytes.NewReader(withEXIF)); parsed != orient {
				t.Errorf("parseEXIFOrientation orient=%d: got %d", orient, parsed)
			}
		}
	}
}

func TestPhotoExtremeAspectRatioDocument(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	src := pngFile(t, dir, "tall.png", 100, 2500) // ratio 1:25 > 1:20
	origBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	snaps, err := Snapshot(s, "d-ratio", []string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0].Kind != "" || snaps[0].Name != "tall.png" {
		t.Fatalf("вытянутая картинка должна уйти документом без изменений: %+v", snaps[0])
	}
	snapBytes, err := os.ReadFile(snaps[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origBytes, snapBytes) {
		t.Fatal("вытянутая картинка была изменена")
	}
}

func TestPhotoAsFilesUntouched(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	src := pngFile(t, dir, "big.png", 2600, 3200)
	origBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	snaps, err := Snapshot(s, "d-asfiles", []string{src}, true)
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0].Kind != "" || snaps[0].Name != "big.png" {
		t.Fatalf("as_files=true должен оставить документ .png: %+v", snaps[0])
	}
	snapBytes, err := os.ReadFile(snaps[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(origBytes, snapBytes) {
		t.Fatal("при as_files=true файл не должен изменяться")
	}
}

func TestPhotoCorruptPNGDocument(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.png")
	if err := os.WriteFile(bad, []byte("not a png at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	trunc := pngHeaderOnly(t, dir, "trunc.png", 800, 600)

	snaps, err := Snapshot(s, "d-corrupt", []string{bad, trunc}, false)
	if err != nil {
		t.Fatalf("битые PNG не должны ронять Snapshot: %v", err)
	}
	for i, snap := range snaps {
		if snap.Kind != "" {
			t.Errorf("snaps[%d] (%s): Kind=%q, ждали документ", i, snap.Name, snap.Kind)
		}
	}
}

func TestPhotoExceedsMaxPixelsDocument(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	// Валидный PNG 5001×5000 (25.005 Мп > 25 Мп): без лимита успешно декодировался бы.
	img := image.NewPaletted(image.Rect(0, 0, 5001, 5000), color.Palette{color.White})
	huge := writePNGImage(t, dir, "huge.png", img)

	snaps, err := Snapshot(s, "d-huge", []string{huge}, false)
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0].Kind != "" || snaps[0].Name != "huge.png" {
		t.Fatalf("гигантская картинка должна остаться документом: %+v", snaps[0])
	}
}

func TestFlattenWhiteSemiTransparent(t *testing.T) {
	// Полупрозрачный красный (200, 0, 0, 128) поверх белого должен стать (~228, ~127, ~127).
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 0, B: 0, A: 128})
	dst := flattenWhite(src)
	p := dst.RGBAAt(0, 0)
	if p.A != 255 || p.R < 220 || p.R > 235 || p.G < 120 || p.G > 135 || p.B < 120 || p.B > 135 {
		t.Fatalf("неожиданный композитинг полупрозрачного пикселя: %+v", p)
	}
}

func TestDownscaleAreaFractionalEdges(t *testing.T) {
	// Дробный масштаб (7×5 → 3×2): проверяем отсутствие выхода за границы и
	// корректное усреднение на последней строке/столбце.
	src := image.NewRGBA(image.Rect(0, 0, 7, 5))
	for y := range 5 {
		for x := range 7 {
			v := uint8(200)
			if x == 6 || y == 4 {
				v = 40
			}
			src.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	dst := downscaleArea(src, 3, 2)
	if dst.Bounds().Dx() != 3 || dst.Bounds().Dy() != 2 {
		t.Fatalf("неожиданный размер: %v", dst.Bounds())
	}
	tl := dst.RGBAAt(0, 0).R
	br := dst.RGBAAt(2, 1).R
	if tl != 200 || br >= 160 || br <= 60 {
		t.Fatalf("ожидали tl=200 и смешанный br, получили tl=%d br=%d", tl, br)
	}
}

func TestPhotoUppercaseJPEGExtension(t *testing.T) {
	s := setupAttachEnv(t)
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 3000, 2000))
	src := writeJPEGImage(t, dir, "PHOTO.JPG", img, 0, nil)

	snaps, err := Snapshot(s, "d-upper", []string{src}, false)
	if err != nil {
		t.Fatal(err)
	}
	snap := snaps[0]
	if snap.Kind != outbox.KindPhoto || snap.Name != "PHOTO.jpg" {
		t.Fatalf("unexpected snap: %+v", snap)
	}
	out, format := decodeFile(t, snap.Path)
	if format != "jpeg" {
		t.Fatalf("format=%q, want jpeg", format)
	}
	if out.Bounds().Dx() != 2560 || out.Bounds().Dy() != 1707 {
		t.Fatalf("size=%dx%d, want 2560x1707", out.Bounds().Dx(), out.Bounds().Dy())
	}
}

func TestDownscaleAreaAveraging(t *testing.T) {
	// Чередующиеся вертикальные полосы 0 и 255: area-averaging 2x должен дать ~128,
	// а не 0 или 255 (как дал бы nearest-neighbor).
	src := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for y := range 200 {
		for x := range 200 {
			v := uint8(0)
			if x%2 == 1 {
				v = 255
			}
			src.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	dst := downscaleArea(src, 100, 100)
	px := dst.RGBAAt(50, 50)
	if px.R < 126 || px.R > 129 || px.G != px.R || px.B != px.R {
		t.Fatalf("ожидали усреднение ~128, получили %+v", px)
	}
}

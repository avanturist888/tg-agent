package attach

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"tgagent/internal/config"
	"tgagent/internal/outbox"
)

func pngFile(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPhotoOrDocument(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		path  string
		photo bool
	}{
		{pngFile(t, dir, "slide.png", 1800, 1060), true},
		{pngFile(t, dir, "long.png", 100, 3000), false},  // вытянута сильнее 1:20
		{pngFile(t, dir, "huge.png", 6000, 5000), false}, // сумма сторон больше 10000
	}
	pdf := filepath.Join(dir, "report.pdf")
	os.WriteFile(pdf, []byte("%PDF-1.4"), 0o600)
	cases = append(cases, struct {
		path  string
		photo bool
	}{pdf, false})
	fake := filepath.Join(dir, "fake.png") // не картинка, хоть и .png
	os.WriteFile(fake, []byte("не png"), 0o600)
	cases = append(cases, struct {
		path  string
		photo bool
	}{fake, false})

	for _, c := range cases {
		st, _ := os.Stat(c.path)
		if got := photoOK(c.path, st.Size()); got != c.photo {
			t.Errorf("%s: фото=%v, ждали %v", filepath.Base(c.path), got, c.photo)
		}
	}
}

func TestSnapshotKinds(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	s := &config.Settings{MaxFileMB: 50}
	dir := t.TempDir()
	img := pngFile(t, dir, "slide.png", 800, 600)
	pdf := filepath.Join(dir, "report.pdf")
	os.WriteFile(pdf, []byte("%PDF-1.4"), 0o600)

	snaps, err := Snapshot(s, "d1", []string{img, pdf}, false)
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0].Kind != outbox.KindPhoto || snaps[1].Kind != "" {
		t.Fatalf("картинка — фото, pdf — документ: %+v", snaps)
	}
	snaps, err = Snapshot(s, "d2", []string{img}, true)
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0].Kind != "" {
		t.Fatal("as_files: картинка должна уйти документом")
	}
}

func TestDenied(t *testing.T) {
	for _, p := range []string{`C:\x\.env`, `C:\x\.env.production`, `C:\u\.ssh\config`, `D:\keys\server.pem`, `C:\a\user.session`} {
		if deniedReason(p) == "" {
			t.Errorf("пропущен секрет %s", p)
		}
	}
	for _, p := range []string{`C:\docs\report.pdf`, `C:\docs\environment.txt`} {
		if r := deniedReason(p); r != "" {
			t.Errorf("ложный отказ %s: %s", p, r)
		}
	}
}

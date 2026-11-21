package attach

import (
	"errors"
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
		{pngFile(t, dir, "long.png", 100, 3000), false},         // вытянута сильнее 1:20
		{pngHeaderOnly(t, dir, "huge.png", 10000, 7000), false}, // > 25 Мп
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
		dstDir := t.TempDir()
		_, _, _, got, err := preparePhoto(c.path, dstDir, filepath.Base(c.path), st.Size())
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", filepath.Base(c.path), err)
		}
		if got != c.photo {
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
	// тот же id у параллельного вызова — не его каталог: ErrTaken, а
	// снимки первого целы
	if _, err := Snapshot(s, "d1", []string{pdf}, false); !errors.Is(err, ErrTaken) {
		t.Fatalf("занятый id: %v", err)
	}
	if _, err := os.Stat(snaps[0].Path); err != nil {
		t.Fatal("снимки первого пропали:", err)
	}
	snaps, err = Snapshot(s, "d2", []string{img}, true)
	if err != nil {
		t.Fatal(err)
	}
	if snaps[0].Kind != "" {
		t.Fatal("as_files: картинка должна уйти документом")
	}
}

func TestAudioSnapshotAndForceDocument(t *testing.T) {
	old := config.Root
	config.Root = t.TempDir()
	t.Cleanup(func() { config.Root = old })
	s := &config.Settings{MaxFileMB: 50}
	var paths []string
	for _, name := range []string{"chapter.MP3", "chapter.m4a", "chapter.m4b", "report.txt"} {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte("synthetic attachment"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	for _, tc := range []struct {
		id      string
		asFiles bool
		network string
		audio   bool
	}{{"audio", false, "", true}, {"forced", true, "", false}, {"wa", false, config.NetWA, false}, {"max", false, config.NetMAX, false}} {
		s.Network = tc.network
		got, err := Snapshot(s, tc.id, paths, tc.asFiles)
		if err != nil {
			t.Fatal(err)
		}
		for i, snap := range got {
			want := ""
			if tc.audio && i < 3 {
				want = outbox.KindAudio
			}
			if snap.Kind != want || snap.Size != int64(len("synthetic attachment")) {
				t.Fatalf("%s %s: kind=%q, size=%d", tc.id, snap.Name, snap.Kind, snap.Size)
			}
		}
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

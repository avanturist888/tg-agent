package tgc

import (
	"os"
	"testing"
)

func TestPackWaveformRoundTrip(t *testing.T) {
	v := make([]int, 100)
	for i := range v {
		v[i] = (i * 7) % 32
	}
	p := packWaveform(v)
	if len(p) != 63 {
		t.Fatalf("len = %d, want 63", len(p))
	}
	for i, want := range v {
		bit := i * 5
		w := uint16(p[bit/8])
		if bit/8+1 < len(p) {
			w |= uint16(p[bit/8+1]) << 8
		}
		if got := int(w>>(bit%8)) & 31; got != want {
			t.Fatalf("v[%d] = %d, want %d", i, got, want)
		}
	}
}

func TestLevelsRange(t *testing.T) {
	sizes := []int{3, 3, 3, 3, 80, 90, 100, 3, 3, 60}
	l := levels(sizes, 5)
	if l[0] != 0 || l[2] == 0 || len(l) != 5 {
		t.Fatalf("levels = %v", l)
	}
	for _, x := range l {
		if x < 0 || x > 31 {
			t.Fatalf("levels = %v", l)
		}
	}
}

// TG_TEST_OGG=путь\к\голосовому.ogg go test -run OggVoice -v ./internal/tgc
func TestOggVoiceFile(t *testing.T) {
	path := os.Getenv("TG_TEST_OGG")
	if path == "" {
		t.Skip("TG_TEST_OGG не задан")
	}
	d, w := oggVoice(path)
	t.Logf("duration %d s, waveform %d bytes", d, len(w))
	if d == 0 || len(w) != 63 {
		t.Fatal("нет длительности или waveform")
	}
}

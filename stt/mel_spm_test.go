package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func makeNPY(t *testing.T, major byte, descr string, fortran bool, shape []int, vals []float32, bigEndian bool) []byte {
	t.Helper()
	var shapeStr string
	if len(shape) == 1 {
		shapeStr = fmt.Sprintf("(%d,)", shape[0])
	} else {
		shapeStr = fmt.Sprintf("(%d, %d)", shape[0], shape[1])
	}
	fortStr := "False"
	if fortran {
		fortStr = "True"
	}
	hdr := fmt.Sprintf("{'descr': '%s', 'fortran_order': %s, 'shape': %s, }\n", descr, fortStr, shapeStr)
	var buf bytes.Buffer
	buf.WriteString("\x93NUMPY")
	buf.WriteByte(major)
	buf.WriteByte(0)
	if major == 1 {
		_ = binary.Write(&buf, binary.LittleEndian, uint16(len(hdr)))
	} else {
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(hdr)))
	}
	buf.WriteString(hdr)
	var order binary.ByteOrder = binary.LittleEndian
	if bigEndian {
		order = binary.BigEndian
	}
	for _, v := range vals {
		_ = binary.Write(&buf, order, math.Float32bits(v))
	}
	return buf.Bytes()
}

func TestParseNPYFloat32(t *testing.T) {
	t.Run("1D_v1", func(t *testing.T) {
		want := []float32{1.5, -2.25, 0, 3.75}
		raw := makeNPY(t, 1, "<f4", false, []int{4}, want, false)
		got, shape, err := parseNPYFloat32(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parseNPYFloat32: %v", err)
		}
		if len(shape) != 1 || shape[0] != 4 {
			t.Fatalf("shape=%v, want [4]", shape)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d]=%v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("2D_COrder_v1", func(t *testing.T) {
		want := []float32{1, 2, 3, 4, 5, 6} // (2, 3)
		raw := makeNPY(t, 1, "<f4", false, []int{2, 3}, want, false)
		got, shape, err := parseNPYFloat32(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parseNPYFloat32: %v", err)
		}
		if len(shape) != 2 || shape[0] != 2 || shape[1] != 3 {
			t.Fatalf("shape=%v, want [2 3]", shape)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d]=%v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("2D_FortranOrder_v2", func(t *testing.T) {
		// Матрица 2x3:
		// [[1, 2, 3],
		//  [4, 5, 6]]
		// В Fortran-order (column-major) хранится как [1, 4, 2, 5, 3, 6].
		colMajor := []float32{1, 4, 2, 5, 3, 6}
		wantRowMajor := []float32{1, 2, 3, 4, 5, 6}
		raw := makeNPY(t, 2, "<f4", true, []int{2, 3}, colMajor, false)
		got, shape, err := parseNPYFloat32(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parseNPYFloat32: %v", err)
		}
		if len(shape) != 2 || shape[0] != 2 || shape[1] != 3 {
			t.Fatalf("shape=%v, want [2 3]", shape)
		}
		for i := range wantRowMajor {
			if got[i] != wantRowMajor[i] {
				t.Fatalf("got[%d]=%v, want %v", i, got[i], wantRowMajor[i])
			}
		}
	})

	t.Run("BigEndian_f4", func(t *testing.T) {
		want := []float32{0.5, -1.25}
		raw := makeNPY(t, 1, ">f4", false, []int{2}, want, true)
		got, _, err := parseNPYFloat32(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parseNPYFloat32: %v", err)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d]=%v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("Errors", func(t *testing.T) {
		if _, _, err := parseNPYFloat32(bytes.NewReader([]byte("BADMAGIC1234"))); err == nil {
			t.Fatal("expected error for bad magic")
		}
		badDtype := makeNPY(t, 1, "<f8", false, []int{1}, []float32{1}, false)
		if _, _, err := parseNPYFloat32(bytes.NewReader(badDtype)); err == nil {
			t.Fatal("expected error for unsupported dtype")
		}
	})
}

func TestLoadMelNPZSynthetic(t *testing.T) {
	dir := t.TempDir()
	npzPath := filepath.Join(dir, "mel.npz")

	win := make([]float32, melWinLen)
	for i := range win {
		win[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(melWinLen)))
	}
	fb := make([]float32, melFreqBins*melBins)
	for k := 0; k < melFreqBins; k++ {
		fb[k*melBins+(k%melBins)] = 1.0
	}

	f, err := os.Create(npzPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, payload := range map[string][]byte{
		"window.npy":     makeNPY(t, 1, "<f4", false, []int{melWinLen}, win, false),
		"filterbank.npy": makeNPY(t, 2, "<f4", false, []int{melFreqBins, melBins}, fb, false),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	m, err := loadMelNPZ(npzPath)
	if err != nil {
		t.Fatalf("loadMelNPZ: %v", err)
	}
	if math.Abs(m.window[160]-1.0) > 1e-6 {
		t.Fatalf("window[160]=%v, want 1.0", m.window[160])
	}
	if m.filterbank[65][1] != 1.0 || m.filterbank[65][0] != 0.0 {
		t.Fatalf("unexpected filterbank values at k=65")
	}
}

func naiveRFFT320Power(in *[melWinLen]float64) [melFreqBins]float64 {
	var power [melFreqBins]float64
	for k := 0; k < melFreqBins; k++ {
		var re, im float64
		for n := 0; n < melWinLen; n++ {
			ang := -2 * math.Pi * float64(k*n) / melNFFT
			s, c := math.Sincos(ang)
			re += in[n] * c
			im += in[n] * s
		}
		power[k] = re*re + im*im
	}
	return power
}

func TestRFFT320MatchesNaiveDFT(t *testing.T) {
	var in [melWinLen]float64
	for n := 0; n < melWinLen; n++ {
		// Комбинация нескольких частот + апериодическая составляющая.
		in[n] = 0.7*math.Sin(2*math.Pi*440*float64(n)/sampleRate) +
			0.3*math.Cos(2*math.Pi*1234.5*float64(n)/sampleRate) +
			0.15*float64(n)/melWinLen - 0.05
	}
	want := naiveRFFT320Power(&in)
	var got [melFreqBins]float64
	rfft320Power(&in, &got)

	for k := 0; k < melFreqBins; k++ {
		diff := math.Abs(got[k] - want[k])
		tol := 1e-9 * math.Max(1, want[k])
		if diff > tol {
			t.Fatalf("k=%d: got=%v, want=%v, diff=%v > tol=%v", k, got[k], want[k], diff, tol)
		}
	}
}

func TestMelExtractLayoutAndEdgeCases(t *testing.T) {
	m := &melExtractor{}
	for i := 0; i < melWinLen; i++ {
		m.window[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(melWinLen))
	}
	// Простая матрица: бин b суммирует пару соседних частотных отсчётов.
	for b := 0; b < melBins; b++ {
		m.filterbank[2*b][b] = 0.5
		m.filterbank[2*b+1][b] = 1.5
	}

	// 1) Короткое аудио (< 320) должно дополняться нулями до 320 → 1 кадр.
	shortAudio := []float32{0.1, -0.2, 0.3}
	feats, nFrames := m.extract(shortAudio)
	if nFrames != 1 || len(feats) != melBins {
		t.Fatalf("short audio: nFrames=%d, len=%d, want 1 and %d", nFrames, len(feats), melBins)
	}

	// 2) Тишина → зажим в 1e-9 → log(1e-9).
	silent := make([]float32, 480) // 2 кадра
	feats, nFrames = m.extract(silent)
	if nFrames != 2 || len(feats) != 2*melBins {
		t.Fatalf("silent: nFrames=%d, len=%d, want 2 and %d", nFrames, len(feats), 2*melBins)
	}
	wantLogMin := float32(math.Log(1e-9))
	for i, v := range feats {
		if math.Abs(float64(v-wantLogMin)) > 1e-5 {
			t.Fatalf("silent[%d]=%v, want %v", i, v, wantLogMin)
		}
	}

	// 3) Проверка точного расчёта и индексации m*numFrames + t на 3 кадрах (640 отсчётов).
	audio := make([]float32, 640)
	for i := range audio {
		audio[i] = float32(0.5 * math.Sin(2*math.Pi*1000*float64(i)/sampleRate+0.3))
	}
	feats, nFrames = m.extract(audio)
	if nFrames != 3 {
		t.Fatalf("nFrames=%d, want 3", nFrames)
	}
	for frameIdx := 0; frameIdx < nFrames; frameIdx++ {
		var windowed [melWinLen]float64
		for i := 0; i < melWinLen; i++ {
			windowed[i] = float64(audio[frameIdx*melHopLen+i]) * m.window[i]
		}
		pwr := naiveRFFT320Power(&windowed)
		for b := 0; b < melBins; b++ {
			val := pwr[2*b]*0.5 + pwr[2*b+1]*1.5
			if val < 1e-9 {
				val = 1e-9
			} else if val > 1e9 {
				val = 1e9
			}
			want := float32(math.Log(val))
			got := feats[b*nFrames+frameIdx]
			if math.Abs(float64(got-want)) > 1e-5 {
				t.Fatalf("mel[b=%d, t=%d]: got=%v, want=%v", b, frameIdx, got, want)
			}
		}
	}
}

func appendVarint(dst []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(dst, tmp[:n]...)
}

func appendBytesField(dst []byte, fieldNum uint64, val []byte) []byte {
	dst = appendVarint(dst, (fieldNum<<3)|2)
	dst = appendVarint(dst, uint64(len(val)))
	return append(dst, val...)
}

func makeSPModelProto(pieces []spPiece) []byte {
	var out []byte
	for i, p := range pieces {
		var entry []byte
		entry = appendBytesField(entry, 1, []byte(p.piece))
		// Поле 2: score (float32, wire type 5).
		entry = appendVarint(entry, (2<<3)|5)
		var sc [4]byte
		binary.LittleEndian.PutUint32(sc[:], math.Float32bits(-float32(i)))
		entry = append(entry, sc[:]...)
		// Для проверки default=NORMAL опускаем поле 3 у NORMAL на чётных индексах.
		if p.typ != spNormal || i%2 == 1 {
			entry = appendVarint(entry, (3<<3)|0)
			entry = appendVarint(entry, uint64(p.typ))
		}
		out = appendBytesField(out, 1, entry)
	}
	// Добавляем посторонние поля разных wire-типов (как trainer_spec / normalizer_spec).
	out = appendBytesField(out, 2, []byte{0x08, 0x01})
	out = appendVarint(out, (10<<3)|0)
	out = appendVarint(out, 42)
	out = appendVarint(out, (11<<3)|1)
	out = append(out, 1, 2, 3, 4, 5, 6, 7, 8)
	return out
}

func TestSentencePieceDecoder(t *testing.T) {
	raw := makeSPModelProto([]spPiece{
		{piece: "<unk>", typ: spUnknown},
		{piece: "<s>", typ: spControl},
		{piece: "</s>", typ: spControl},
		{piece: "<unused>", typ: spUnused},
		{piece: "\u2581Привет", typ: spNormal},
		{piece: ",", typ: spNormal},
		{piece: "\u2581", typ: spNormal},
		{piece: "<0xD0>", typ: spByte}, // UTF-8 'м' = 0xD0 0xBC
		{piece: "<0xBC>", typ: spByte},
		{piece: "ир", typ: spNormal},
		{piece: "!", typ: spUserDefined},
	})
	dec, err := parseSentencePiece(raw)
	if err != nil {
		t.Fatalf("parseSentencePiece: %v", err)
	}
	if len(dec.pieces) != 11 {
		t.Fatalf("len(pieces)=%d, want 11", len(dec.pieces))
	}
	got := dec.decode([]int64{-1, 1, 4, 5, 3, 6, 7, 8, 9, 10, 2, 999})
	want := "Привет, мир!"
	if got != want {
		t.Fatalf("decode=%q, want %q", got, want)
	}
}

func TestRealV4RNNTAssetsIfPresent(t *testing.T) {
	dir := filepath.Join("models", asrDirV4RNNT)
	melPath := filepath.Join(dir, rnntMelFile)
	tokPath := filepath.Join(dir, rnntTokenizerFile)
	if _, err := os.Stat(melPath); err != nil {
		t.Skip("v4_rnnt_mel.npz не скачан")
	}
	if _, err := os.Stat(tokPath); err != nil {
		t.Skip("v4_rnnt_e2e_tokenizer.model не скачан")
	}

	m, err := loadMelNPZ(melPath)
	if err != nil {
		t.Fatalf("loadMelNPZ(real): %v", err)
	}
	// Проверяем окно и filterbank на конечные значения и ненулевую энергию.
	var winSum, fbSum float64
	for _, w := range m.window {
		if math.IsNaN(w) || math.IsInf(w, 0) {
			t.Fatal("real window contains NaN/Inf")
		}
		winSum += w
	}
	for k := 0; k < melFreqBins; k++ {
		for b := 0; b < melBins; b++ {
			v := m.filterbank[k][b]
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatal("real filterbank contains NaN/Inf")
			}
			fbSum += v
		}
	}
	if winSum <= 0 || fbSum <= 0 {
		t.Fatalf("real mel assets look empty: winSum=%v fbSum=%v", winSum, fbSum)
	}

	dec, err := loadSentencePiece(tokPath)
	if err != nil {
		t.Fatalf("loadSentencePiece(real): %v", err)
	}
	if len(dec.pieces) != 1024 {
		t.Fatalf("real tokenizer vocab=%d, want 1024", len(dec.pieces))
	}
	// Ищем простой нормальный токен и проверяем декодирование без паники.
	var sampleIDs []int64
	for id, p := range dec.pieces {
		if p.typ == spNormal && len(p.piece) > 0 {
			sampleIDs = append(sampleIDs, int64(id))
			if len(sampleIDs) == 5 {
				break
			}
		}
	}
	if len(sampleIDs) == 0 {
		t.Fatal("real tokenizer has no NORMAL pieces")
	}
	if out := dec.decode(sampleIDs); out == "" {
		t.Fatalf("real tokenizer decode(%v) returned empty string", sampleIDs)
	}
}

package main

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

const (
	melWinLen   = 320
	melHopLen   = 160
	melNFFT     = 320
	melFreqBins = melNFFT/2 + 1 // 161
	melBins     = 64
	melMinVal   = 1e-9
	melMaxVal   = 1e9
)

// melExtractor — вычисляет log-mel фичи GigaAM-v4 RNN-T:
// sampleRate=16000, winLength=320, hopLength=160, nFFT=320, nMel=64.
type melExtractor struct {
	window     [melWinLen]float64
	filterbank [melFreqBins][melBins]float64
}

// Предвычисленные таблицы для 320-точечного rfft (разложение 320 = 5 * 64).
var (
	fftRev64 [64]int
	fftTw64  [32]complex128
	fftTw320 [5][melFreqBins]complex128
)

func init() {
	for i := 0; i < 64; i++ {
		fftRev64[i] = int(bits.Reverse8(uint8(i)) >> 2)
	}
	for m := 0; m < 32; m++ {
		ang := -2 * math.Pi * float64(m) / 64
		s, c := math.Sincos(ang)
		fftTw64[m] = complex(c, s)
	}
	for r := 0; r < 5; r++ {
		for k := 0; k < melFreqBins; k++ {
			ang := -2 * math.Pi * float64(r*k) / melNFFT
			s, c := math.Sincos(ang)
			fftTw320[r][k] = complex(c, s)
		}
	}
}

// loadMelNPZ читает window.npy (320,) и filterbank.npy (161, 64) из v4_rnnt_mel.npz.
func loadMelNPZ(path string) (*melExtractor, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("открыть %s: %w", path, err)
	}
	defer zr.Close()

	var winData, fbData []float32
	var winShape, fbShape []int
	for _, f := range zr.File {
		switch f.Name {
		case "window.npy", "filterbank.npy":
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("прочитать %s из %s: %w", f.Name, path, err)
			}
			data, shape, perr := parseNPYFloat32(rc)
			_ = rc.Close()
			if perr != nil {
				return nil, fmt.Errorf("разобрать %s из %s: %w", f.Name, path, perr)
			}
			if f.Name == "window.npy" {
				winData, winShape = data, shape
			} else {
				fbData, fbShape = data, shape
			}
		}
	}
	if winData == nil {
		return nil, fmt.Errorf("в %s нет window.npy", path)
	}
	if fbData == nil {
		return nil, fmt.Errorf("в %s нет filterbank.npy", path)
	}
	if len(winShape) != 1 || winShape[0] != melWinLen {
		return nil, fmt.Errorf("window.npy: ожидалась форма (%d,), получено %v", melWinLen, winShape)
	}
	if len(fbShape) != 2 || fbShape[0] != melFreqBins || fbShape[1] != melBins {
		return nil, fmt.Errorf("filterbank.npy: ожидалась форма (%d, %d), получено %v", melFreqBins, melBins, fbShape)
	}

	m := &melExtractor{}
	for i := 0; i < melWinLen; i++ {
		m.window[i] = float64(winData[i])
	}
	for k := 0; k < melFreqBins; k++ {
		for b := 0; b < melBins; b++ {
			m.filterbank[k][b] = float64(fbData[k*melBins+b])
		}
	}
	return m, nil
}

// parseNPYFloat32 разбирает поток формата NumPy .npy (v1.0/v2.0/v3.0)
// с массивом float32 (1D или 2D, C-order или Fortran-order) и возвращает
// данные в C-order (row-major) и форму массива.
func parseNPYFloat32(r io.Reader) ([]float32, []int, error) {
	var prefix [8]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, nil, fmt.Errorf("заголовок npy: %w", err)
	}
	if string(prefix[:6]) != "\x93NUMPY" {
		return nil, nil, errors.New("неверный magic npy")
	}
	major, minor := prefix[6], prefix[7]
	var hlen uint32
	switch major {
	case 1:
		var l uint16
		if err := binary.Read(r, binary.LittleEndian, &l); err != nil {
			return nil, nil, fmt.Errorf("длина заголовка npy v1.%d: %w", minor, err)
		}
		hlen = uint32(l)
	case 2, 3:
		if err := binary.Read(r, binary.LittleEndian, &hlen); err != nil {
			return nil, nil, fmt.Errorf("длина заголовка npy v%d.%d: %w", major, minor, err)
		}
	default:
		return nil, nil, fmt.Errorf("неподдерживаемая версия npy %d.%d", major, minor)
	}
	if hlen == 0 || hlen > 1<<20 {
		return nil, nil, fmt.Errorf("некорректная длина заголовка npy: %d", hlen)
	}
	hdrBytes := make([]byte, hlen)
	if _, err := io.ReadFull(r, hdrBytes); err != nil {
		return nil, nil, fmt.Errorf("чтение заголовка npy: %w", err)
	}
	descr, fortran, shape, err := parseNPYHeader(string(hdrBytes))
	if err != nil {
		return nil, nil, err
	}
	var order binary.ByteOrder
	switch descr {
	case "<f4", "f4", "=f4":
		order = binary.LittleEndian
	case ">f4":
		order = binary.BigEndian
	default:
		return nil, nil, fmt.Errorf("ожидался float32 (<f4), получено %q", descr)
	}
	if len(shape) == 0 || len(shape) > 2 {
		return nil, nil, fmt.Errorf("поддерживаются только 1D и 2D массивы, получено %v", shape)
	}
	const maxNPYElements = 1 << 20 // защита от OOM/переполнения на повреждённом npy (mel-матрицы < 20К элементов)
	total := 1
	for _, d := range shape {
		if d < 0 {
			return nil, nil, fmt.Errorf("отрицательная размерность npy: %v", shape)
		}
		if d > 0 && total > maxNPYElements/d {
			return nil, nil, fmt.Errorf("слишком большой массив npy: %v", shape)
		}
		total *= d
	}
	raw := make([]byte, total*4)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, nil, fmt.Errorf("данные npy: %w", err)
	}
	// Дочитываем до EOF, чтобы archive/zip проверил CRC32 всей записи.
	var tail [1]byte
	if n, err := r.Read(tail[:]); err != io.EOF || n != 0 {
		if err != nil && err != io.EOF {
			return nil, nil, fmt.Errorf("проверка конца npy: %w", err)
		}
		return nil, nil, errors.New("лишние байты после данных npy")
	}
	vals := make([]float32, total)
	for i := range vals {
		vals[i] = math.Float32frombits(order.Uint32(raw[4*i:]))
	}
	if fortran && len(shape) == 2 {
		rows, cols := shape[0], shape[1]
		transposed := make([]float32, total)
		for c := 0; c < cols; c++ {
			for row := 0; row < rows; row++ {
				transposed[row*cols+c] = vals[c*rows+row]
			}
		}
		vals = transposed
	}
	return vals, shape, nil
}

// parseNPYHeader разбирает Python-словарь заголовка .npy:
// {'descr': '<f4', 'fortran_order': False, 'shape': (161, 64), }
func parseNPYHeader(hdr string) (descr string, fortran bool, shape []int, err error) {
	s := strings.TrimSpace(hdr)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return "", false, nil, fmt.Errorf("некорректный словарь заголовка npy: %q", s)
	}
	body := s[1 : len(s)-1]

	// Ищем descr.
	descrVal, ok := extractDictField(body, "descr")
	if !ok {
		return "", false, nil, errors.New("в заголовке npy нет descr")
	}
	descr = strings.Trim(strings.TrimSpace(descrVal), "'\"")

	// Ищем fortran_order.
	fortVal, ok := extractDictField(body, "fortran_order")
	if !ok {
		return "", false, nil, errors.New("в заголовке npy нет fortran_order")
	}
	switch strings.TrimSpace(fortVal) {
	case "True":
		fortran = true
	case "False":
		fortran = false
	default:
		return "", false, nil, fmt.Errorf("некорректный fortran_order: %q", fortVal)
	}

	// Ищем shape.
	shapeVal, ok := extractDictField(body, "shape")
	if !ok {
		return "", false, nil, errors.New("в заголовке npy нет shape")
	}
	shapeVal = strings.TrimSpace(shapeVal)
	if !strings.HasPrefix(shapeVal, "(") || !strings.HasSuffix(shapeVal, ")") {
		return "", false, nil, fmt.Errorf("некорректный shape: %q", shapeVal)
	}
	inner := strings.TrimSpace(shapeVal[1 : len(shapeVal)-1])
	if inner != "" {
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			d, perr := strconv.Atoi(part)
			if perr != nil {
				return "", false, nil, fmt.Errorf("некорректная размерность shape %q: %w", part, perr)
			}
			shape = append(shape, d)
		}
	}
	return descr, fortran, shape, nil
}

// extractDictField вытаскивает значение ключа из плоского Python-словаря заголовка npy.
func extractDictField(body, key string) (string, bool) {
	for _, q := range []string{"'", "\""} {
		pat := q + key + q
		idx := strings.Index(body, pat)
		if idx < 0 {
			continue
		}
		rest := body[idx+len(pat):]
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			continue
		}
		rest = strings.TrimLeft(rest[colon+1:], " \t")
		if len(rest) == 0 {
			return "", false
		}
		switch rest[0] {
		case '(':
			end := strings.IndexByte(rest, ')')
			if end < 0 {
				return "", false
			}
			return rest[:end+1], true
		case '\'', '"':
			quote := rest[0]
			end := strings.IndexByte(rest[1:], quote)
			if end < 0 {
				return "", false
			}
			return rest[:end+2], true
		default:
			end := strings.IndexAny(rest, ", \t\r\n")
			if end < 0 {
				return rest, true
			}
			return rest[:end], true
		}
	}
	return "", false
}

// fft64 выполняет 64-точечное БПФ на месте (radix-2 DIT).
func fft64(a *[64]complex128) {
	for size := 2; size <= 64; size <<= 1 {
		half := size >> 1
		step := 64 / size
		for base := 0; base < 64; base += size {
			for j := 0; j < half; j++ {
				w := fftTw64[j*step]
				u := a[base+j]
				v := w * a[base+j+half]
				a[base+j] = u + v
				a[base+j+half] = u - v
			}
		}
	}
}

// rfft320Power вычисляет спектр мощности |rfft(in)|^2 (161 отсчёт) для 320 реальных точек.
func rfft320Power(in *[melWinLen]float64, power *[melFreqBins]float64) {
	var sub [5][64]complex128
	for r := 0; r < 5; r++ {
		for j := 0; j < 64; j++ {
			sub[r][fftRev64[j]] = complex(in[5*j+r], 0)
		}
		fft64(&sub[r])
	}
	for k := 0; k < melFreqBins; k++ {
		m := k & 63
		sum := sub[0][m] +
			sub[1][m]*fftTw320[1][k] +
			sub[2][m]*fftTw320[2][k] +
			sub[3][m]*fftTw320[3][k] +
			sub[4][m]*fftTw320[4][k]
		re, im := real(sum), imag(sum)
		power[k] = re*re + im*im
	}
}

// extract вычисляет log-mel фичи в плоском массиве формы [1, 64, numFrames]
// (индекс m*numFrames + t). Короткое аудио (< 320 отсчётов) дополняется нулями.
func (m *melExtractor) extract(audio []float32) ([]float32, int) {
	if len(audio) < melWinLen {
		padded := make([]float32, melWinLen)
		copy(padded, audio)
		audio = padded
	}
	numFrames := (len(audio)-melWinLen)/melHopLen + 1
	out := make([]float32, melBins*numFrames)

	var frame [melWinLen]float64
	var power [melFreqBins]float64
	var melFrame [melBins]float64

	for t := 0; t < numFrames; t++ {
		off := t * melHopLen
		for i := 0; i < melWinLen; i++ {
			frame[i] = float64(audio[off+i]) * m.window[i]
		}
		rfft320Power(&frame, &power)
		melFrame = [melBins]float64{}
		for k := 0; k < melFreqBins; k++ {
			pk := power[k]
			if pk == 0 {
				continue
			}
			row := &m.filterbank[k]
			for b := 0; b < melBins; b++ {
				melFrame[b] += pk * row[b]
			}
		}
		for b := 0; b < melBins; b++ {
			v := melFrame[b]
			if v < melMinVal {
				v = melMinVal
			} else if v > melMaxVal {
				v = melMaxVal
			}
			out[b*numFrames+t] = float32(math.Log(v))
		}
	}
	return out, numFrames
}

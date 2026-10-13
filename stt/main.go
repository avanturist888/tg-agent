// tgstt — расшифровка голосовых для tg-agent без Python: GigaAM v3 (с
// пунктуацией) через sherpa-onnx, на процессоре.
//
// Служба tg-agent держит этот процесс и шлёт ему запросы строками JSON в
// stdin, ответ — строкой JSON в stdout (логи — в stderr):
//
//	{"id": 1, "path": "C:/.../voice.oga", "language": "auto"}
//	→ {"id": 1, "ok": true, "language": "ru", "engine": "gigaam",
//	   "model": "v3_e2e_rnnt", "device": "cpu", "text": "…", "speech": true,
//	   "duration": 12.3, "seconds": 0.8}
//
// language: "auto" — сначала определить язык (whisper base, только детектор):
// не русская речь — engine = null, текста нет, её расшифрует Telegram;
// "ru" — сразу GigaAM. {"id": 0, "ping": true} — загрузить модели.
// Длинные записи режутся по паузам (Silero VAD) на куски до 22 с.
// Аудио любого формата читает ffmpeg. Процесс выходит, когда закрыт stdin.
//
// Модели — в stt/models (их кладёт scripts\install-stt.ps1); каталог можно
// задать TG_STT_MODELS. Собирается отдельно (cgo): scripts\build.ps1.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	sampleRate = 16000
	modelName  = "v3_e2e_rnnt"
	maxChunk   = 22 * sampleRate // GigaAM берёт до 25 с за раз
	pad        = sampleRate / 5  // 0,2 с вокруг речи
	lidMax     = 29 * sampleRate // детектору языка — до 29 с речи (whisper берёт меньше 30)
)

// Модели внутри каталога моделей (как их раскладывает install-stt.ps1).
const (
	asrDir = "sherpa-onnx-nemo-transducer-punct-giga-am-v3-russian-2025-12-16"
	lidDir = "sherpa-onnx-whisper-base"
	vadOnx = "silero_vad.onnx"
)

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[stt] "+format+"\n", a...)
}

func modelsDir() string {
	if d := os.Getenv("TG_STT_MODELS"); d != "" {
		return d
	}
	exe, _ := os.Executable()
	// bin\tgstt.exe → <установка>\stt\models
	return filepath.Join(filepath.Dir(filepath.Dir(exe)), "stt", "models")
}

func threads() int {
	return max(2, min(8, runtime.NumCPU()/2))
}

type engine struct {
	dir string
	asr *sherpa.OfflineRecognizer
	lid *sherpa.SpokenLanguageIdentification
}

func need(paths ...string) error {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("нет модели %s — поставь scripts\\install-stt.ps1", p)
		}
	}
	return nil
}

func (e *engine) load() error {
	if e.asr != nil {
		return nil
	}
	a := filepath.Join(e.dir, asrDir)
	l := filepath.Join(e.dir, lidDir)
	if err := need(filepath.Join(a, "encoder.int8.onnx"), filepath.Join(a, "tokens.txt"),
		filepath.Join(l, "base-encoder.int8.onnx"), filepath.Join(e.dir, vadOnx)); err != nil {
		return err
	}
	t := time.Now()
	cfg := sherpa.OfflineRecognizerConfig{}
	cfg.FeatConfig = sherpa.FeatureConfig{SampleRate: sampleRate, FeatureDim: 80} // как у sherpa-onnx-offline; размерность NeMo-модели берёт из её метаданных
	cfg.ModelConfig.Transducer = sherpa.OfflineTransducerModelConfig{
		Encoder: filepath.Join(a, "encoder.int8.onnx"),
		Decoder: filepath.Join(a, "decoder.onnx"),
		Joiner:  filepath.Join(a, "joiner.onnx"),
	}
	cfg.ModelConfig.Tokens = filepath.Join(a, "tokens.txt")
	cfg.ModelConfig.ModelType = "nemo_transducer"
	cfg.ModelConfig.NumThreads = threads()
	cfg.ModelConfig.Provider = "cpu"
	cfg.DecodingMethod = "greedy_search"
	e.asr = sherpa.NewOfflineRecognizer(&cfg)
	if e.asr == nil {
		return errors.New("GigaAM не загрузился (подробности выше в логе)")
	}
	logf("GigaAM %s на cpu (%d потоков): %.1f с", modelName, threads(), time.Since(t).Seconds())
	t = time.Now()
	e.lid = sherpa.NewSpokenLanguageIdentification(&sherpa.SpokenLanguageIdentificationConfig{
		Whisper: sherpa.SpokenLanguageIdentificationWhisperConfig{
			Encoder: filepath.Join(l, "base-encoder.int8.onnx"),
			Decoder: filepath.Join(l, "base-decoder.int8.onnx"),
		},
		NumThreads: threads(),
		Provider:   "cpu",
	})
	if e.lid == nil {
		return errors.New("детектор языка не загрузился")
	}
	logf("детектор языка whisper base: %.1f с", time.Since(t).Seconds())
	return nil
}

// span — кусок записи [start, end) в отсчётах.
type span struct{ start, end int }

// speech — где в записи речь (Silero VAD).
func (e *engine) speech(wav []float32) []span {
	cfg := sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model: filepath.Join(e.dir, vadOnx), Threshold: 0.5,
			MinSilenceDuration: 0.3, MinSpeechDuration: 0.25, WindowSize: 512, MaxSpeechDuration: 20,
		},
		SampleRate: sampleRate, NumThreads: 1, Provider: "cpu",
	}
	vad := sherpa.NewVoiceActivityDetector(&cfg, float32(len(wav))/sampleRate+5)
	defer sherpa.DeleteVoiceActivityDetector(vad)
	var out []span
	collect := func() {
		for !vad.IsEmpty() {
			s := vad.Front()
			out = append(out, span{s.Start, s.Start + len(s.Samples)})
			vad.Pop()
		}
	}
	const window = 512
	for i := 0; i+window <= len(wav); i += window {
		vad.AcceptWaveform(wav[i : i+window])
		collect()
	}
	vad.Flush()
	collect()
	return out
}

// chunks — куски речи до maxChunk, границы — по паузам.
func chunks(n int, spans []span) []span {
	var out []span
	var cur *span
	for _, s := range spans {
		start, end := max(0, s.start-pad), min(n, s.end+pad)
		for end-start > maxChunk { // одна фраза длиннее куска — режем по живому
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
			out = append(out, span{start, start + maxChunk})
			start += maxChunk
		}
		if cur != nil && end-cur.start <= maxChunk {
			cur.end = end
			continue
		}
		if cur != nil {
			out = append(out, *cur)
		}
		cur = &span{start, end}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func (e *engine) language(wav []float32, spans []span) string {
	var buf []float32
	for _, s := range spans {
		buf = append(buf, wav[s.start:s.end]...)
		if len(buf) >= lidMax {
			break
		}
	}
	if len(buf) > lidMax {
		buf = buf[:lidMax]
	}
	st := e.lid.CreateStream()
	defer sherpa.DeleteOfflineStream(st)
	st.AcceptWaveform(sampleRate, buf)
	return e.lid.Compute(st).Lang
}

// transcribe — куски по одному: все разом одним DecodeStreams на голосовом
// в 39 минут съели 3,9 ГБ памяти, по одному — 1 ГБ при той же скорости.
func (e *engine) transcribe(wav []float32, spans []span) string {
	var texts []string
	for _, p := range chunks(len(wav), spans) {
		st := sherpa.NewOfflineStream(e.asr)
		st.AcceptWaveform(sampleRate, wav[p.start:p.end])
		e.asr.Decode(st)
		if t := strings.TrimSpace(st.GetResult().Text); t != "" {
			texts = append(texts, t)
		}
		sherpa.DeleteOfflineStream(st)
	}
	return strings.Join(texts, " ")
}

// loadAudio — файл любого формата в 16 кГц моно через ffmpeg.
func loadAudio(path string) ([]float32, error) {
	ff := os.Getenv("TG_FFMPEG")
	if ff == "" {
		ff = "ffmpeg"
	}
	cmd := exec.Command(ff, "-nostdin", "-loglevel", "error", "-i", path,
		"-f", "s16le", "-ac", "1", "-acodec", "pcm_s16le", "-ar", fmt.Sprint(sampleRate), "-")
	hideWindow(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg не прочитал запись: %v %s", err, strings.TrimSpace(errb.String()))
	}
	raw := out.Bytes()
	wav := make([]float32, len(raw)/2)
	for i := range wav {
		wav[i] = float32(int16(binary.LittleEndian.Uint16(raw[2*i:]))) / 32768
	}
	return wav, nil
}

type request struct {
	ID       any    `json:"id"`
	Path     string `json:"path"`
	Language string `json:"language"`
	Ping     bool   `json:"ping"`
}

func (e *engine) handle(req request) (map[string]any, error) {
	if err := e.load(); err != nil {
		return nil, err
	}
	if req.Ping {
		return map[string]any{"ready": true, "device": "cpu", "model": modelName}, nil
	}
	t := time.Now()
	wav, err := loadAudio(req.Path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"duration": math.Round(float64(len(wav))/sampleRate*10) / 10, "device": "cpu"}
	spans := e.speech(wav)
	switch {
	case len(spans) == 0:
		out["speech"], out["language"], out["engine"], out["model"], out["text"] = false, nil, "gigaam", modelName, ""
	default:
		out["speech"] = true
		russian := true
		if req.Language == "" || req.Language == "auto" {
			lang := e.language(wav, spans)
			out["language"] = lang
			russian = lang == "ru"
		} else {
			out["language"] = "ru"
		}
		if russian {
			out["engine"], out["model"], out["text"] = "gigaam", modelName, e.transcribe(wav, spans)
		} else {
			out["engine"], out["text"] = nil, nil
		}
	}
	out["seconds"] = math.Round(time.Since(t).Seconds()*100) / 100
	return out, nil
}

func main() {
	e := &engine{dir: modelsDir()}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	reply := func(id any, res map[string]any, err error) {
		out := map[string]any{"id": id, "ok": err == nil}
		if err != nil {
			out["error"] = err.Error()
		}
		for k, v := range res {
			out[k] = v
		}
		_ = enc.Encode(out)
	}
	if len(os.Args) > 1 && os.Args[1] == "--check" { // установка: загрузить модели и показать, что всё живо
		res, err := e.handle(request{ID: 0, Ping: true})
		reply(0, res, err)
		if err != nil {
			os.Exit(1)
		}
		return
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<16)
	for {
		line, err := in.ReadBytes('\n')
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))) // PowerShell шлёт BOM
		if len(line) > 0 {
			var req request
			if jerr := json.Unmarshal(line, &req); jerr != nil {
				reply(nil, nil, fmt.Errorf("запрос не разобрался: %w", jerr))
			} else {
				res, herr := e.handle(req)
				reply(req.ID, res, herr)
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			logf("stdin: %v", err)
			return
		}
	}
}

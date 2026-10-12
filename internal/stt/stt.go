// Package stt — расшифровка голосовых на этой машине: GigaAM для русской
// речи через sherpa-onnx (bin\tgstt.exe, исходники — stt/; модели в
// stt\models кладёт scripts\install-stt.ps1). Без Python.
//
// Процесс tgstt с загруженными моделями живёт, пока есть работа: первый
// запрос его запускает (загрузка моделей — секунда-две), после простоя
// IdleTimeout он закрывается и отдаёт память. Запросы идут по одному:
// строка JSON в stdin, строка JSON в ответ.
package stt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"tgagent/internal/config"
)

// IdleTimeout — сколько держать модели в памяти без запросов.
const IdleTimeout = 15 * time.Minute

// Result — ответ расшифровщика.
type Result struct {
	// Language — язык речи (whisper), LanguageProb — уверенность
	Language     string  `json:"language"`
	LanguageProb float64 `json:"language_prob"`
	// Engine — "gigaam", если расшифровал; "" — речь не русская, текста нет
	Engine   string  `json:"engine"`
	Model    string  `json:"model"`
	Device   string  `json:"device"`
	Text     string  `json:"text"`
	Speech   bool    `json:"speech"` // false — речи не слышно
	Duration float64 `json:"duration"`
	Seconds  float64 `json:"seconds"`
}

// Done — расшифровано здесь (русская речь или тишина).
func (r Result) Done() bool { return r.Engine != "" }

// Label — чем расшифровано, для агента: «gigaam-v3_e2e_rnnt».
func (r Result) Label() string {
	if r.Model == "" {
		return r.Engine
	}
	return r.Engine + "-" + r.Model
}

// Exe — программа расшифровки ("" — не собрана).
func Exe() string {
	name := "tgstt"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(config.Root, "bin", name)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// ModelsDir — каталог моделей (scripts\install-stt.ps1).
func ModelsDir() string { return filepath.Join(config.Root, "stt", "models") }

// Enabled — расшифровка своими силами включена и установлена: есть tgstt и
// модели (TG_STT=off — выключить и оставить только Telegram).
func Enabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TG_STT")), "off") {
		return false
	}
	if _, err := os.Stat(filepath.Join(ModelsDir(), "silero_vad.onnx")); err != nil {
		return false
	}
	return Exe() != ""
}

type worker struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	seq  int
	idle *time.Timer
}

var w worker

func (w *worker) start() error {
	cmd := exec.Command(Exe())
	cmd.Env = append(os.Environ(), "TG_STT_MODELS="+ModelsDir())
	if ff, err := exec.LookPath("ffmpeg"); err == nil {
		cmd.Env = append(cmd.Env, "TG_FFMPEG="+ff)
	}
	hideWindow(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr // в службе — в data/service.log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("расшифровщик не запустился: %w", err)
	}
	w.cmd, w.in, w.out = cmd, in, bufio.NewReaderSize(out, 1<<20)
	return nil
}

// stop — закрыть процесс (под w.mu).
func (w *worker) stop() {
	if w.cmd == nil {
		return
	}
	_ = w.in.Close()
	done := make(chan struct{})
	go func(cmd *exec.Cmd) { _ = cmd.Wait(); close(done) }(w.cmd)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = w.cmd.Process.Kill()
	}
	w.cmd, w.in, w.out = nil, nil, nil
}

func (w *worker) call(ctx context.Context, req map[string]any) (Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.idle != nil {
		w.idle.Stop()
	}
	defer func() {
		w.idle = time.AfterFunc(IdleTimeout, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.stop()
		})
	}()
	if w.cmd == nil {
		if err := w.start(); err != nil {
			return Result{}, err
		}
	}
	w.seq++
	req["id"] = w.seq
	line, _ := json.Marshal(req)
	if _, err := w.in.Write(append(line, '\n')); err != nil {
		w.stop()
		return Result{}, fmt.Errorf("расшифровщик не принял запрос: %w", err)
	}
	type reply struct {
		raw []byte
		err error
	}
	got := make(chan reply, 1)
	out := w.out
	go func() {
		raw, err := out.ReadBytes('\n')
		got <- reply{raw, err}
	}()
	var r reply
	select {
	case r = <-got:
	case <-ctx.Done():
		w.stop() // ответ уже никому не нужен, а занятый процесс задержит следующих
		return Result{}, ctx.Err()
	}
	if r.err != nil {
		w.stop()
		return Result{}, fmt.Errorf("расшифровщик упал (подробности — в data/service.log): %w", r.err)
	}
	var res struct {
		Result
		ID    int    `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(r.raw, &res); err != nil {
		w.stop()
		return Result{}, fmt.Errorf("ответ расшифровщика не разобрался: %w", err)
	}
	if !res.OK {
		return Result{}, errors.New("расшифровщик: " + res.Error)
	}
	return res.Result, nil
}

// Transcribe — расшифровать файл. language: "auto" — русскую речь
// расшифровать, о другой только сказать язык (Done() == false); "ru" — без
// определения языка.
func Transcribe(ctx context.Context, path, language string) (Result, error) {
	if !Enabled() {
		return Result{}, errors.New("расшифровка GigaAM не установлена (scripts\\install-stt.ps1, scripts\\build.ps1)")
	}
	if language == "" {
		language = "auto"
	}
	return w.call(ctx, map[string]any{"path": path, "language": language})
}

// Ping — загрузить модели и проверить, что расшифровщик жив.
func Ping(ctx context.Context) (Result, error) {
	return w.call(ctx, map[string]any{"ping": true})
}

package main

import (
	"os"
	"path/filepath"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

func TestResolveEncoderIO(t *testing.T) {
	t.Run("StandardOrder", func(t *testing.T) {
		ins := []ort.InputOutputInfo{
			{Name: "features", DataType: ort.TensorElementDataTypeFloat},
			{Name: "sizes", DataType: ort.TensorElementDataTypeInt64},
		}
		outs := []ort.InputOutputInfo{
			{Name: "lengths", DataType: ort.TensorElementDataTypeInt64},
			{Name: "projected", DataType: ort.TensorElementDataTypeFloat},
		}
		inNames, outNames, err := resolveEncoderIO(ins, outs)
		if err != nil {
			t.Fatalf("resolveEncoderIO: %v", err)
		}
		if inNames[0] != "features" || inNames[1] != "sizes" {
			t.Fatalf("inNames=%v, want [features sizes]", inNames)
		}
		if outNames[0] != "projected" || outNames[1] != "lengths" {
			t.Fatalf("outNames=%v, want [projected lengths]", outNames)
		}
	})

	t.Run("ReversedInputsAndOutputs", func(t *testing.T) {
		ins := []ort.InputOutputInfo{
			{Name: "length", DataType: ort.TensorElementDataTypeInt64},
			{Name: "audio_signal", DataType: ort.TensorElementDataTypeFloat},
		}
		outs := []ort.InputOutputInfo{
			{Name: "projected", DataType: ort.TensorElementDataTypeFloat},
			{Name: "lengths", DataType: ort.TensorElementDataTypeInt64},
		}
		inNames, outNames, err := resolveEncoderIO(ins, outs)
		if err != nil {
			t.Fatalf("resolveEncoderIO: %v", err)
		}
		if inNames[0] != "audio_signal" || inNames[1] != "length" {
			t.Fatalf("inNames=%v, want [audio_signal length]", inNames)
		}
		if outNames[0] != "projected" || outNames[1] != "lengths" {
			t.Fatalf("outNames=%v, want [projected lengths]", outNames)
		}
	})
}

func TestResolveAdvanceIO(t *testing.T) {
	ins := []ort.InputOutputInfo{
		{Name: "lengths", DataType: ort.TensorElementDataTypeInt64},
		{Name: "projected", DataType: ort.TensorElementDataTypeFloat},
	}
	outs := []ort.InputOutputInfo{
		{Name: "tokens", DataType: ort.TensorElementDataTypeInt64},
	}
	inNames, outNames, err := resolveAdvanceIO(ins, outs)
	if err != nil {
		t.Fatalf("resolveAdvanceIO: %v", err)
	}
	if inNames[0] != "projected" || inNames[1] != "lengths" {
		t.Fatalf("inNames=%v, want [projected lengths]", inNames)
	}
	if len(outNames) != 1 || outNames[0] != "tokens" {
		t.Fatalf("outNames=%v, want [tokens]", outNames)
	}
}

func TestEngineSupports(t *testing.T) {
	cases := []struct {
		model string
		lang  string
		want  bool
	}{
		{modelNameV4RNNT, "ru", true},
		{modelNameV4RNNT, "en", true},
		{modelNameV4RNNT, "de", false},
		{modelNameV4CTC, "ru", true},
		{modelNameV4CTC, "en", true},
		{modelNameV4CTC, "fr", false},
		{modelNameV3, "ru", true},
		{modelNameV3, "en", false},
	}
	for _, tc := range cases {
		e := &engine{model: tc.model}
		if got := e.supports(tc.lang); got != tc.want {
			t.Errorf("model=%s lang=%s: got %v, want %v", tc.model, tc.lang, got, tc.want)
		}
	}
}

func TestRNNTLive(t *testing.T) {
	if os.Getenv("TG_STT_LIVE") == "" {
		t.Skip("установи TG_STT_LIVE=1 для живой проверки ONNX-инференса")
	}
	dir := filepath.Join("models", asrDirV4RNNT)
	m, err := loadRNNT(dir, 2)
	if err != nil {
		t.Fatalf("loadRNNT: %v", err)
	}
	defer m.close()

	// 1 секунда тишины: проверяем, что encoder + advance отрабатывают без ошибок и утечек.
	wav := make([]float32, sampleRate)
	text, err := m.decodeChunk(wav)
	if err != nil {
		t.Fatalf("decodeChunk(silence): %v", err)
	}
	t.Logf("silence decoded to: %q", text)

	// Также проверяем совместимость engine.load() (onnxruntime_go + sherpa-onnx whisper LID в одном процессе).
	e := &engine{dir: "models"}
	defer e.close()
	res, err := e.handle(request{ID: 0, Ping: true})
	if err != nil {
		t.Fatalf("engine.handle(ping): %v", err)
	}
	if res["ready"] != true || res["model"] != modelNameV4RNNT {
		t.Fatalf("ping=%v, want ready=true model=%s", res, modelNameV4RNNT)
	}
}

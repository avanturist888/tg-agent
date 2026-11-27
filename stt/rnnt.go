package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	rnntEncoderFile   = "v4_rnnt_encoder.int8.onnx"
	rnntAdvanceFile   = "v4_rnnt_advance.onnx"
	rnntMelFile       = "v4_rnnt_mel.npz"
	rnntTokenizerFile = "v4_rnnt_e2e_tokenizer.model"
)

var ortInitMu sync.Mutex

func ortSharedLibPath() string {
	if p := os.Getenv("TG_ORT_LIB"); p != "" {
		return p
	}
	name := "libonnxruntime.so"
	switch runtime.GOOS {
	case "windows":
		name = "onnxruntime.dll"
	case "darwin":
		name = "libonnxruntime.dylib"
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return name
}

var (
	ortRefs int
)

func acquireORT() error {
	ortInitMu.Lock()
	defer ortInitMu.Unlock()
	if !ort.IsInitialized() {
		ort.SetSharedLibraryPath(ortSharedLibPath())
		if err := ort.InitializeEnvironment(ort.WithLogLevelError()); err != nil {
			return fmt.Errorf("инициализация onnxruntime: %w", err)
		}
	}
	ortRefs++
	return nil
}

func releaseORT() {
	ortInitMu.Lock()
	defer ortInitMu.Unlock()
	if ortRefs > 0 {
		ortRefs--
	}
	if ortRefs == 0 && ort.IsInitialized() {
		_ = ort.DestroyEnvironment()
	}
}

// rnntModel — GigaAM-v4 RNN-T (v4_e2e_rnnt) поверх onnxruntime.dll.
type rnntModel struct {
	mel     *melExtractor
	tok     *spDecoder
	encoder *ort.DynamicAdvancedSession
	advance *ort.DynamicAdvancedSession
	ortHeld bool
}

func newCPUSessionOptions(numThreads int, optLevel ort.GraphOptimizationLevel) (*ort.SessionOptions, error) {
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	if err := opts.SetIntraOpNumThreads(numThreads); err != nil {
		_ = opts.Destroy()
		return nil, err
	}
	if err := opts.SetInterOpNumThreads(1); err != nil {
		_ = opts.Destroy()
		return nil, err
	}
	if err := opts.SetGraphOptimizationLevel(optLevel); err != nil {
		_ = opts.Destroy()
		return nil, err
	}
	if err := opts.AddSessionConfigEntry("session.intra_op.allow_spinning", "0"); err != nil {
		_ = opts.Destroy()
		return nil, err
	}
	return opts, nil
}

func loadRNNT(dir string, numThreads int) (*rnntModel, error) {
	mel, err := loadMelNPZ(filepath.Join(dir, rnntMelFile))
	if err != nil {
		return nil, err
	}
	tok, err := loadSentencePiece(filepath.Join(dir, rnntTokenizerFile))
	if err != nil {
		return nil, err
	}
	if err := acquireORT(); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			releaseORT()
		}
	}()

	encPath := filepath.Join(dir, rnntEncoderFile)
	advPath := filepath.Join(dir, rnntAdvanceFile)

	// Читаем имена входов/выходов динамически без тяжёлой оптимизации графа.
	inspectOpts, err := newCPUSessionOptions(1, ort.GraphOptimizationLevelDisableAll)
	if err != nil {
		return nil, fmt.Errorf("опции инспекции ORT: %w", err)
	}
	encIns, encOuts, err := ort.GetInputOutputInfoWithOptions(encPath, inspectOpts)
	if err != nil {
		_ = inspectOpts.Destroy()
		return nil, fmt.Errorf("входы/выходы %s: %w", rnntEncoderFile, err)
	}
	advIns, advOuts, err := ort.GetInputOutputInfoWithOptions(advPath, inspectOpts)
	_ = inspectOpts.Destroy()
	if err != nil {
		return nil, fmt.Errorf("входы/выходы %s: %w", rnntAdvanceFile, err)
	}

	encInNames, encOutNames, err := resolveEncoderIO(encIns, encOuts)
	if err != nil {
		return nil, err
	}
	advInNames, advOutNames, err := resolveAdvanceIO(advIns, advOuts)
	if err != nil {
		return nil, err
	}

	runOpts, err := newCPUSessionOptions(numThreads, ort.GraphOptimizationLevelEnableAll)
	if err != nil {
		return nil, fmt.Errorf("опции сессии ORT: %w", err)
	}
	defer runOpts.Destroy()

	encSess, err := ort.NewDynamicAdvancedSession(encPath, encInNames, encOutNames, runOpts)
	if err != nil {
		return nil, fmt.Errorf("загрузка %s: %w", rnntEncoderFile, err)
	}
	advSess, err := ort.NewDynamicAdvancedSession(advPath, advInNames, advOutNames, runOpts)
	if err != nil {
		_ = encSess.Destroy()
		return nil, fmt.Errorf("загрузка %s: %w", rnntAdvanceFile, err)
	}
	ok = true
	return &rnntModel{
		mel:     mel,
		tok:     tok,
		encoder: encSess,
		advance: advSess,
		ortHeld: true,
	}, nil
}

// resolveEncoderIO возвращает имена входов [features, sizes] и выходов [projected, lengths].
func resolveEncoderIO(ins, outs []ort.InputOutputInfo) ([]string, []string, error) {
	if len(ins) != 2 {
		return nil, nil, fmt.Errorf("encoder: ожидалось 2 входа, получено %d", len(ins))
	}
	if len(outs) != 2 {
		return nil, nil, fmt.Errorf("encoder: ожидалось 2 выхода, получено %d", len(outs))
	}

	// Входы: float32 features [1, 64, T] и int64 sizes [1].
	featName, sizeName := ins[0].Name, ins[1].Name
	if ins[0].DataType == ort.TensorElementDataTypeInt64 && ins[1].DataType == ort.TensorElementDataTypeFloat {
		featName, sizeName = ins[1].Name, ins[0].Name
	}

	// Выходы: сопоставляем по имени с входами advance ("projected" и "lengths");
	// если имена отличаются, берём порядок из референса (lengths, projected).
	var projName, lenName string
	for _, o := range outs {
		switch strings.ToLower(o.Name) {
		case "projected":
			projName = o.Name
		case "lengths":
			lenName = o.Name
		}
	}
	if projName == "" || lenName == "" {
		// Фолбэк: сначала различаем по типу (int64 = lengths, float32 = projected), затем по порядку.
		if outs[0].DataType == ort.TensorElementDataTypeInt64 && outs[1].DataType == ort.TensorElementDataTypeFloat {
			lenName, projName = outs[0].Name, outs[1].Name
		} else if outs[1].DataType == ort.TensorElementDataTypeInt64 && outs[0].DataType == ort.TensorElementDataTypeFloat {
			projName, lenName = outs[0].Name, outs[1].Name
		} else {
			lenName, projName = outs[0].Name, outs[1].Name
		}
	}
	return []string{featName, sizeName}, []string{projName, lenName}, nil
}

// resolveAdvanceIO проверяет входы "projected" и "lengths" и возвращает выход tokens.
func resolveAdvanceIO(ins, outs []ort.InputOutputInfo) ([]string, []string, error) {
	if len(outs) != 1 {
		return nil, nil, fmt.Errorf("advance: ожидался 1 выход, получено %d", len(outs))
	}
	var hasProj, hasLen bool
	for _, in := range ins {
		switch in.Name {
		case "projected":
			hasProj = true
		case "lengths":
			hasLen = true
		}
	}
	if !hasProj || !hasLen {
		return nil, nil, fmt.Errorf("advance: ожидались входы projected и lengths, получено %v", ins)
	}
	return []string{"projected", "lengths"}, []string{outs[0].Name}, nil
}

func destroyValues(vals []ort.Value) {
	for i, v := range vals {
		if v != nil {
			_ = v.Destroy()
			vals[i] = nil
		}
	}
}

func (m *rnntModel) decodeChunk(audio []float32) (string, error) {
	feats, numFrames := m.mel.extract(audio)

	featTensor, err := ort.NewTensor(ort.NewShape(1, melBins, int64(numFrames)), feats)
	if err != nil {
		return "", fmt.Errorf("тензор features: %w", err)
	}
	defer featTensor.Destroy()

	sizeTensor, err := ort.NewTensor(ort.NewShape(1), []int64{int64(numFrames)})
	if err != nil {
		return "", fmt.Errorf("тензор sizes: %w", err)
	}
	defer sizeTensor.Destroy()

	encOut := make([]ort.Value, 2)
	defer destroyValues(encOut)
	if err := m.encoder.Run([]ort.Value{featTensor, sizeTensor}, encOut); err != nil {
		return "", fmt.Errorf("encoder run: %w", err)
	}

	advOut := make([]ort.Value, 1)
	defer destroyValues(advOut)
	// encOut уже в порядке [projected, lengths], как ждёт advance.
	if err := m.advance.Run(encOut, advOut); err != nil {
		return "", fmt.Errorf("advance run: %w", err)
	}

	tokTensor, ok := advOut[0].(*ort.Tensor[int64])
	if !ok {
		return "", fmt.Errorf("advance: неожиданный тип выхода %T", advOut[0])
	}
	shape := tokTensor.GetShape()
	if len(shape) != 3 || shape[1] < 1 || shape[2] < 1 {
		return "", fmt.Errorf("advance: неожиданная форма tokens %v", shape)
	}
	L, numHeads, B := int(shape[0]), int(shape[1]), int(shape[2])
	if L == 0 {
		return "", nil
	}
	data := tokTensor.GetData()
	stride := numHeads * B
	ids := make([]int64, 0, L)
	for i := 0; i < L; i++ {
		if tok := data[i*stride]; tok >= 0 {
			ids = append(ids, tok)
		}
	}
	return m.tok.decode(ids), nil
}

func (m *rnntModel) close() {
	if m == nil {
		return
	}
	if m.advance != nil {
		_ = m.advance.Destroy()
		m.advance = nil
	}
	if m.encoder != nil {
		_ = m.encoder.Destroy()
		m.encoder = nil
	}
	if m.ortHeld {
		m.ortHeld = false
		releaseORT()
	}
}

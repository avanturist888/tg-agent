# Расшифровка голосовых через GigaAM: модели в stt\models (≈ 380 МБ).
#
#   powershell -ExecutionPolicy Bypass -File scripts\install-stt.ps1
#
# Python не нужен: расшифровывает bin\tgstt.exe (sherpa-onnx, на процессоре),
# его собирает scripts\build.ps1 (нужен gcc — MinGW). Нужен ffmpeg в PATH.
# Модели — официальные сборки sherpa-onnx:
#   - GigaAM v3 с пунктуацией (e2e RNN-T, int8) — русская речь;
#   - whisper base (int8) — только определить язык;
#   - Silero VAD — нарезать длинные записи по паузам.
# Служба подхватывает расшифровку сама — перезапускать ничего не нужно.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$root = Split-Path -Parent $PSScriptRoot
$models = Join-Path $root 'stt\models'
$base = 'https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models'
New-Item -ItemType Directory -Force $models | Out-Null

if (-not (Get-Command ffmpeg -ErrorAction SilentlyContinue)) {
    Write-Warning 'ffmpeg не найден в PATH — без него голосовые не прочитать (winget install Gyan.FFmpeg).'
}

function Get-Model($archive, $dir, [string[]]$keep) {
    $dst = Join-Path $models $dir
    if (Test-Path $dst) { Write-Host "есть: $dir"; return }
    $tmp = Join-Path $env:TEMP "tgstt-$archive"
    Write-Host "скачиваю $archive…"
    Invoke-WebRequest "$base/$archive" -OutFile $tmp
    $unpack = Join-Path $env:TEMP "tgstt-unpack-$dir"
    Remove-Item $unpack -Recurse -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force $unpack | Out-Null
    # tar из Windows: tar из Git (если он первым в PATH) не понимает пути C:\…
    & (Join-Path $env:SystemRoot 'System32\tar.exe') -xjf $tmp -C $unpack
    if ($LASTEXITCODE) { throw "не распаковался $archive" }
    New-Item -ItemType Directory -Force $dst | Out-Null
    foreach ($f in $keep) { Move-Item (Join-Path $unpack "$dir\$f") $dst }
    Remove-Item $tmp, $unpack -Recurse -Force
}

Get-Model 'sherpa-onnx-nemo-transducer-punct-giga-am-v3-russian-2025-12-16.tar.bz2' `
    'sherpa-onnx-nemo-transducer-punct-giga-am-v3-russian-2025-12-16' `
    @('encoder.int8.onnx', 'decoder.onnx', 'joiner.onnx', 'tokens.txt', 'LICENSE')
Get-Model 'sherpa-onnx-whisper-base.tar.bz2' 'sherpa-onnx-whisper-base' `
    @('base-encoder.int8.onnx', 'base-decoder.int8.onnx', 'base-tokens.txt')
$vad = Join-Path $models 'silero_vad.onnx'
if (-not (Test-Path $vad)) {
    Write-Host 'скачиваю silero_vad.onnx…'
    Invoke-WebRequest "$base/silero_vad.onnx" -OutFile $vad
}

$exe = Join-Path $root 'bin\tgstt.exe'
if (-not (Test-Path $exe)) {
    Write-Warning 'bin\tgstt.exe ещё не собран — запусти scripts\build.ps1 (нужен gcc, MinGW).'
    return
}
Write-Host 'Проверяю…'
$out = & $exe --check
Write-Host $out
if ($out -notmatch '"ready":true') { throw 'проверка не прошла — см. вывод выше' }
Write-Host 'Готово: голосовые на русском теперь расшифровывает GigaAM.'

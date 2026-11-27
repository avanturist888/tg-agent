# Расшифровка голосовых через GigaAM: модели в stt\models.
#
#   powershell -ExecutionPolicy Bypass -File scripts\install-stt.ps1
#
# Python не нужен: расшифровывает bin\tgstt.exe (sherpa-onnx, на процессоре),
# его собирает scripts\build.ps1 (нужен gcc — MinGW). Нужен ffmpeg в PATH.
# Модели:
#   - GigaAM v4 с пунктуацией (ctc-e2e, int8) — русская и английская речь
#     (включая code-switching);
#   - whisper base (int8) — только определить язык;
#   - Silero VAD — нарезать длинные записи по паузам.
# Служба подхватывает расшифровку сама — перезапускать ничего не нужно.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$root = Split-Path -Parent $PSScriptRoot
$models = Join-Path $root 'stt\models'
$base = 'https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models'
$v4Base = 'https://huggingface.co/ai-babai/gigaam-v4-onnx/resolve/9c1f125787db5befcfcd3c8e232a642f9b1531f6/ctc-e2e'
New-Item -ItemType Directory -Force $models | Out-Null

if (-not (Get-Command ffmpeg -ErrorAction SilentlyContinue)) {
    Write-Warning 'ffmpeg не найден в PATH — без него голосовые не прочитать (winget install Gyan.FFmpeg).'
}

function Get-VerifiedFile($url, $dst, $expectedSha256) {
    $name = Split-Path -Leaf $dst
    $want = $expectedSha256.ToLowerInvariant()
    if (Test-Path $dst) {
        $got = (Get-FileHash -Algorithm SHA256 $dst).Hash.ToLowerInvariant()
        if ($got -eq $want) {
            Write-Host "есть: $name"
            return
        }
        Write-Warning "не совпал SHA-256 у существующего ${dst} (получено $got) — перекачиваю…"
    }
    $tmp = "$dst.part"
    Write-Host "скачиваю $name…"
    try {
        Invoke-WebRequest $url -OutFile $tmp
        $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLowerInvariant()
        if ($got -ne $want) {
            throw "не совпал SHA-256 для ${name}: ожидалось $want, получено $got"
        }
        Move-Item -LiteralPath $tmp -Destination $dst -Force
    } catch {
        if (Test-Path -LiteralPath $tmp) {
            Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        }
        throw
    }
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

$v4Dir = Join-Path $models 'giga-am-v4-ctc-e2e'
New-Item -ItemType Directory -Force $v4Dir | Out-Null
Get-VerifiedFile "$v4Base/model.int8.onnx" (Join-Path $v4Dir 'model.int8.onnx') `
    '0f61ec8ca69517cb68e24531c5000053b80ab84cf7071cc294571edfae493462'
Get-VerifiedFile "$v4Base/tokens.txt" (Join-Path $v4Dir 'tokens.txt') `
    'c6a37fab667d53c37e66835c90083808daa116c3993c5e93ce518b5a3896de6e'
Get-VerifiedFile "$v4Base/LICENSE" (Join-Path $v4Dir 'LICENSE') `
    'd02ed0afc408cb4ed5eb34b233096ad8c3db8b6751cd9385276a6725dbf92925'

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
$out = (& $exe --check) -join "`n"
$code = $LASTEXITCODE
Write-Host $out
if ($code -ne 0) { throw "проверка не прошла (код $code) — см. вывод выше" }
$res = $out | ConvertFrom-Json
if (-not $res.ready -or $res.model -ne 'v4_e2e_ctc') {
    throw "ожидалась модель v4_e2e_ctc с ready=true, получено ready=$($res.ready), model=$($res.model) — пересобери bin\tgstt.exe"
}
Write-Host 'Готово: голосовые на русском и английском теперь расшифровывает GigaAM v4.'

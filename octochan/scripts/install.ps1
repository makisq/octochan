# Собирает ochan и ochan-mcp и кладёт их в каталог из PATH пользователя:
# после этого они вызываются по имени — ochan, ochan-mcp — в любом новом
# окне PowerShell или cmd.
#
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Rlm     ещё и модуль RLM
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Llm     ещё и адаптер своей модели ℒ:
#       llama\*.py в <Bin>\llm, venv с requirements.txt; -Python — интерпретатор (по умолчанию py -3),
#       -TorchIndex — индекс pip для torch, например https://download.pytorch.org/whl/cu128
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Uninstall
#
# Каталог по умолчанию — %LOCALAPPDATA%\Programs\ochan; другой — -Bin.
param(
    [string]$Bin = (Join-Path $env:LOCALAPPDATA 'Programs\ochan'),
    [switch]$Uninstall,
    [switch]$Rlm,
    [switch]$Llm,
    [string]$Python = '',
    [string]$TorchIndex = ''
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $userPath) { $userPath = '' }
$parts = @($userPath -split ';' | Where-Object { $_ -ne '' })

if ($Uninstall) {
    foreach ($name in 'ochan.exe', 'ochan-mcp.exe') {
        $file = Join-Path $Bin $name
        if (Test-Path $file) { Remove-Item $file; Write-Host "Удалён $file" }
    }
    $kept = @($parts | Where-Object { $_ -ne $Bin })
    [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
    return
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Нужен Go 1.22 или новее: https://go.dev/dl/'
}

New-Item -ItemType Directory -Force -Path $Bin | Out-Null
Write-Host 'Сборка ochan…'
Push-Location $root
try { go build -trimpath -o (Join-Path $Bin 'ochan.exe') . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { throw 'сборка ochan не удалась' }
Write-Host 'Сборка ochan-mcp…'
Push-Location (Join-Path $root 'mcp')
try { go build -trimpath -o (Join-Path $Bin 'ochan-mcp.exe') . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { throw 'сборка ochan-mcp не удалась' }
Write-Host "Установлено: $Bin"

if ($Rlm) {
    # Модуль ставит сам ochan: он фиксирует контрольную сумму, и подменённый
    # файл модуля потом не загрузится.
    Write-Host 'Сборка модуля RLM…'
    $module = Join-Path $Bin 'rlm.hcplugin'
    Push-Location $root
    try { go build -trimpath -o $module ./plugins/rlm } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw 'сборка модуля RLM не удалась' }
    & (Join-Path $Bin 'ochan.exe') '!!install' $module
    if ($LASTEXITCODE -ne 0) { throw 'модуль RLM не установился' }
    Write-Host 'Токен RLM: ochan auth --executor rlm; адрес — executors.rlm.api_url в ~/.octochan/config.yaml'
}

if ($Llm) {
    # Адаптер своей модели ℒ: ochan находит его сам — adapter.py в <Bin>\llm,
    # интерпретатор — venv рядом с ним.
    $llmDir = Join-Path $Bin 'llm'
    New-Item -ItemType Directory -Force -Path $llmDir | Out-Null
    Copy-Item (Join-Path $root 'llama\*.py') $llmDir -Force
    Copy-Item (Join-Path $root 'llama\requirements.txt') $llmDir -Force
    Write-Host 'Сборка fpcheck…'
    Push-Location $root
    try { go build -trimpath -o (Join-Path $llmDir 'fpcheck.exe') ./llama/fpcheck } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw 'сборка fpcheck не удалась' }
    $venvPython = Join-Path $llmDir 'venv\Scripts\python.exe'
    if (-not (Test-Path $venvPython)) {
        Write-Host "Окружение Python: $(Join-Path $llmDir 'venv')"
        if ($Python) { & $Python -m venv (Join-Path $llmDir 'venv') } else { & py -3 -m venv (Join-Path $llmDir 'venv') }
        if ($LASTEXITCODE -ne 0) { throw 'venv не создан: нужен Python 3.10 или новее (-Python путь)' }
    }
    if ($TorchIndex) {
        & $venvPython -m pip install torch --index-url $TorchIndex
        if ($LASTEXITCODE -ne 0) { throw 'torch не установился' }
    }
    & $venvPython -m pip install -r (Join-Path $llmDir 'requirements.txt')
    if ($LASTEXITCODE -ne 0) { throw 'зависимости адаптера не установились' }
    Write-Host "Адаптер своей модели: $(Join-Path $llmDir 'adapter.py'); в config.yaml — llm.model и llm.endpoint, запуск — ochan llm serve"
}

if ($parts -notcontains $Bin) {
    [Environment]::SetEnvironmentVariable('Path', (($parts + $Bin) -join ';'), 'User')
    Write-Host "Каталог $Bin добавлен в PATH пользователя. Откройте новое окно терминала и наберите: ochan --version"
} else {
    & (Join-Path $Bin 'ochan.exe') --version
}

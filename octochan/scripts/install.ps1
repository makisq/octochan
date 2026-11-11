# Собирает ochan и ochan-mcp и ставит их. Куда — решаете вы.
#
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1            спросит: всё в эту папку или каждый путь отдельно
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Here      быстрая без вопросов: всё в текущей папке
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Bin D:\tools -HomeDir D:\ochan -Modules D:\ochan\modules
#                                                                           ручная без вопросов; незаданное — по умолчанию
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Standard  в %LOCALAPPDATA%\Programs\ochan и в PATH пользователя
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Here -Desktop   ещё и значок: ярлыки в меню «Пуск» и на рабочем столе
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Rlm       ещё и модуль RLM
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Uninstall
#
# Быстрая установка — для машин, где работать и запускать программы
# разрешено только в одной папке: рядом с ochan.exe остаётся указатель
# ochan.home, и переменные окружения не нужны. Вопросы задаёт сам ochan
# (ochan setup) и читает ответы из stdin построчно; без ответа ставит в
# каталоги по умолчанию.
param(
    [switch]$Here,
    [string]$Bin = '',
    [string]$HomeDir = '',
    [string]$Modules = '',
    [string]$Logs = '',
    [string]$Reports = '',
    [string]$RegistryCache = '',
    [switch]$Standard,
    [switch]$Desktop,
    [switch]$Uninstall,
    [switch]$Rlm
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$standardBin = Join-Path $env:LOCALAPPDATA 'Programs\ochan'

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $userPath) { $userPath = '' }
$parts = @($userPath -split ';' | Where-Object { $_ -ne '' })

if ($Uninstall) {
    if ($Bin -eq '') { $Bin = $standardBin }
    # Ярлыки убирает сама программа, пока она есть.
    $installed = Join-Path $Bin 'ochan.exe'
    if (Test-Path $installed) { & $installed setup --desktop=remove }
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

$out = Join-Path $root 'bin'
New-Item -ItemType Directory -Force -Path $out | Out-Null
Write-Host 'Сборка ochan…'
Push-Location $root
try { go build -trimpath -o (Join-Path $out 'ochan.exe') . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { throw 'сборка ochan не удалась' }
Write-Host 'Сборка ochan-mcp…'
Push-Location (Join-Path $root 'mcp')
try { go build -trimpath -o (Join-Path $out 'ochan-mcp.exe') . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { throw 'сборка ochan-mcp не удалась' }
$module = Join-Path $out 'rlm.hcplugin'
if ($Rlm) {
    Write-Host 'Сборка модуля RLM…'
    Push-Location $root
    try { go build -trimpath -o $module ./plugins/rlm } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw 'сборка модуля RLM не удалась' }
}

# Куда ставить, решает ochan setup: спрашивает или берёт из ключей.
$setup = @('setup')
if ($Standard) {
    if ($Bin -eq '') { $Bin = $standardBin }
} elseif ($Here) {
    $setup += '--here'
}
if (-not $Here) {
    if ($Bin -ne '') { $setup += "--bin=$Bin" }
    if ($HomeDir -ne '') { $setup += "--home=$HomeDir" }
    if ($Modules -ne '') { $setup += "--modules=$Modules" }
    if ($Logs -ne '') { $setup += "--logs=$Logs" }
    if ($Reports -ne '') { $setup += "--reports=$Reports" }
    if ($RegistryCache -ne '') { $setup += "--registry-cache=$RegistryCache" }
}
# Модуль ставит сам ochan: он фиксирует контрольную сумму, и подменённый
# файл модуля потом не загрузится.
if ($Rlm) { $setup += "--module=$module" }
if ($Desktop) { $setup += '--desktop' }
& (Join-Path $out 'ochan.exe') @setup
if ($LASTEXITCODE -ne 0) { throw 'установка не удалась' }
if ($Rlm) { Write-Host 'Токен RLM: ochan auth --executor rlm; адрес — executors.rlm.api_url в config.yaml' }

# В PATH пользователя каталог добавляется, только когда его назвали: при
# установке в папку ochan запускают из неё.
if ($Bin -ne '' -and $parts -notcontains $Bin) {
    [Environment]::SetEnvironmentVariable('Path', (($parts + $Bin) -join ';'), 'User')
    Write-Host "Каталог $Bin добавлен в PATH пользователя. Откройте новое окно терминала и наберите: ochan --version"
}

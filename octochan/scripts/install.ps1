# Собирает ochan и ochan-mcp и кладёт их в каталог из PATH пользователя:
# после этого они вызываются по имени — ochan, ochan-mcp — в любом новом
# окне PowerShell или cmd.
#
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1 -Uninstall
#
# Каталог по умолчанию — %LOCALAPPDATA%\Programs\ochan; другой — -Bin.
param(
    [string]$Bin = (Join-Path $env:LOCALAPPDATA 'Programs\ochan'),
    [switch]$Uninstall
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

if ($parts -notcontains $Bin) {
    [Environment]::SetEnvironmentVariable('Path', (($parts + $Bin) -join ';'), 'User')
    Write-Host "Каталог $Bin добавлен в PATH пользователя. Откройте новое окно терминала и наберите: ochan --version"
} else {
    & (Join-Path $Bin 'ochan.exe') --version
}

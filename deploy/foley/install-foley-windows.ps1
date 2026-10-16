<#
Installs (or updates) the HunyuanVideo-Foley service (foley_server.py, next to this script) as a
background scheduled task, for HomeLLM's "add sound to a clip" (homellm -sound foley).

Prerequisites: a HunyuanVideo-Foley checkout, a Python venv with its requirements (PyTorch built
for your GPU's CUDA), the XL weights + config_xl.yaml in -Models, and ffmpeg. Then, from an
elevated PowerShell:

  powershell -ExecutionPolicy Bypass -File install-foley-windows.ps1 `
    -CodeDir D:\HomeAI\HunyuanVideo-Foley -Python D:\HomeAI\foley-venv\Scripts\python.exe `
    -Models D:\HomeAI\foley-models -FfmpegDir C:\path\to\ffmpeg\bin

Like HomeLLM's own task: runs at boot whether or not anyone is logged in (S4U), restarts the
service 5 seconds after any exit, logs to foley.log in CodeDir, and listens on 127.0.0.1 only.
#>
param(
    [Parameter(Mandatory)][string]$CodeDir,
    [Parameter(Mandatory)][string]$Python,
    [Parameter(Mandatory)][string]$Models,
    [Parameter(Mandatory)][string]$FfmpegDir,
    [string]$HfHome = "",
    [int]$Port = 8190
)
$ErrorActionPreference = "Stop"
$taskName = "HomeLLM-Foley"
foreach ($p in @($CodeDir, $Python, $Models, (Join-Path $FfmpegDir "ffmpeg.exe"))) {
    if (-not (Test-Path $p)) { throw "not found: $p" }
}
if (-not $HfHome) { $HfHome = Join-Path (Split-Path $CodeDir) "hf-cache" }

if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $taskName
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
}
Get-Process python -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $Python } | Stop-Process -Force
Start-Sleep -Seconds 1

Copy-Item -Force (Join-Path $PSScriptRoot "foley_server.py") $CodeDir
$log = Join-Path $CodeDir "foley.log"
# ping, not timeout: timeout.exe fails without a console, which an S4U task doesn't have.
@"
@echo off
cd /d "$CodeDir"
set HF_HOME=$HfHome
set PATH=$FfmpegDir;%PATH%
:loop
"$Python" foley_server.py --models "$Models" --port $Port >> "$log" 2>&1
ping -n 6 127.0.0.1 >nul
goto loop
"@ | Set-Content -Encoding ASCII (Join-Path $CodeDir "run-homellm-foley.cmd")

$action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c `"$CodeDir\run-homellm-foley.cmd`"" -WorkingDirectory $CodeDir
$trigger = New-ScheduledTaskTrigger -AtStartup
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $taskName

Write-Host "Foley service task '$taskName' started on http://127.0.0.1:$Port. Log: $log"
Write-Host "Then run HomeLLM with -sound foley."

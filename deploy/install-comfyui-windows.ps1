<#
Installs (or updates) ComfyUI as a background scheduled task, for HomeLLM's image generation
(homellm -images comfyui). Run from an elevated PowerShell, pointing at an unpacked
ComfyUI_windows_portable (the NVIDIA build from github.com/Comfy-Org/ComfyUI/releases), with
the image model already in ComfyUI\models\checkpoints:

  powershell -ExecutionPolicy Bypass -File install-comfyui-windows.ps1 -ComfyDir F:\HomeAI\ComfyUI_windows_portable

Like HomeLLM's own task: runs at boot whether or not anyone is logged in (S4U), restarts
ComfyUI 5 seconds after any exit, and logs to comfyui.log in ComfyDir. ComfyUI only listens
on 127.0.0.1 - HomeLLM is its only client; nothing on the network can reach it.
#>
param(
    [Parameter(Mandatory)][string]$ComfyDir,
    [int]$Port = 8188,
    # Extra ComfyUI flags, e.g. the memory options Wan 2.2 needs on some machines (see README).
    [string]$ExtraArgs = ""
)
$ErrorActionPreference = "Stop"
$taskName = "HomeLLM-ComfyUI"
$python = Join-Path $ComfyDir "python_embeded\python.exe"
$main = Join-Path $ComfyDir "ComfyUI\main.py"
if (-not (Test-Path $python) -or -not (Test-Path $main)) { throw "no ComfyUI portable install in $ComfyDir" }

if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $taskName
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
}
# Stopping the task doesn't stop the python it started; stop this install's copies.
Get-Process python -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $python } | Stop-Process -Force
Start-Sleep -Seconds 1

$log = Join-Path $ComfyDir "comfyui.log"
# ping, not timeout: timeout.exe fails without a console, which an S4U task doesn't have.
@"
@echo off
cd /d "$ComfyDir"
:loop
"$python" -s "$main" --listen 127.0.0.1 --port $Port --disable-auto-launch $ExtraArgs >> "$log" 2>&1
ping -n 6 127.0.0.1 >nul
goto loop
"@ | Set-Content -Encoding ASCII (Join-Path $ComfyDir "run-homellm.cmd")

$action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c `"$ComfyDir\run-homellm.cmd`"" -WorkingDirectory $ComfyDir
$trigger = New-ScheduledTaskTrigger -AtStartup
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $taskName

Write-Host "ComfyUI task '$taskName' started on http://127.0.0.1:$Port. Log: $log"
Write-Host "Then run HomeLLM with -images comfyui (install-windows.ps1 -ExtraArgs '-images comfyui')."

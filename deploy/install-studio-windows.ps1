<#
Installs (or updates) HomeLLM Studio on the PC itself, as a background scheduled task, so it can
be used from anywhere at https://homellm.eiuoo.com/studio/ through HomeLLM's web-app relay.

Put this script next to homellm-studio.exe (GOOS=windows go build -o homellm-studio.exe ./cmd/studio)
and run it on the PC:

  powershell -ExecutionPolicy Bypass -File install-studio-windows.ps1

Then start HomeLLM with -app studio=http://127.0.0.1:8777 (install-windows.ps1 -ExtraArgs).
Studio talks to ComfyUI, Ollama and HomeLLM's control port on this PC directly (no SSH tunnel)
and keeps its runs in -RunsDir.
#>
param(
    [string]$RunsDir = "D:\HomeLLM-Studio",
    [int]$Port = 8777
)
$ErrorActionPreference = "Stop"
$taskName = "HomeLLM-Studio"
$dir = $PSScriptRoot
$exe = Join-Path $dir "homellm-studio.exe"

if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $taskName
}
Get-Process homellm-studio -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe } | Stop-Process -Force
Start-Sleep -Seconds 1
if (Test-Path "$exe.new") { Move-Item -Force "$exe.new" $exe }
if (-not (Test-Path $exe)) { throw "homellm-studio.exe not found in $dir" }

$flags = "-comfy http://127.0.0.1:8188 -ollama http://127.0.0.1:11434 -control http://127.0.0.1:8091 " +
    "-addr 127.0.0.1:$Port -dir `"$RunsDir`" -no-open"
# ping, not timeout: timeout.exe fails without a console, which an S4U task doesn't have.
@"
@echo off
:loop
"$exe" $flags >> "$dir\studio.log" 2>&1
ping -n 6 127.0.0.1 >nul
goto loop
"@ | Set-Content -Encoding ASCII (Join-Path $dir "run-studio.cmd")

$action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c `"$dir\run-studio.cmd`"" -WorkingDirectory $dir
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger (New-ScheduledTaskTrigger -AtStartup) `
    -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName $taskName
Write-Host "HomeLLM Studio running on 127.0.0.1:$Port (task '$taskName'); runs in $RunsDir."

<#
Installs (or updates) HomeLLM as a background scheduled task on Windows.

Put this script next to homellm.exe (or homellm.exe.new, for an update of a running install)
and run it from an elevated PowerShell:

  powershell -ExecutionPolicy Bypass -File install-windows.ps1 -SignalUrl ws://192.168.1.187:8090/ws

The task runs at boot whether or not anyone is logged in (S4U: no stored password, no window),
and run.cmd restarts homellm 5 seconds after any exit. Output goes to homellm.log here.
Pair phones afterwards with:  homellm.exe pair
#>
param(
    [Parameter(Mandatory)][string]$SignalUrl,
    [string]$PublicSignalUrl = "",
    [string]$Model = "gemma3:12b",
    [string]$Name = $env:COMPUTERNAME,
    [string]$ExtraArgs = ""
)
$ErrorActionPreference = "Stop"
$taskName = "HomeLLM"
$dir = $PSScriptRoot
$exe = Join-Path $dir "homellm.exe"

# Stop any running copy first: the exe is locked while it runs, and a second copy with the
# same identity would be dropped by signalling anyway.
if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $taskName
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
}
Get-Process homellm -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe } | Stop-Process -Force
Start-Sleep -Seconds 1
if (Test-Path "$exe.new") { Move-Item -Force "$exe.new" $exe }
if (-not (Test-Path $exe)) { throw "homellm.exe not found in $dir" }

$flags = "-signal `"$SignalUrl`" -model `"$Model`" -name `"$Name`" -dir `"$dir\data`" -log `"$dir\homellm.log`""
if ($PublicSignalUrl) { $flags += " -public-signal `"$PublicSignalUrl`"" }
if ($ExtraArgs) { $flags += " $ExtraArgs" }

# ping, not timeout: timeout.exe fails without a console, which an S4U task doesn't have.
@"
@echo off
:loop
"$exe" $flags
ping -n 6 127.0.0.1 >nul
goto loop
"@ | Set-Content -Encoding ASCII (Join-Path $dir "run.cmd")

$action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c `"$dir\run.cmd`"" -WorkingDirectory $dir
$trigger = New-ScheduledTaskTrigger -AtStartup
# Not $env:USERDOMAIN\$env:USERNAME: over SSH, USERDOMAIN is "WORKGROUP", which isn't an account.
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $taskName

Write-Host "HomeLLM installed in $dir and started (task '$taskName')."
Write-Host "Log: $dir\homellm.log   Pair a phone: & '$exe' pair"

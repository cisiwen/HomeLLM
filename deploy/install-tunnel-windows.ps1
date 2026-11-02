<#
Installs (or updates) tunnel-home as a background scheduled task on Windows.

Put this script next to tunnel-home.exe (or tunnel-home.exe.new, for an update of a running
install) and run it from an elevated PowerShell:

  powershell -ExecutionPolicy Bypass -File install-tunnel-windows.ps1 -SignalUrl wss://<id>.execute-api.<region>.amazonaws.com/prod

The task runs at boot whether or not anyone is logged in (S4U: no stored password, no window),
and run.cmd restarts tunnel-home 5 seconds after any exit. Output goes to tunnel-home.log here.

Pair a client BEFORE installing, in a terminal on this PC (a second copy with the same
identity would replace the service on signalling, so the task must not be running):

  .\tunnel-home.exe -dir .\data -signal <same url> -pair

then run the printed `tunnel-client pair ...` command on the client and answer y here. Pairings
are kept by the signalling server, so installing afterwards keeps them.
#>
param(
    [Parameter(Mandatory)][string]$SignalUrl,
    [string]$Ports = "8000",
    [string]$Name = $env:COMPUTERNAME,
    [string]$ExtraArgs = ""
)
$ErrorActionPreference = "Stop"
$taskName = "TunnelHome"
$dir = $PSScriptRoot
$exe = Join-Path $dir "tunnel-home.exe"

if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $taskName
    Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
}
Get-Process tunnel-home -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe } | Stop-Process -Force
Start-Sleep -Seconds 1
if (Test-Path "$exe.new") { Move-Item -Force "$exe.new" $exe }
if (-not (Test-Path $exe)) { throw "tunnel-home.exe not found in $dir" }

$flags = "-signal `"$SignalUrl`" -ports `"$Ports`" -name `"$Name`" -dir `"$dir\data`" -log `"$dir\tunnel-home.log`""
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
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $taskName

Write-Host "tunnel-home installed in $dir and started (task '$taskName'), forwarding ports $Ports."
Write-Host "Log: $dir\tunnel-home.log"

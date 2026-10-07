# Starts the Ollama tray app in the logged-in desktop session (not this SSH session, whose
# processes are killed on disconnect) via a one-off interactive scheduled task.
$exe = "$env:LOCALAPPDATA\Programs\Ollama\ollama app.exe"
# Stop the old Ollama first - including its model runners (llama-server.exe), which outlive a
# killed parent and would keep holding GPU memory.
Stop-Process -Name "ollama app", ollama, llama-server -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$name = "OllamaRestartOnce"
Unregister-ScheduledTask -TaskName $name -Confirm:$false -ErrorAction SilentlyContinue
$action = New-ScheduledTaskAction -Execute $exe
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive
Register-ScheduledTask -TaskName $name -Action $action -Principal $principal | Out-Null
Start-ScheduledTask -TaskName $name
Start-Sleep -Seconds 10
Unregister-ScheduledTask -TaskName $name -Confirm:$false
Get-Process ollama*, "ollama app" -ErrorAction SilentlyContinue | Format-Table Id, ProcessName, SessionId -AutoSize
try { "Ollama API: " + (Invoke-WebRequest -UseBasicParsing http://127.0.0.1:11434/api/version).Content } catch { "Ollama API not up yet: $_" }

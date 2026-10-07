# HomeLLM

Run an LLM on your own computer at home and use it from the AndroidStreamer app, anywhere,
with **no open ports, no VPN, and no relay server in the data path**.

```
            ┌──────────── signalling (tiny; pairing + one offer/answer per connection) ────────────┐
            │                                                                                      │
 Android app ═══════════════ direct WebRTC data channels (encrypted end to end) ═══════════════ homellm ──▶ Ollama
  (AndroidStreamer                     prompts, streamed tokens, images                         (your PC)
   "Home AI")
```

1. `homellm` runs on the home PC and keeps an **outbound** WebSocket to the signalling server.
2. The phone pairs once by scanning a QR code the home server prints. The owner approves it on
   the PC.
3. To chat, the phone sends a WebRTC offer through signalling. The home server replies with an
   answer **signed with its Ed25519 key**. The phone checks it against the key from the QR, so
   even a compromised signalling server can't intercept the connection.
4. The two devices connect directly (STUN hole punching). After that, signalling is closed and
   all traffic goes device to device.

## Layout

| Path | What it is |
|---|---|
| `protocol/` | Every message shape (signalling and data channels). The Android client mirrors this by hand. |
| `internal/home/` | The home server: identity, pairing, WebRTC sessions, image receiving, LLM backends |
| `internal/signaldev/` | Local **development** signalling server |
| `cmd/homellm` | Home server binary |
| `cmd/signal-dev` | Dev signalling server binary |
| `cmd/testclient` | Plays the phone's role, for testing without a phone |

Android side: `AndroidStreamer/app/src/main/java/com/androidstreamer/app/homellm/`
(menu: **+ → Home AI**).

## Try it locally

```bash
go build -o bin/ ./cmd/...
```

```bash
bin/signal-dev -addr 0.0.0.0:8090
```

In another terminal, start the home server. `-llm mock` echoes your message back, so no model
is needed yet:

```bash
bin/homellm -signal ws://127.0.0.1:8090/ws -public-signal ws://<this-PC-LAN-IP>:8090/ws -llm mock
```

`-public-signal` is the URL put in the QR code, i.e. how the **phone** reaches signalling. Use
`ws://10.0.2.2:8090/ws` for the Android emulator.

Type `p` + Enter to print a pairing QR, scan it in the app, then type `y` to approve.

Without a phone:

```bash
bin/homellm -signal ws://127.0.0.1:8090/ws -llm mock -auto-approve -pair -qr-file qr.json
```

```bash
bin/testclient -qr-file qr.json -user alice -prompt "hello" -image photo.jpg
```

### With a real model

Install [Ollama](https://ollama.com), then:

```bash
ollama pull qwen2.5vl:7b
```

```bash
bin/homellm -signal ws://127.0.0.1:8090/ws -model qwen2.5vl:7b
```

Any OpenAI-compatible server works through `-llm-url` (llama.cpp `llama-server`, LM Studio,
vLLM). Pass `-vision=false` for text-only models; the app then hides its attach button.

## Running on Windows as a background service

Build on any machine, then copy `homellm.exe.new` and `deploy/install-windows.ps1` into one
folder on the PC (e.g. `C:\Users\<you>\HomeLLM`):

```bash
GOOS=windows GOARCH=amd64 go build -o homellm.exe.new ./cmd/homellm
```

Then run this in an elevated PowerShell on the PC. Re-running it later updates a running
install.

```powershell
powershell -ExecutionPolicy Bypass -File install-windows.ps1 -SignalUrl ws://<signalling-host>:8090/ws -Model gemma3:12b
```

The installer registers a scheduled task named `HomeLLM`. It starts at boot (no login needed,
no window) and restarts the server if it exits. Output goes to `homellm.log` next to the exe.
The service has no console, so to pair a phone, run `homellm.exe pair` in any terminal on the
PC. It shows the QR and asks you to approve in that terminal, using a control port the service
only listens on locally (`127.0.0.1:8091`).

Keep the PC awake (Windows sleeps after 15 minutes by default) and consider
`OLLAMA_KEEP_ALIVE=-1`. Otherwise Ollama unloads the model after 5 idle minutes, and the next
request waits a minute or more while it loads again.

## Data channel protocol

The phone creates two ordered, reliable channels:

- **`chat`**: `hello`; `request {id, messages[{role, content, image_ids}]}`; replies stream as
  `chunk {id, delta}` and end with `done {id}` or `error {id, message}`; `cancel {id}` stops
  generation.
- **`files`**: `file_start {id, mime, size, sha256}`, 16 KiB binary chunks, then `file_end {id}`.
  It's a separate channel so an image doesn't hold up streamed tokens. The phone limits how much
  data it queues (`bufferedAmount`) and downscales photos to 1280 px first.

## Before production

The dev signalling server is **not** safe to expose: it trusts phone tokens without verifying
them, and trusts a device's secret the first time it sees it. To go live:

- **Signalling backend.** The cheapest fit for this project is the same setup as
  `StreamerInfraStructure/lambda-videochat`: API Gateway WebSocket + Lambda + DynamoDB, with the
  Cognito token verified at `$connect`. Only a few messages per connection go through it, so it
  costs a few cents. The message protocol in `protocol/` stays unchanged.
- **TURN fallback.** Some networks (strict mobile carriers, corporate Wi-Fi) block direct paths.
  Add a TURN server with `-turn` (Cloudflare and Metered offer free tiers) and the same
  `IceServer` on Android. Traffic through TURN is still end-to-end encrypted.
- **Reconnecting.** If the phone switches networks, the connection drops and the app shows "tap
  to reconnect". Automatic ICE restart is a later improvement.
- **Run as a service.** Use launchd or systemd, disable sleep, and keep `~/.homellm/identity.json`
  safe. Deleting it means every phone has to pair again.
- **Phone identity on the home side.** The home server trusts signalling to say which user a
  session belongs to. To remove that trust too, phones could register their own key at pairing.

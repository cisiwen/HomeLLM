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
| `web/` | The web client at https://homellm.eiuoo.com (see below) |
| `cmd/studio` | HomeLLM Studio: a desktop playground for the PC's video and image models (see below) |
| `cmd/tunnel-home`, `cmd/tunnel-client` | Forward a PC's localhost TCP ports over the same scheme (see "Tunnel" below) |
| `internal/tunnel/` | The tunnel's forwarding logic |
| `deploy/` | Windows installers (HomeLLM, tunnel, ComfyUI, Foley, Studio); `deploy/web-cdk` hosts the web client |

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

### Restyling photos (cartoonise)

`-restyle comfyui` lets the phone redraw a photo it sent in another style: a cartoon by
default, or any style it names ("watercolour", "3D animated film", "anime"). It uses
[FLUX.1 Kontext [dev]](https://huggingface.co/Comfy-Org/flux1-kontext-dev_ComfyUI), an
instruction-following editor that keeps the people and composition, in the same ComfyUI as
`-images`. Put Comfy-Org's repackaged files in ComfyUI's `models` folder:

| File | Folder |
|---|---|
| `flux1-dev-kontext_fp8_scaled.safetensors` | `models/diffusion_models` |
| `clip_l.safetensors`, `t5xxl_fp8_e4m3fn_scaled.safetensors` | `models/text_encoders` |
| `ae.safetensors` | `models/vae` |

Check it and time it on the PC before turning it on (about 12 GB of VRAM; 20 steps):

```bash
homellm restyle -image photo.jpg
```

```bash
homellm restyle -image photo.jpg watercolour painting
```

Without a GPU, `-restyle mock` posterizes the photo, and `testclient -image photo.jpg -restyle
[-style watercolour] [-enhance]` drives the whole request. The phone sends
`restyle {id, image_id, prompt?, enhance?, seed?}` and gets the same `image_prompt`,
`image_progress` and `image_done` replies as `imagine`. `hello` says `restyle: true` when it's
on.

## Web client: homellm.eiuoo.com

`web/` is the browser version of the app's Home AI: chat, pictures, photo → video and
restyle. It's plain static files (no build step) hosted by `HomeLlmWebStack`, the CDK app in
`deploy/web-cdk` (S3 + CloudFront + Route 53). It also publishes Studio's page and
video-dubbing's (checked out next to this repo), so deploy from a machine with both:

```bash
cd deploy/web-cdk && npm ci && npx cdk deploy HomeLlmWebStack
```

It signs in to the same Cognito user pool as the app (SRP, in the browser, so no hosted UI
or callback URLs), lists PCs and sets up connections through the same signalling server, and
then talks to the PC directly over WebRTC exactly like the phone, including checking every
answer's signature against the PC's key. `web/config.js` holds the (public) pool, client and
signalling URL.

**Pairing with a typed code.** A browser can't scan the QR, so it pairs with a short code:

```bash
homellm code        # prints e.g. 7KQM2-XRD4P; valid once, for 10 minutes
```

(or the **HomeLLM pairing code** shortcut on the PC's desktop, which runs `homellm code -wait`,
or **Pairing code** in HomeLLM Studio). Typing the code is the approval, so there's no y/N
prompt. The code never travels in plain form: signalling sees only a hash, and the PC proves
its public key with an HMAC keyed by the code, which the browser checks
(`protocol/paircode.go`). The QR's JSON can still be pasted instead.

**Web apps on the PC (`/dubbing/`).** HomeLLM can also carry a browser's web requests to apps
running on the PC itself, so they work from anywhere with nothing exposed to the internet:

```bash
homellm ... -app dubbing=http://127.0.0.1:8000   # repeatable; loopback URLs only
```

hello lists the app names, and the client opens a third data channel, `http`, that multiplexes
requests with credit-based flow control, so a long video only streams as fast as the browser
reads it (`protocol/httprelay.go`, `internal/home/httprelay.go`). `/dubbing/` (the video-dubbing repo's
studio) and `/studio/` (HomeLLM Studio) use it: CloudFront serves each app's page at `/<app>/app/`,
the site's service worker (`web/sw.js`) hands that page's server requests to the `/<app>/` bridge
page (`web/apps/bridge.js`), and the bridge relays them to the PC. Throughput is whatever the PC can upload: on its
Wi-Fi that's currently about 0.4 MB/s, so long videos take a few seconds to seek.

 browsers hide their local IP behind a `.local` name, so the PC has to
accept the browser's incoming connection, which Windows Firewall blocks on a "Public" network
unless `homellm.exe` has an inbound rule (phones share their real IP, so they don't need one).
Away from home the browser connects like the phone does.

## HomeLLM Studio: learning image-to-video and text-to-video

`cmd/studio` is a desktop playground for the home PC's ComfyUI models. It's for learning what
the settings do: every control is exposed and explained, runs can sweep one setting, and each
result is saved with its settings and timings so runs can be compared side by side.

- **Image → Video**: Wan 2.2 14B i2v (the same models the phone uses).
- **Text → Video**: Wan 2.2 14B t2v with the lightx2v 4-step LoRAs.
- **Text → Image → Video**: FLUX schnell draws frame 1, then Wan animates it, in one job.
- **Text → Image**: FLUX schnell, to make start frames.

It runs on the Mac, reaches ComfyUI (and Ollama, to unload the LLM) through an SSH tunnel to the
PC, and saves runs in `~/HomeLLM Studio/runs/<id>/` (`meta.json` + the files). Build it as an app:

```bash
deploy/build-studio-mac.sh cisiwen@192.168.1.120   # -> ~/Applications/HomeLLM Studio.app
```

It also runs on the PC itself, for use from anywhere at https://homellm.eiuoo.com/studio/: build
`homellm-studio.exe` (`GOOS=windows go build -o homellm-studio.exe ./cmd/studio`), run
`deploy/install-studio-windows.ps1` next to it (a boot task on 127.0.0.1:8777, runs in
`D:\HomeLLM-Studio`), and start HomeLLM with `-app studio=http://127.0.0.1:8777`. Progress comes
from `/api/events` (server-sent events) rather than ComfyUI's WebSocket, so it works through the
relay; runs made there are kept on the PC, separate from the Mac app's.

Or run it directly with `go run ./cmd/studio -host user@pc` (`-comfy http://127.0.0.1:8188`
on the PC itself). `ssh user@pc` must work without a password prompt.

Text-to-video needs four more files in ComfyUI's models folders (about 31 GB):
`wan2.2_t2v_{high,low}_noise_14B_fp8_scaled.safetensors` (diffusion_models) and
`wan2.2_t2v_lightx2v_4steps_lora_v1.1_{high,low}_noise.safetensors` (loras), from
[Comfy-Org/Wan-2.2](https://huggingface.co/Comfy-Org/Wan-2.2/tree/main/split_files).

ComfyUI keeps models in RAM between jobs. Wan i2v, Wan t2v and FLUX all together are more than
the PC's 96 GB, and ComfyUI crashes. So the studio asks ComfyUI to drop its models before a job
that needs different ones.

## Production signalling

The real signalling server is `HomeLlmStack` in `StreamerInfraStructure/cdk`
(`lambda-homellm`: API Gateway WebSocket + Lambda + DynamoDB). It checks each home server's
device secret and each phone's Cognito ID token at `$connect`. The message protocol is the
same as `signal-dev`'s, so nothing here changes between the two.

| Value | Current deployment (AWS account 323455446472, ap-southeast-2) |
|---|---|
| Signalling URL (`-signal`) | `wss://it8qx5mjlk.execute-api.ap-southeast-2.amazonaws.com/prod` |
| Cognito user pool (`-cognito-pool`) | `ap-southeast-2_z1WRw9nUz` |
| Cognito app client (`-cognito-client`) | `27ubaplgpe7r5gli1fmnp737kf` |

These come from the stacks' outputs (`HomeLlmStack.HomeLlmWebSocketUrl`, and
`CognitoInfraStack.UserPoolId`/`UserPoolClientId`). Check there if they ever change: a stack
rebuilt from scratch gets a new URL or IDs.

**Moving to a new deployment** (as happened when the previous account was suspended): re-run
the installer with the new `-SignalUrl`. The PC can keep its `data\identity.json`, since a new
signalling backend registers a device the first time it connects. Pairings are stored by the
signalling backend, though, and each phone saved the old URL from its QR code, so remove the
PC in the app and pair again (`homellm.exe pair`). Users also have to sign up again if the
Cognito user pool is new.

## Running on Windows as a background service

Build on any machine, then copy `homellm.exe.new` and `deploy/install-windows.ps1` into one
folder on the PC (e.g. `C:\Users\<you>\HomeLLM`):

```bash
GOOS=windows GOARCH=amd64 go build -o homellm.exe.new ./cmd/homellm
```

Then run this in an elevated PowerShell on the PC. Re-running it later updates a running
install, including switching it to a new signalling URL.

```powershell
powershell -ExecutionPolicy Bypass -File install-windows.ps1 -SignalUrl wss://it8qx5mjlk.execute-api.ap-southeast-2.amazonaws.com/prod -Model gemma3:12b
```

For testing against a local `signal-dev` instead, pass `-SignalUrl ws://<signalling-host>:8090/ws`.

The installer registers a scheduled task named `HomeLLM`. It starts at boot (no login needed,
no window) and restarts the server if it exits. Output goes to `homellm.log` next to the exe.
The service has no console, so to pair a phone, run `homellm.exe pair` in any terminal on the
PC. It shows the QR and asks you to approve in that terminal, using a control port the service
only listens on locally (`127.0.0.1:8091`).

Keep the PC awake (Windows sleeps after 15 minutes by default) and set `OLLAMA_KEEP_ALIVE` to
suit you. Ollama unloads the model after that much idle time (5 minutes by default), and the
next request waits while it loads again: about 10 s, or a minute or more on the first load after
a reboot. `30m` frees the GPU when you're not chatting without pausing a conversation; `-1`
keeps the model loaded permanently. Restart Ollama afterwards (`deploy/restart-ollama.ps1`).

## Tunnel: reach a PC's localhost services from anywhere

`tunnel-home` (on the PC) and `tunnel-client` (on the machine you work from) forward TCP over
the same signalling, pairing and signed-answer scheme, so something that listens only on
`127.0.0.1` on the PC (e.g. the windows-mcp server on 8000) is usable from any network with no
open ports. Only ports on `-ports` are ever forwarded, and only to `127.0.0.1`.

```
Your Mac (any network)              AWS account                      Home PC
┌─────────────────────────┐         ┌──────────────────────┐         ┌─────────────────────────┐
│ Claude Code (MCP client)│         │ Cognito              │         │ windows-mcp             │
│        │                │         │   │ verifies token   │         │ 127.0.0.1:8000 + key    │
│        ▼                │         │   ▼                  │         │        ▲                │
│ tunnel-client           │─sign in▶│ HomeLlmStack         │◀connect─│ tunnel-home             │
│ 127.0.0.1:18000         │         │ (signalling API)     │         │ (boot-time task)        │
└────────┬────────────────┘         └──────────────────────┘         └────────┬────────────────┘
         │                                                                    │
         └────────────── direct WebRTC data channel (encrypted) ──────────────┘
                  STUN for NAT traversal; TURN relay if UDP is blocked
```

1. `tunnel-client` signs in to Cognito and both tunnel programs connect **out** to the signalling
   API, so the PC needs no inbound ports.
2. They swap one WebRTC offer/answer through signalling; the answer is signed with the PC's
   Ed25519 key, which the client pinned when it paired.
3. After that the traffic goes directly between them over an encrypted data channel (one channel
   per forwarded TCP connection) and never touches AWS.

```bash
go build -o bin/ ./cmd/tunnel-home ./cmd/tunnel-client
```

On the PC (service stopped), pair once, then run `deploy/install-tunnel-windows.ps1`:

```bash
tunnel-home -signal wss://it8qx5mjlk.execute-api.ap-southeast-2.amazonaws.com/prod -ports 8000 -pair
```

On the client, with the blob it printed. Against the real signalling use Cognito sign-in (values
from "Production signalling" above); the dev server (`signal-dev`) takes `-token <any user id>`
instead:

```bash
tunnel-client pair -qr '<blob>' -cognito-pool ap-southeast-2_z1WRw9nUz -cognito-client 27ubaplgpe7r5gli1fmnp737kf -username you@example.com
```

```bash
tunnel-client serve -listen 127.0.0.1:8000 -port 8000 -cognito-pool ap-southeast-2_z1WRw9nUz -cognito-client 27ubaplgpe7r5gli1fmnp737kf -username you@example.com
```

The password comes from `$TUNNEL_PASSWORD` or a prompt. If the client's network blocks UDP,
pass `-turn turn:host:3478 -turn-user ... -turn-pass ...` to both sides.

## Data channel protocol

The phone creates two ordered, reliable channels:

- **`chat`**: `hello`; `request {id, messages[{role, content, image_ids}]}`; replies stream as
  `chunk {id, delta}` and end with `done {id}` or `error {id, message}`; `cancel {id}` stops
  generation.
- **`files`**: `file_start {id, mime, size, sha256}`, 16 KiB binary chunks, then `file_end {id}`.
  It's a separate channel so an image doesn't hold up streamed tokens. The phone limits how much
  data it queues (`bufferedAmount`) and downscales photos to 1280 px first.

## Production notes

The dev signalling server is **not** safe to expose: it trusts phone tokens without verifying
them. Use the production backend instead (see "Production signalling" above): it verifies each
phone's Cognito ID token at `$connect`, and only a few messages per connection go through it, so
it costs a few cents. Like the dev server, it registers a home server's device ID and secret
the first time that ID connects, and checks the secret on every later connection.

Still open:

- **TURN fallback.** Some networks (strict mobile carriers, corporate Wi-Fi) block direct paths.
  Add a TURN server with `-turn` (Cloudflare and Metered offer free tiers) and the same
  `IceServer` on Android. Traffic through TURN is still end-to-end encrypted.
- **Reconnecting.** If the phone switches networks, the connection drops and the app shows "tap
  to reconnect". Automatic ICE restart is a later improvement.
- **Run as a service.** Use launchd or systemd, disable sleep, and keep `~/.homellm/identity.json`
  safe. Deleting it means every phone has to pair again.
- **Phone identity on the home side.** The home server trusts signalling to say which user a
  session belongs to. To remove that trust too, phones could register their own key at pairing.

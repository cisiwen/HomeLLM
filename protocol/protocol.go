// Package protocol defines every message HomeLLM exchanges: the signalling WebSocket (used only
// to pair devices and to swap a WebRTC offer/answer) and the two WebRTC data channels that carry
// all actual LLM traffic directly between the phone and the home server.
//
// The Android client (AndroidStreamer's homellm package) mirrors these shapes by hand, so any
// change here must be made there too.
package protocol

import (
	"crypto/sha256"
	"encoding/json"
)

// Signal is one signalling-WebSocket message. Every message carries an "action" (the same
// convention lambda-videochat uses, so a production API Gateway backend can route on it); the
// remaining fields are a flat union, and each action documents which ones it uses.
type Signal struct {
	Action string `json:"action"`

	// Pairing.
	DeviceID  string `json:"device_id,omitempty"`
	Code      string `json:"code,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // unix seconds
	RequestID string `json:"request_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Approved  bool   `json:"approved,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	Name      string `json:"name,omitempty"`

	// WebRTC negotiation, relayed opaquely by the signalling server. Session identifies one
	// phone-side peer connection, so a home server can serve several phones at once.
	Session string          `json:"session,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`

	Devices []Device `json:"devices,omitempty"`
	Message string   `json:"message,omitempty"`

	// On "replaced": the instance ID of the connection that replaced this one.
	Instance string `json:"instance,omitempty"`
}

// Signalling actions.
const (
	// home -> server: {code, expires_at}. Registers a one-time pairing code shown in a QR.
	ActionPairOpen = "pair_open"
	// phone -> server: {device_id, code}. Redeems a scanned QR code.
	ActionPair = "pair"
	// server -> home: {request_id, user_id}. Asks the home owner to approve a pairing.
	ActionPairRequest = "pair_request"
	// home -> server: {request_id, approved}.
	ActionPairResponse = "pair_response"
	// server -> phone: {ok, device_id, name, message}.
	ActionPairResult = "pair_result"
	// phone -> server: {}. server -> phone: {devices}. Lists the caller's paired devices.
	ActionDevices = "devices"
	// phone -> server: {device_id, session, payload}. server -> home: {session, user_id, payload}.
	// home -> server: {session, payload}. server -> phone: {device_id, session, payload}.
	ActionSignal = "signal"
	// server -> either: {message, session?}.
	ActionError = "error"
	// home -> server: {}. server -> home: "pong" {}. An application-level keepalive: API
	// Gateway closes WebSockets idle for 10 minutes, and WebSocket ping frames don't reliably
	// reach the backend there, so a real message is the portable way to keep alive and to
	// notice a dead link.
	ActionPing = "ping"
	ActionPong = "pong"
	// server -> home: {instance, message}. Sent just before the server drops this connection
	// because the same device connected again. If instance is this process's own, it was just
	// its own reconnect racing; otherwise a second copy of the server is running.
	ActionReplaced = "replaced"
)

// Connection parameters. A home server sends its device secret in the HeaderDeviceSecret
// header and a phone its Cognito ID token as "Authorization: Bearer <token>", never in the
// URL, which ends up in access logs and error messages.
const HeaderDeviceSecret = "X-Device-Secret"

// Device is one paired home server, as listed to a phone.
type Device struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
}

// Negotiation is the opaque Signal.Payload. ICE is non-trickle: each side waits for candidate
// gathering to finish and sends one complete SDP, so the signalling server only ever relays two
// messages per connection.
type Negotiation struct {
	Type string `json:"type"` // "offer" or "answer"
	SDP  string `json:"sdp"`
	// Sig, on an answer only: base64 Ed25519 signature over AnswerSigningBytes, made with the
	// home server's identity key. The phone got the matching public key from the pairing QR,
	// so a compromised signalling server can't substitute its own DTLS fingerprint and sit in
	// the middle of the connection.
	Sig string `json:"sig,omitempty"`
}

// AnswerSigningBytes is what the home server signs for an answer. Binding the offer in too
// proves the answer was made for this phone's offer (and so its DTLS fingerprint), not replayed.
func AnswerSigningBytes(offerSDP, answerSDP string) []byte {
	o := sha256.Sum256([]byte(offerSDP))
	a := sha256.Sum256([]byte(answerSDP))
	out := []byte("homellm-answer-v1\x00")
	out = append(out, o[:]...)
	return append(out, a[:]...)
}

// PairingQR is the JSON encoded in the pairing QR code.
type PairingQR struct {
	V        int    `json:"v"`
	Signal   string `json:"signal"` // signalling WebSocket URL
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Code     string `json:"code"`
	Pub      string `json:"pub"` // base64 Ed25519 public key
}

// Data channel labels. The phone (the offerer) creates both; both are ordered and reliable.
// Files get their own channel so a large image doesn't hold up streamed tokens behind it.
const (
	ChannelChat  = "chat"
	ChannelFiles = "files"
)

// Peer is one text message on either data channel.
type Peer struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`

	// chat: request
	Model    string        `json:"model,omitempty"`
	Messages []ChatMessage `json:"messages,omitempty"`
	// chat: chunk
	Delta string `json:"delta,omitempty"`
	// chat: error / files: file_error
	Message string `json:"message,omitempty"`
	// chat: hello reply. Images says this server can generate images (PeerImagine), Videos
	// that it can animate them (PeerAnimate).
	Name   string `json:"name,omitempty"`
	Vision bool   `json:"vision,omitempty"`
	Images bool   `json:"images,omitempty"`
	Videos bool   `json:"videos,omitempty"`

	// chat: imagine / image_prompt / image_progress / image_done
	Prompt  string `json:"prompt,omitempty"`
	Enhance bool   `json:"enhance,omitempty"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Seed    int64  `json:"seed,omitempty"`
	Step    int    `json:"step,omitempty"`
	Steps   int    `json:"steps,omitempty"`
	FileID  string `json:"file_id,omitempty"`
	// chat: animate / video_done
	ImageID string `json:"image_id,omitempty"`
	Frames  int    `json:"frames,omitempty"`
	FPS     int    `json:"fps,omitempty"`

	// files: file_start
	MIME   string `json:"mime,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"` // hex
}

// Data channel message types.
const (
	// chat, phone -> home: {}. home -> phone: {name, model, vision}.
	PeerHello = "hello"
	// chat, phone -> home: {id, model?, messages}.
	PeerRequest = "request"
	// chat, home -> phone: {id, delta}.
	PeerChunk = "chunk"
	// chat, home -> phone: {id}.
	PeerDone = "done"
	// chat, home -> phone: {id, message}.
	PeerError = "error"
	// chat, phone -> home: {id}. Stops generation (text or image).
	PeerCancel = "cancel"

	// chat, phone -> home: {id, prompt, enhance?, width?, height?, seed?}. Generates an image
	// from a text prompt; enhance first has the LLM rewrite a short idea into a detailed
	// English prompt. Failures come back as error {id, message}.
	PeerImagine = "imagine"
	// chat, home -> phone: {id, prompt}. The prompt actually used, when enhance rewrote it.
	PeerImagePrompt = "image_prompt"
	// chat, home -> phone: {id, step, steps}. Sampling progress.
	PeerImageProgress = "image_progress"
	// chat, home -> phone: {id, file_id, width, height, seed}. The image itself arrives on the
	// files channel as file_id - possibly before or after this message, since the two
	// channels are independent streams.
	PeerImageDone = "image_done"

	// chat, phone -> home: {id, image_id, prompt, frames?, seed?}. Animates an image already
	// sent on the files channel into a short clip; prompt describes the motion. Progress comes
	// as image_progress. Unlike other requests, a clip keeps rendering if the phone
	// disconnects: the result is held and sent when that user next says hello.
	PeerAnimate = "animate"
	// chat, home -> phone: {id, file_id, width, height, frames, fps, seed}. The clip
	// (video/mp4) arrives on the files channel as file_id, in either order.
	PeerVideoDone = "video_done"

	// files, sender: {id, mime, size, sha256}, then binary chunks, then file_end {id}. Only one
	// file is in flight per direction at a time, so the binary chunks need no header. Phones
	// send photos this way; the home server sends generated images.
	PeerFileStart = "file_start"
	PeerFileEnd   = "file_end"
	// files, receiver -> sender: {id, message}.
	PeerFileError = "file_error"
)

// ChatMessage is one conversation turn. ImageIDs refer to files already sent on the files
// channel; the home server swaps them for the image bytes before calling the LLM.
type ChatMessage struct {
	Role     string   `json:"role"`
	Content  string   `json:"content"`
	ImageIDs []string `json:"image_ids,omitempty"`
}

// FileChunkSize is the binary chunk size for the files channel. 16 KiB stays well inside every
// WebRTC implementation's message size limit.
const FileChunkSize = 16 * 1024

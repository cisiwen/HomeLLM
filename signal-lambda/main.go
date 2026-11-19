// signal-lambda is the signalling backend for HomeLLM: it lets the
// AndroidStreamer app pair with, and open direct WebRTC connections to, an LLM server running
// on the user's own home computer. Like lambda-videochat it never sees any actual traffic -
// prompts, replies and images all go over the direct WebRTC data channels - only pairing and
// one SDP offer/answer per connection pass through here.
//
// It's the production counterpart of internal/signaldev (local dev server): the message
// protocol (protocol/protocol.go) is identical, so neither the home server nor the app knows
// which one it's talking to. Deployed as HomeLlmStack (deploy/cdk/lib/homellm-stack.ts). Its
// own Go module (AWS SDK dependencies the home server doesn't need), so the message types are
// mirrored here rather than imported.
//
// Routes: $connect (authenticate a home server by device secret, or a phone by Cognito ID
// token), $disconnect, and $default, which dispatches on the message's "action" field.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// signal mirrors HomeLLM's protocol.Signal - one flat union of every action's fields.
type signal struct {
	Action    string          `json:"action"`
	DeviceID  string          `json:"device_id,omitempty"`
	Code      string          `json:"code,omitempty"`
	ExpiresAt int64           `json:"expires_at,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	UserID    string          `json:"user_id,omitempty"`
	Approved  bool            `json:"approved,omitempty"`
	OK        bool            `json:"ok,omitempty"`
	Name      string          `json:"name,omitempty"`
	Instance  string          `json:"instance,omitempty"`
	Session   string          `json:"session,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Devices   []deviceInfo    `json:"devices,omitempty"`
	Message   string          `json:"message,omitempty"`
}

type deviceInfo struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
}

var (
	awsCfg aws.Config
	db     *store
)

func init() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("loading AWS config: %v", err)
	}
	awsCfg = cfg
	db = &store{ddb: dynamodb.NewFromConfig(cfg)}
}

func handler(ctx context.Context, req events.APIGatewayWebsocketProxyRequest) (events.APIGatewayProxyResponse, error) {
	r := &request{ctx: ctx, req: req, connID: req.RequestContext.ConnectionID}
	r.mgmt = apigatewaymanagementapi.NewFromConfig(awsCfg, func(o *apigatewaymanagementapi.Options) {
		o.BaseEndpoint = aws.String("https://" + req.RequestContext.DomainName + "/" + req.RequestContext.Stage)
	})
	switch req.RequestContext.RouteKey {
	case "$connect":
		return r.connect(), nil
	case "$disconnect":
		r.disconnect()
		return ok(), nil
	default:
		return r.message(), nil
	}
}

type request struct {
	ctx    context.Context
	req    events.APIGatewayWebsocketProxyRequest
	connID string
	mgmt   *apigatewaymanagementapi.Client
}

func (r *request) header(name string) string {
	for k, v := range r.req.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// --- $connect / $disconnect ---

func (r *request) connect() events.APIGatewayProxyResponse {
	q := r.req.QueryStringParameters
	expires := time.Now().Add(connectionTTL).Unix()
	switch q["role"] {
	case "home":
		deviceID, instance := q["device_id"], q["instance"]
		secret := r.header("X-Device-Secret")
		if deviceID == "" || len(secret) < 16 {
			return fail(http.StatusUnauthorized, "device_id and X-Device-Secret required")
		}
		d, err := db.getDevice(r.ctx, deviceID)
		if err != nil {
			return internal("getDevice", err)
		}
		if d == nil {
			// First connection registers the device. That grants nothing by itself: a phone
			// only gets access after scanning a QR this device shows and its owner approving.
			d = &device{DeviceID: deviceID, SecretHash: hashHex(secret), Name: q["name"], LastSeen: time.Now().Unix()}
			if err := db.createDevice(r.ctx, d); err != nil {
				return fail(http.StatusUnauthorized, "device registration failed")
			}
		} else if subtle.ConstantTimeCompare([]byte(d.SecretHash), []byte(hashHex(secret))) != 1 {
			return fail(http.StatusUnauthorized, "bad device secret")
		}
		if err := db.putState(r.ctx, &state{PK: "conn#" + r.connID, Role: "home", DeviceID: deviceID, ExpiresAt: expires}); err != nil {
			return internal("putState", err)
		}
		if err := db.setDeviceConnection(r.ctx, deviceID, r.connID, instance, q["name"]); err != nil {
			return internal("setDeviceConnection", err)
		}
		if old := d.ConnectionID; old != "" && old != r.connID {
			// Tell the replaced connection which instance replaced it: the same process (its
			// own reconnect) just reconnects, a different one means a second copy is running.
			r.post(old, signal{Action: "replaced", Instance: instance, Message: "replaced by a newer connection"})
			r.mgmt.DeleteConnection(r.ctx, &apigatewaymanagementapi.DeleteConnectionInput{ConnectionId: aws.String(old)})
			db.deleteState(r.ctx, "conn#"+old)
		}
		log.Printf("connect home device=%s", deviceID)

	case "phone":
		token := bearerToken(r.header("Authorization"))
		if token == "" {
			token = q["idToken"]
		}
		sub, err := authenticate(token)
		if err != nil {
			log.Printf("connect phone rejected: %v", err)
			return fail(http.StatusUnauthorized, "unauthorized")
		}
		if err := db.putState(r.ctx, &state{PK: "conn#" + r.connID, Role: "phone", UserID: sub, ExpiresAt: expires}); err != nil {
			return internal("putState", err)
		}
		log.Printf("connect phone user=%s", sub)

	default:
		return fail(http.StatusBadRequest, "role must be home or phone")
	}
	return ok()
}

func (r *request) disconnect() {
	st, err := db.takeState(r.ctx, "conn#"+r.connID)
	if err != nil || st == nil {
		return
	}
	if st.Role == "home" {
		db.clearDeviceConnection(r.ctx, st.DeviceID, r.connID)
	}
}

// --- messages ---

func (r *request) message() events.APIGatewayProxyResponse {
	var msg signal
	if err := json.Unmarshal([]byte(r.req.Body), &msg); err != nil {
		return fail(http.StatusBadRequest, "bad json")
	}
	me, err := db.getState(r.ctx, "conn#"+r.connID)
	if err != nil {
		return internal("getState", err)
	}
	if me == nil {
		return fail(http.StatusForbidden, "unknown connection")
	}
	if msg.Action == "ping" {
		r.reply(signal{Action: "pong"})
		return ok()
	}
	if me.Role == "home" {
		r.fromHome(me, msg)
	} else {
		r.fromPhone(me, msg)
	}
	return ok()
}

func (r *request) fromHome(me *state, msg signal) {
	switch msg.Action {
	case "pair_open":
		if len(msg.Code) < 16 {
			r.reply(errorSignal("pairing code too short", ""))
			return
		}
		expires := min(msg.ExpiresAt, time.Now().Add(maxCodeTTL).Unix())
		if err := db.putState(r.ctx, &state{PK: "code#" + hashHex(msg.Code), DeviceID: me.DeviceID, ExpiresAt: expires}); err != nil {
			log.Printf("pair_open: %v", err)
			r.reply(errorSignal("could not register pairing code", ""))
		}

	case "pair_response":
		req, err := db.takeState(r.ctx, "req#"+msg.RequestID)
		if err != nil || req == nil || req.DeviceID != me.DeviceID {
			return
		}
		// payload is the home server's key proof for a typed-code pairing (HomeLLM's
		// protocol/paircode.go); passed through untouched.
		res := signal{Action: "pair_result", DeviceID: me.DeviceID, OK: msg.Approved, Payload: msg.Payload}
		if msg.Approved {
			if err := db.grantAccess(r.ctx, me.DeviceID, req.UserID); err != nil {
				log.Printf("grantAccess: %v", err)
				res.OK, res.Message = false, "could not save the pairing"
			}
			if d, _ := db.getDevice(r.ctx, me.DeviceID); d != nil {
				res.Name = d.Name
			}
		} else {
			res.Message = "pairing was rejected on the home server"
		}
		r.post(req.ConnectionID, res)

	case "signal":
		sess, err := db.getState(r.ctx, "sess#"+msg.Session)
		if err != nil || sess == nil || sess.DeviceID != me.DeviceID {
			r.reply(errorSignal("unknown session", msg.Session))
			return
		}
		r.post(sess.ConnectionID, signal{Action: "signal", DeviceID: me.DeviceID, Session: msg.Session, Payload: msg.Payload})
	}
}

func (r *request) fromPhone(me *state, msg signal) {
	switch msg.Action {
	case "pair":
		r.startPairing(me, msg)

	case "devices":
		ids, err := db.userDevices(r.ctx, me.UserID)
		if err != nil {
			log.Printf("userDevices: %v", err)
			r.reply(errorSignal("could not list devices", ""))
			return
		}
		list := []deviceInfo{}
		for _, id := range ids {
			if d, _ := db.getDevice(r.ctx, id); d != nil {
				list = append(list, deviceInfo{DeviceID: id, Name: d.Name, Online: d.ConnectionID != ""})
			}
		}
		r.reply(signal{Action: "devices", Devices: list})

	case "signal":
		if msg.Session == "" {
			r.reply(errorSignal("session required", ""))
			return
		}
		allowed, err := db.hasAccess(r.ctx, msg.DeviceID, me.UserID)
		if err != nil || !allowed {
			// Same reply as "no such device", so device IDs can't be probed.
			r.reply(errorSignal("device not found", msg.Session))
			return
		}
		d, err := db.getDevice(r.ctx, msg.DeviceID)
		if err != nil || d == nil || d.ConnectionID == "" {
			r.reply(errorSignal("home server is offline", msg.Session))
			return
		}
		if err := db.claimSession(r.ctx, msg.Session, r.connID, msg.DeviceID, me.UserID); err != nil {
			r.reply(errorSignal(err.Error(), msg.Session))
			return
		}
		if !r.post(d.ConnectionID, signal{Action: "signal", Session: msg.Session, UserID: me.UserID, Payload: msg.Payload}) {
			db.clearDeviceConnection(r.ctx, msg.DeviceID, d.ConnectionID)
			r.reply(errorSignal("home server is offline", msg.Session))
		}
	}
}

func (r *request) startPairing(me *state, msg signal) {
	fail := func(m string) {
		r.reply(signal{Action: "pair_result", DeviceID: msg.DeviceID, Message: m})
	}
	// Single use: consumed by the first attempt, right or wrong.
	code, err := db.takeState(r.ctx, "code#"+hashHex(msg.Code))
	// A typed code (HomeLLM's protocol/paircode.go, "pc2-" lookups) says which device it
	// belongs to by itself: the user only typed the code.
	if err == nil && code != nil && msg.DeviceID == "" && strings.HasPrefix(msg.Code, "pc2-") {
		msg.DeviceID = code.DeviceID
	}
	if err != nil || code == nil || code.DeviceID != msg.DeviceID {
		fail("invalid or expired pairing code")
		return
	}
	d, err := db.getDevice(r.ctx, msg.DeviceID)
	if err != nil || d == nil || d.ConnectionID == "" {
		fail("home server is offline")
		return
	}
	reqID := randomID()
	if err := db.putState(r.ctx, &state{PK: "req#" + reqID, DeviceID: msg.DeviceID, UserID: me.UserID, ConnectionID: r.connID, ExpiresAt: time.Now().Add(approvalTTL).Unix()}); err != nil {
		fail("could not start pairing")
		return
	}
	// The phone's own timeout covers an owner who never answers; there's no timer here.
	// The code tells the home server which of its codes was used, so a typed one can be
	// approved without asking.
	if !r.post(d.ConnectionID, signal{Action: "pair_request", RequestID: reqID, UserID: me.UserID, Code: msg.Code}) {
		fail("home server is offline")
	}
}

// --- helpers ---

// post sends one message to a connection; false if it's gone.
func (r *request) post(connID string, s signal) bool {
	data, _ := json.Marshal(s)
	_, err := r.mgmt.PostToConnection(r.ctx, &apigatewaymanagementapi.PostToConnectionInput{ConnectionId: aws.String(connID), Data: data})
	if err != nil {
		var gone *types.GoneException
		if !errors.As(err, &gone) {
			log.Printf("PostToConnection %s: %v", s.Action, err)
		}
		return false
	}
	return true
}

func (r *request) reply(s signal) { r.post(r.connID, s) }

func errorSignal(message, session string) signal {
	return signal{Action: "error", Message: message, Session: session}
}

func ok() events.APIGatewayProxyResponse { return events.APIGatewayProxyResponse{StatusCode: http.StatusOK} }

func fail(status int, msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{StatusCode: status, Body: msg}
}

func internal(op string, err error) events.APIGatewayProxyResponse {
	log.Printf("%s: %v", op, err)
	return fail(http.StatusInternalServerError, "internal error")
}

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func main() {
	lambda.Start(handler)
}

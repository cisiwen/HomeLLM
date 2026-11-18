package home_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/wweng/homellm/internal/home"
	"github.com/wweng/homellm/internal/signaldev"
	"github.com/wweng/homellm/protocol"
)

// A typed code pairs without the owner's approval prompt and hands the client the PC's key,
// vouched for by the code; a wrong code is refused.
func TestPairWithTypedCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srv, err := signaldev.New("")
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	defer hs.Close()
	signalURL := "ws" + strings.TrimPrefix(hs.URL, "http")

	id, err := home.LoadOrCreateIdentity(t.TempDir(), "test-pc")
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	agent := home.NewAgent(home.Config{
		SignalURL: signalURL,
		Backend:   home.Mock{},
		Approve:   func(context.Context, string) bool { asked = true; return false },
	}, id)
	go agent.Run(ctx)
	for !agent.Connected() {
		time.Sleep(20 * time.Millisecond)
	}

	pair := func(typed string) protocol.Signal {
		t.Helper()
		h := http.Header{"Authorization": {"Bearer alice"}}
		ws, _, err := websocket.Dial(ctx, signalURL+"?role=phone", &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatal(err)
		}
		defer ws.CloseNow()
		code := protocol.NormalizePairCode(typed)
		if err := wsjson.Write(ctx, ws, protocol.Signal{Action: protocol.ActionPair, Code: protocol.PairCodeLookup(code)}); err != nil {
			t.Fatal(err)
		}
		for {
			var m protocol.Signal
			if err := wsjson.Read(ctx, ws, &m); err != nil {
				t.Fatal(err)
			}
			if m.Action == protocol.ActionPairResult {
				return m
			}
		}
	}

	if res := pair("ABCDE-FGHJK"); res.OK {
		t.Fatal("a code the PC never issued paired")
	}

	pc, err := agent.NewPairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Typed sloppily: lower case, spaces, O for 0 and so on are all accepted.
	typed := strings.ToLower(strings.ReplaceAll(pc.Code, "-", " "))
	typed = strings.NewReplacer("0", "o", "1", "l").Replace(typed)
	res := pair(typed)
	if !res.OK || res.DeviceID != id.DeviceID {
		t.Fatalf("pairing with the PC's code failed: %+v", res)
	}
	if asked {
		t.Error("a typed code should not need the approval prompt")
	}
	var proof protocol.PairCodeProof
	if err := json.Unmarshal(res.Payload, &proof); err != nil {
		t.Fatalf("pair_result payload: %v", err)
	}
	pub, ok := protocol.VerifyPairCodeProof(protocol.NormalizePairCode(pc.Code), res.DeviceID, proof)
	if !ok || !ed25519.PublicKey(pub).Equal(id.PrivateKey.Public()) {
		t.Fatal("the key proof doesn't check out")
	}
	if _, ok := protocol.VerifyPairCodeProof("ZZZZZZZZZZ", res.DeviceID, proof); ok {
		t.Fatal("the proof verified with the wrong code")
	}

	// Single use.
	if res := pair(pc.Code); res.OK {
		t.Fatal("a used code paired again")
	}
}

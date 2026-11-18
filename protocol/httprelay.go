package protocol

import "encoding/binary"

// Web apps on the PC, over the WebRTC connection.
//
// The home server can relay HTTP requests to web apps running on the PC itself (e.g. the
// dubbing studio on 127.0.0.1:8000), so a browser can use them from anywhere without anything
// being exposed to the internet. Only apps the owner listed (-app name=url) can be reached,
// and hello lists their names. The browser opens a third data channel, ChannelHTTP, and
// multiplexes requests on it, each numbered by the client:
//
//	client -> home: HTTPMsg{type: "req", n, app, method, path, headers, body}
//	                binary frames [n][bytes] with the request body, then {type: "req_end", n}
//	                {type: "credit", n, credit}: may send that many more response bytes
//	                {type: "cancel", n}: stop (the browser no longer wants the response)
//	home -> client: {type: "res", n, status, headers}
//	                binary frames [n][bytes] with the response body, then {type: "res_end", n}
//	                {type: "res_error", n, message} instead, if the request failed
//
// Response bodies only flow while the client has granted credit, so a video the browser has
// stopped reading (it seeks by cancelling and asking for another range) doesn't keep streaming.
// Binary frames start with n as 4 big-endian bytes.

// ChannelHTTP is the data channel label for relayed HTTP.
const ChannelHTTP = "http"

// HTTPMsg is one text message on ChannelHTTP.
type HTTPMsg struct {
	Type    string            `json:"type"`
	N       uint32            `json:"n"`
	App     string            `json:"app,omitempty"`
	Method  string            `json:"method,omitempty"`
	Path    string            `json:"path,omitempty"` // with the query string, starting with "/"
	Headers map[string]string `json:"headers,omitempty"`
	Body    bool              `json:"body,omitempty"` // req: body frames follow
	Status  int               `json:"status,omitempty"`
	Credit  int64             `json:"credit,omitempty"`
	Message string            `json:"message,omitempty"`
}

// HTTP relay message types.
const (
	HTTPReq      = "req"
	HTTPReqEnd   = "req_end"
	HTTPCancel   = "cancel"
	HTTPCredit   = "credit"
	HTTPRes      = "res"
	HTTPResEnd   = "res_end"
	HTTPResError = "res_error"
)

// HTTPFrame prefixes a body chunk with its request number.
func HTTPFrame(n uint32, data []byte) []byte {
	out := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(out, n)
	copy(out[4:], data)
	return out
}

// ParseHTTPFrame splits a binary frame into its request number and bytes.
func ParseHTTPFrame(frame []byte) (uint32, []byte, bool) {
	if len(frame) < 4 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint32(frame), frame[4:], true
}

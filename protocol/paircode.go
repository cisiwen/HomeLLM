package protocol

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Pairing with a typed code, for clients that can't scan the QR (the web client).
//
// The home server shows a short one-time code such as "7KQM2-XRD4P" (10 Crockford base32
// characters, 50 random bits). The user types it in, which both picks the home server and
// approves the pairing: only someone who can see the code on the PC can enter it. So, unlike
// a QR pairing, there's no y/N prompt.
//
// The code never travels in plain form. Signalling only ever sees PairCodeLookup(code), the
// value used as the pairing code in pair_open / pair / pair_request. The client also needs
// the home server's public key, which a QR carries but a typed code can't, so the home server
// sends it back in pair_response's payload (relayed in pair_result) as a PairCodeProof: the
// key plus an HMAC over it keyed by the code. A compromised signalling server could only swap
// in its own key by recovering the code from its lookup hash, which takes about 2^50 hashes,
// not something it can do within the code's lifetime.

// pairCodeAlphabet is Crockford's base32: no I, L, O or U, so codes read back unambiguously.
const pairCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// PairCodeLen is the number of code characters (without the dash).
const PairCodeLen = 10

// NewPairCode returns a fresh random code, formatted "XXXXX-XXXXX".
func NewPairCode() string {
	b := make([]byte, PairCodeLen)
	rand.Read(b)
	var sb strings.Builder
	for i, v := range b {
		if i == PairCodeLen/2 {
			sb.WriteByte('-')
		}
		sb.WriteByte(pairCodeAlphabet[v&31]) // 256 is a multiple of 32: no bias
	}
	return sb.String()
}

// NormalizePairCode upper-cases a typed code, drops spaces and dashes and maps the letters
// people confuse with digits (O→0, I/L→1). It returns "" if the result isn't a valid code.
func NormalizePairCode(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case ' ', '-', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if !strings.ContainsRune(pairCodeAlphabet, r) {
			return ""
		}
		sb.WriteRune(r)
	}
	if sb.Len() != PairCodeLen {
		return ""
	}
	return sb.String()
}

// PairCodeLookup is what stands in for a typed code in signalling messages. code must be
// normalized. The "pc2-" prefix lets the home server tell these from QR codes.
func PairCodeLookup(code string) string {
	h := sha256.Sum256([]byte("homellm-pair-code-v2\x00" + code))
	return "pc2-" + hex.EncodeToString(h[:])
}

// IsPairCodeLookup reports whether a pairing code came from a typed code.
func IsPairCodeLookup(s string) bool { return strings.HasPrefix(s, "pc2-") }

// PairCodeProof is pair_response's (and so pair_result's) payload for a typed-code pairing.
type PairCodeProof struct {
	Pub   string `json:"pub"`   // base64 Ed25519 public key
	Proof string `json:"proof"` // base64 PairCodeMAC
}

// pairCodeIterations slows each guess at the code; it costs the client well under a second.
const pairCodeIterations = 100_000

// PairCodeMAC proves that whoever made it knew code: an HMAC-SHA256 over the device ID and
// public key, keyed by PBKDF2-SHA256(code, "homellm-pair-proof-v2|"+deviceID).
func PairCodeMAC(code, deviceID string, pub []byte) []byte {
	key, _ := pbkdf2.Key(sha256.New, code, []byte("homellm-pair-proof-v2|"+deviceID), pairCodeIterations, 32)
	m := hmac.New(sha256.New, key)
	m.Write([]byte("homellm-pair-key-v2\x00" + deviceID + "\x00"))
	m.Write(pub)
	return m.Sum(nil)
}

// NewPairCodeProof builds the payload the home server returns for a typed-code pairing.
func NewPairCodeProof(code, deviceID string, pub []byte) PairCodeProof {
	return PairCodeProof{
		Pub:   base64.StdEncoding.EncodeToString(pub),
		Proof: base64.StdEncoding.EncodeToString(PairCodeMAC(code, deviceID, pub)),
	}
}

// VerifyPairCodeProof checks a PairCodeProof and returns the public key it vouches for.
func VerifyPairCodeProof(code, deviceID string, p PairCodeProof) ([]byte, bool) {
	pub, err := base64.StdEncoding.DecodeString(p.Pub)
	if err != nil || len(pub) != 32 {
		return nil, false
	}
	mac, err := base64.StdEncoding.DecodeString(p.Proof)
	if err != nil {
		return nil, false
	}
	return pub, hmac.Equal(mac, PairCodeMAC(code, deviceID, pub))
}

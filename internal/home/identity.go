package home

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Identity is the home server's long-lived credentials, created on first run and kept in
// <dir>/identity.json (mode 0600).
//   - DeviceID/DeviceSecret authenticate it to the signalling server.
//   - PrivateKey signs every WebRTC answer; its public half goes into the pairing QR, so phones
//     can tell they're really talking to this machine even if signalling is compromised.
type Identity struct {
	DeviceID     string             `json:"device_id"`
	DeviceSecret string             `json:"device_secret"`
	PrivateKey   ed25519.PrivateKey `json:"private_key"`
	Name         string             `json:"name"`
}

// PublicKeyBase64 is the encoding used in the pairing QR.
func (id *Identity) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(id.PrivateKey.Public().(ed25519.PublicKey))
}

// LoadOrCreateIdentity reads the identity from dir, creating one if none exists. A non-empty
// name overrides the stored display name.
func LoadOrCreateIdentity(dir, name string) (*Identity, error) {
	path := filepath.Join(dir, "identity.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var id Identity
		if err := json.Unmarshal(data, &id); err != nil {
			return nil, err
		}
		if len(id.PrivateKey) != ed25519.PrivateKeySize || id.DeviceID == "" {
			return nil, errors.New(path + " is corrupt; delete it to re-create (phones will need to re-pair)")
		}
		if name != "" && name != id.Name {
			id.Name = name
			return &id, save(path, &id)
		}
		return &id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	id := &Identity{DeviceID: randomHex(16), DeviceSecret: randomHex(32), PrivateKey: priv, Name: name}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return id, save(path, id)
}

func save(path string, id *Identity) error {
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

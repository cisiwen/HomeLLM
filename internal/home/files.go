package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/wweng/homellm/protocol"
)

// MaxFileSize caps one received image. Phones should downscale photos before sending (vision
// models resize to about 1 megapixel anyway), so this only stops abuse.
const MaxFileSize = 20 << 20

// maxClipSize caps one received clip (sent back to have sound added).
const maxClipSize = 64 << 20

// imageStore holds the images one phone session has sent. A request on the chat channel can
// arrive before the image it refers to has finished arriving on the files channel (they are
// independent streams), so lookups wait for the image instead of failing.
type imageStore struct {
	mu      sync.Mutex
	entries map[string]*imageEntry
}

type imageEntry struct {
	ready chan struct{} // closed once img or err is set
	img   Image
	err   error
}

func newImageStore() *imageStore {
	return &imageStore{entries: map[string]*imageEntry{}}
}

func (s *imageStore) entry(id string) *imageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		e = &imageEntry{ready: make(chan struct{})}
		s.entries[id] = e
	}
	return e
}

func (s *imageStore) finish(id string, img Image, err error) {
	e := s.entry(id)
	select {
	case <-e.ready: // a duplicate id; keep the first result
	default:
		e.img, e.err = img, err
		close(e.ready)
	}
}

// Wait returns the image with the given id once it has fully arrived.
func (s *imageStore) Wait(ctx context.Context, id string) (Image, error) {
	e := s.entry(id)
	select {
	case <-e.ready:
		return e.img, e.err
	case <-ctx.Done():
		return Image{}, fmt.Errorf("image %s did not arrive: %w", id, ctx.Err())
	}
}

// fileReceiver reassembles files from the files channel: a file_start text message, binary
// chunks, then file_end. Only one file is in flight at a time, so chunks need no header.
type fileReceiver struct {
	store   *imageStore
	replyFn func(protocol.Peer) // sends file_error back to the phone

	cur  *protocol.Peer
	buf  []byte
	fail string // set when the current file has already been rejected
}

func (r *fileReceiver) onText(data []byte) {
	var m protocol.Peer
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case protocol.PeerFileStart:
		if r.cur != nil {
			r.reject("a new file started before the previous one ended")
		}
		r.cur, r.buf, r.fail = &m, nil, ""
		switch {
		case m.ID == "":
			r.fail = "file id required"
		case m.Size <= 0 || m.Size > maxClipSize || (m.Size > MaxFileSize && !strings.HasPrefix(m.MIME, "video/")):
			r.fail = fmt.Sprintf("file size must be 1..%d bytes (%d for a clip)", MaxFileSize, maxClipSize)
		case !strings.HasPrefix(m.MIME, "image/") && m.MIME != "video/mp4":
			r.fail = "only images and MP4 clips are supported"
		default:
			r.buf = make([]byte, 0, m.Size)
		}
	case protocol.PeerFileEnd:
		if r.cur == nil || r.cur.ID != m.ID {
			return
		}
		if r.fail != "" {
			r.reject(r.fail)
			return
		}
		if int64(len(r.buf)) != r.cur.Size {
			r.reject(fmt.Sprintf("expected %d bytes, got %d", r.cur.Size, len(r.buf)))
			return
		}
		if r.cur.SHA256 != "" {
			sum := sha256.Sum256(r.buf)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), r.cur.SHA256) {
				r.reject("sha256 mismatch")
				return
			}
		}
		r.store.finish(r.cur.ID, Image{MIME: r.cur.MIME, Data: r.buf}, nil)
		r.cur, r.buf = nil, nil
	}
}

func (r *fileReceiver) onBinary(data []byte) {
	if r.cur == nil || r.fail != "" {
		return
	}
	if int64(len(r.buf)+len(data)) > r.cur.Size {
		r.fail = "more data than the declared size"
		r.buf = nil
		return
	}
	r.buf = append(r.buf, data...)
}

func (r *fileReceiver) reject(reason string) {
	id := r.cur.ID
	r.cur, r.buf, r.fail = nil, nil, ""
	if id == "" {
		return
	}
	r.store.finish(id, Image{}, errors.New(reason))
	r.replyFn(protocol.Peer{Type: protocol.PeerFileError, ID: id, Message: reason})
}

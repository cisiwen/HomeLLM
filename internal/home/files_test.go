package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/wweng/homellm/protocol"
)

func sendFile(rx *fileReceiver, id, mime string, data []byte, declaredSize int64, sum string) {
	start, _ := json.Marshal(protocol.Peer{Type: protocol.PeerFileStart, ID: id, MIME: mime, Size: declaredSize, SHA256: sum})
	rx.onText(start)
	for off := 0; off < len(data); off += 5 {
		rx.onBinary(data[off:min(off+5, len(data))])
	}
	end, _ := json.Marshal(protocol.Peer{Type: protocol.PeerFileEnd, ID: id})
	rx.onText(end)
}

func TestImageArrivesAfterRequestStartsWaiting(t *testing.T) {
	store := newImageStore()
	rx := &fileReceiver{store: store, replyFn: func(protocol.Peer) {}}
	data := []byte("not really a jpeg, but bytes are bytes")
	sum := sha256.Sum256(data)

	got := make(chan Image, 1)
	go func() {
		img, err := store.Wait(context.Background(), "a")
		if err != nil {
			t.Error(err)
		}
		got <- img
	}()
	time.Sleep(10 * time.Millisecond) // the request is already waiting
	sendFile(rx, "a", "image/jpeg", data, int64(len(data)), hex.EncodeToString(sum[:]))

	select {
	case img := <-got:
		if string(img.Data) != string(data) || img.MIME != "image/jpeg" {
			t.Fatalf("got %q %q", img.MIME, img.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never woke up")
	}
}

func TestRejectedFilesReportErrors(t *testing.T) {
	cases := []struct {
		name string
		mime string
		size int64
		sum  string
	}{
		{"wrong size", "image/png", 3, ""},
		{"bad hash", "image/png", 10, "00"},
		{"not an image", "text/plain", 10, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newImageStore()
			var replies []protocol.Peer
			rx := &fileReceiver{store: store, replyFn: func(p protocol.Peer) { replies = append(replies, p) }}
			sendFile(rx, "x", c.mime, []byte("0123456789"), c.size, c.sum)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := store.Wait(ctx, "x"); err == nil {
				t.Fatal("expected an error")
			}
			if len(replies) != 1 || replies[0].Type != protocol.PeerFileError {
				t.Fatalf("expected one file_error, got %+v", replies)
			}
		})
	}
}

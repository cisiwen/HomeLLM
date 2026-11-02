// Package tunnel forwards TCP connections over WebRTC data channels, so a service on a home
// PC that listens only on localhost (e.g. windows-mcp) can be used from anywhere with no open
// ports, no VPN and no relay in the data path.
//
// It reuses HomeLLM's signalling, pairing and signed-answer scheme (see package protocol and
// package home): the PC runs a Home that connects out to the signalling server, and a client
// pairs with it once and then opens one peer connection, with one data channel per forwarded
// TCP connection. A channel is labelled "tcp:<port>"; the Home only dials ports on its
// allow-list, and only on 127.0.0.1.
package tunnel

import (
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/wweng/homellm/protocol"
)

const (
	// ChannelPrefix starts the label of every forwarding data channel; the rest is the port.
	ChannelPrefix = "tcp:"
	// controlChannel is created only so the offer contains an SCTP section; every forwarding
	// channel is then added on the already-connected peer connection without renegotiating.
	controlChannel = "ctl"

	// maxBuffered is how much a sender lets queue in the data channel before it waits.
	maxBuffered = 1 << 20
	// maxRead is pion's default maximum data channel message size.
	maxRead = 64 * 1024
)

// newAPI returns a WebRTC API whose data channels can be detached into plain io.ReadWriters.
func newAPI() *webrtc.API {
	var se webrtc.SettingEngine
	se.DetachDataChannels()
	return webrtc.NewAPI(webrtc.WithSettingEngine(se))
}

// pipe copies between a detached data channel and a TCP connection until either side ends,
// then closes both. dc is the channel, used only to see how much is queued in it.
func pipe(raw io.ReadWriteCloser, dc *webrtc.DataChannel, c net.Conn) {
	var stopped atomic.Bool
	done := make(chan struct{}, 2)

	go func() { // data channel -> TCP
		buf := make([]byte, maxRead)
		for {
			n, err := raw.Read(buf)
			if n > 0 {
				if _, werr := c.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		stopped.Store(true)
		done <- struct{}{}
	}()
	go func() { // TCP -> data channel
		buf := make([]byte, protocol.FileChunkSize)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				// Detached channels don't push back on their own, so a fast local reader
				// (a big screenshot) would otherwise queue without limit.
				for dc.BufferedAmount() > maxBuffered && !stopped.Load() {
					time.Sleep(5 * time.Millisecond)
				}
				if _, werr := raw.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		stopped.Store(true)
		done <- struct{}{}
	}()

	<-done
	raw.Close()
	c.Close()
	<-done
}

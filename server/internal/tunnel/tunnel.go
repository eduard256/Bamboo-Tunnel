package tunnel

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/eduard256/Bamboo-Tunnel/pkg/crypto"
	"github.com/eduard256/Bamboo-Tunnel/pkg/protocol"
	"github.com/eduard256/Bamboo-Tunnel/pkg/tunnel"
)

var (
	log   *slog.Logger
	token string

	// active tunnel connection
	activeMu   sync.Mutex
	activeConn *conn

	// TUN device write callback -- set by tun module
	tunWriter func([]byte)

	// ring buffer for packets when no tunnel is active
	ringBuf *tunnel.RingBuffer
)

type conn struct {
	w       http.ResponseWriter
	flusher http.Flusher
	done    chan struct{}
	seq     atomic.Uint32
}

func Init(mux *http.ServeMux) {
	token = os.Getenv("BAMBOO_TOKEN")
	if token == "" {
		slog.Error("[tunnel] BAMBOO_TOKEN is not set")
		os.Exit(1)
	}

	log = slog.Default().With("module", "tunnel")

	// 64MB / ~1400 bytes per packet ≈ 48000 packets
	ringBuf = tunnel.NewRingBuffer(48000)

	// register on mux -- handles ALL paths, web module handles non-tunnel requests
	mux.HandleFunc("/", handler)

	log.Info("tunnel handler registered")
}

// SetTUNWriter sets the callback for writing packets to TUN device
func SetTUNWriter(f func([]byte)) {
	tunWriter = f
}

// SendPacket sends an IP packet through the active tunnel.
// If no tunnel is active, packets are buffered in ring buffer.
func SendPacket(pkt []byte) {
	activeMu.Lock()
	c := activeConn
	activeMu.Unlock()

	if c == nil {
		ringBuf.Push(pkt)
		return
	}

	c.sendData(pkt)
}

func handler(w http.ResponseWriter, r *http.Request) {
	// only POST with valid token = tunnel
	if r.Method != http.MethodPost || !crypto.ValidateRequest(r, token) {
		// delegate to web handler for static content
		webHandler(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	log.Info("tunnel client connected", "remote", r.RemoteAddr, "path", r.URL.Path)

	// set response headers to mimic video call stream
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	c := &conn{
		w:       w,
		flusher: flusher,
		done:    make(chan struct{}),
	}

	// send fake handshake
	for _, msg := range protocol.ServerHandshakeMessages() {
		f := &protocol.Frame{
			Type:    protocol.TypeHandshake,
			Seq:     c.seq.Add(1),
			Payload: msg,
		}
		buf := make([]byte, protocol.MaxFrameSize)
		n := protocol.MarshalFrame(buf, f, protocol.RandPadding(10, 40))
		w.Write(buf[:n])
	}
	flusher.Flush()

	// register as active connection
	activeMu.Lock()
	prev := activeConn
	activeConn = c
	activeMu.Unlock()

	if prev != nil {
		close(prev.done)
	}

	// drain ring buffer
	for _, pkt := range ringBuf.Drain() {
		c.sendData(pkt)
	}

	// start heartbeat sender
	go c.heartbeatLoop()

	// read incoming frames from client
	c.readLoop(r.Body)

	// cleanup
	activeMu.Lock()
	if activeConn == c {
		activeConn = nil
	}
	activeMu.Unlock()

	select {
	case <-c.done:
	default:
		close(c.done)
	}

	log.Info("tunnel client disconnected", "remote", r.RemoteAddr)
}

func (c *conn) sendData(pkt []byte) {
	buf := make([]byte, protocol.MaxFrameSize)
	f := &protocol.Frame{
		Type:    protocol.TypeData,
		Seq:     c.seq.Add(1),
		Payload: pkt,
	}
	padding := protocol.RandPadding(5, 50)
	n := protocol.MarshalFrame(buf, f, padding)

	select {
	case <-c.done:
		return
	default:
	}

	c.w.Write(buf[:n])
	c.flusher.Flush()
}

func (c *conn) readLoop(body io.ReadCloser) {
	buf := make([]byte, protocol.MaxFrameSize)
	for {
		f, err := protocol.ReadFrame(body, buf)
		if err != nil {
			return
		}

		if err := protocol.SkipPadding(body, buf); err != nil {
			return
		}

		switch f.Type {
		case protocol.TypeData:
			if tunWriter != nil && len(f.Payload) > 0 {
				// split coalesced packets
				packets, _ := protocol.SplitPackets(f.Payload)
				for _, pkt := range packets {
					tunWriter(pkt)
				}
			}
		case protocol.TypeHeartbeat:
			// heartbeat received, connection is alive
		case protocol.TypeClose:
			return
		}
	}
}

func (c *conn) heartbeatLoop() {
	for {
		interval := protocol.RandInterval(protocol.HeartbeatMinInterval, protocol.HeartbeatMaxInterval)

		select {
		case <-c.done:
			return
		case <-timeAfter(interval):
		}

		f := &protocol.Frame{
			Type:    protocol.TypeHeartbeat,
			Seq:     c.seq.Add(1),
			Payload: protocol.MakeHeartbeatPayload(),
		}
		buf := make([]byte, protocol.MaxFrameSize)
		n := protocol.MarshalFrame(buf, f, protocol.RandPadding(5, 20))

		select {
		case <-c.done:
			return
		default:
		}

		c.w.Write(buf[:n])
		c.flusher.Flush()
	}
}

// webHandler is set by the web module
var webHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

// SetWebHandler sets the fallback handler for non-tunnel requests
func SetWebHandler(h http.HandlerFunc) {
	webHandler = h
}

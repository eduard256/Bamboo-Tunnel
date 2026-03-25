package tunnel

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/eduard256/Bamboo-Tunnel/pkg/crypto"
	"github.com/eduard256/Bamboo-Tunnel/pkg/protocol"
	"github.com/eduard256/Bamboo-Tunnel/pkg/tunnel"
)

var (
	log   *slog.Logger
	token string

	// active tunnel connection (download stream)
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

	ringBuf = tunnel.NewRingBuffer(48000)

	mux.HandleFunc("/", handler)

	log.Info("tunnel handler registered")
}

func SetTUNWriter(f func([]byte)) {
	tunWriter = f
}

// SendPacket sends an IP packet through the active tunnel (download stream).
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
		webHandler(w, r)
		return
	}

	path := r.URL.Path

	// /xxx/xxx/data = upload (client -> server), short-lived POST
	if strings.HasSuffix(path, "/data") {
		handleUpload(w, r)
		return
	}

	// /xxx/xxx/stream = download (server -> client), long-lived chunked response
	handleDownload(w, r)
}

// handleDownload opens the download stream (server -> client).
// Server writes frames into chunked response. Connection stays open.
func handleDownload(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	log.Info("tunnel client connected", "remote", r.RemoteAddr, "path", r.URL.Path)

	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache, no-store")
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

	// register as active
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

	// heartbeat in background
	go c.heartbeatLoop()

	// block until done -- keep response open
	<-c.done

	// cleanup
	activeMu.Lock()
	if activeConn == c {
		activeConn = nil
	}
	activeMu.Unlock()

	log.Info("tunnel client disconnected", "remote", r.RemoteAddr)
}

// handleUpload receives frames from client via persistent chunked POST.
// Client keeps writing frames into the request body as a stream.
func handleUpload(w http.ResponseWriter, r *http.Request) {
	log.Info("upload stream opened", "remote", r.RemoteAddr, "path", r.URL.Path)

	buf := make([]byte, protocol.MaxFrameSize)
	for {
		f, err := protocol.ReadFrame(r.Body, buf)
		if err != nil {
			break
		}
		if err := protocol.SkipPadding(r.Body, buf); err != nil {
			break
		}

		switch f.Type {
		case protocol.TypeData:
			if tunWriter != nil && len(f.Payload) > 0 {
				packets, _ := protocol.SplitPackets(f.Payload)
				for _, pkt := range packets {
					tunWriter(pkt)
				}
			}
		case protocol.TypeHeartbeat:
			// alive
		case protocol.TypeClose:
			activeMu.Lock()
			c := activeConn
			activeMu.Unlock()
			if c != nil {
				c.markDone()
			}
		}
	}

	log.Info("upload stream closed", "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusOK)
}

func (c *conn) sendData(pkt []byte) {
	select {
	case <-c.done:
		log.Info("sendData: conn done, dropping")
		return
	default:
	}

	log.Info("sendData: sending", "pktLen", len(pkt))

	// wrap packet in coalesced format for SplitPackets on receiver
	coalBuf := make([]byte, 2+len(pkt))
	n := protocol.CoalescePackets(coalBuf, [][]byte{pkt})

	f := &protocol.Frame{
		Type:    protocol.TypeData,
		Seq:     c.seq.Add(1),
		Payload: coalBuf[:n],
	}
	buf := make([]byte, protocol.MaxFrameSize)
	padding := protocol.RandPadding(5, 50)
	n = protocol.MarshalFrame(buf, f, padding)

	written, err := c.w.Write(buf[:n])
	log.Info("sendData: written", "n", written, "err", err)
	c.flusher.Flush()
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

func (c *conn) markDone() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

var webHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

func SetWebHandler(h http.HandlerFunc) {
	webHandler = h
}


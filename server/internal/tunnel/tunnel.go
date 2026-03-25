package tunnel

import (
	"bufio"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eduard256/Bamboo-Tunnel/pkg/crypto"
	"github.com/eduard256/Bamboo-Tunnel/pkg/protocol"
	"github.com/eduard256/Bamboo-Tunnel/pkg/tunnel"
)

const (
	batchSize    = 32 * 1024     // 32KB max coalesced payload
	batchTimeout = time.Millisecond // flush after 1ms of no new packets
	pktChanSize  = 4096          // buffered channel for packets
	writeBufSize = 64 * 1024     // bufio.Writer buffer
)

var (
	log   *slog.Logger
	token string

	activeConn atomic.Pointer[conn]

	tunWriter func([]byte)
	ringBuf   *tunnel.RingBuffer

	framePool = sync.Pool{New: func() any { b := make([]byte, protocol.MaxFrameSize); return &b }}
)

type conn struct {
	bw      *bufio.Writer
	flusher http.Flusher
	done    chan struct{}
	seq     atomic.Uint32
	pktCh   chan []byte
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

func SetTUNWriter(f func([]byte)) { tunWriter = f }

// SendPacket sends an IP packet through the active tunnel.
func SendPacket(pkt []byte) {
	c := activeConn.Load()
	if c == nil {
		ringBuf.Push(pkt)
		return
	}

	// copy packet -- TUN read buffer is reused
	cp := make([]byte, len(pkt))
	copy(cp, pkt)

	select {
	case c.pktCh <- cp:
	default:
		// channel full, drop packet (TCP will retransmit)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !crypto.ValidateRequest(r, token) {
		webHandler(w, r)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/data") {
		handleUpload(w, r)
		return
	}

	handleDownload(w, r)
}

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

	bw := bufio.NewWriterSize(w, writeBufSize)

	c := &conn{
		bw:      bw,
		flusher: flusher,
		done:    make(chan struct{}),
		pktCh:   make(chan []byte, pktChanSize),
	}

	// send handshake
	buf := getFrameBuf()
	for _, msg := range protocol.ServerHandshakeMessages() {
		f := &protocol.Frame{Type: protocol.TypeHandshake, Seq: c.seq.Add(1), Payload: msg}
		n := protocol.MarshalFrame(*buf, f, protocol.RandPadding(10, 40))
		bw.Write((*buf)[:n])
	}
	bw.Flush()
	flusher.Flush()
	putFrameBuf(buf)

	// register as active
	prev := activeConn.Swap(c)
	if prev != nil {
		close(prev.done)
	}

	// drain ring buffer
	for _, pkt := range ringBuf.Drain() {
		select {
		case c.pktCh <- pkt:
		default:
		}
	}

	// batch writer goroutine
	go c.batchWriteLoop()

	// heartbeat
	go c.heartbeatLoop()

	// block until done
	<-c.done

	if activeConn.CompareAndSwap(c, nil) {
		// we were the active conn
	}

	log.Info("tunnel client disconnected", "remote", r.RemoteAddr)
}

// batchWriteLoop collects packets from channel and writes them as coalesced frames.
// Flushes when: buffer exceeds batchSize, or batchTimeout elapsed since last packet.
func (c *conn) batchWriteLoop() {
	coalBuf := make([]byte, batchSize+2*1500) // extra room
	frameBuf := make([]byte, protocol.MaxFrameSize)
	timer := time.NewTimer(batchTimeout)
	timer.Stop()

	var packets [][]byte
	var coalSize int

	flush := func() {
		if len(packets) == 0 {
			return
		}

		off := 0
		off = protocol.CoalescePackets(coalBuf, packets)

		f := &protocol.Frame{
			Type:    protocol.TypeData,
			Seq:     c.seq.Add(1),
			Payload: coalBuf[:off],
		}
		n := protocol.MarshalFrame(frameBuf, f, protocol.RandPadding(1, 3))
		c.bw.Write(frameBuf[:n])
		c.bw.Flush()
		c.flusher.Flush()

		packets = packets[:0]
		coalSize = 0
	}

	for {
		select {
		case <-c.done:
			flush()
			return

		case pkt := <-c.pktCh:
			if pkt == nil {
				// heartbeat signal
				flush()
				hb := &protocol.Frame{
					Type:    protocol.TypeHeartbeat,
					Seq:     c.seq.Add(1),
					Payload: protocol.MakeHeartbeatPayload(),
				}
				n := protocol.MarshalFrame(frameBuf, hb, protocol.RandPadding(5, 20))
				c.bw.Write(frameBuf[:n])
				c.bw.Flush()
				c.flusher.Flush()
				continue
			}

			packets = append(packets, pkt)
			coalSize += 2 + len(pkt)

			if coalSize >= batchSize {
				timer.Stop()
				flush()
				continue
			}

			// drain channel without blocking
			drained := true
			for drained {
				select {
				case pkt2 := <-c.pktCh:
					if pkt2 == nil {
						continue // skip heartbeat during drain
					}
					packets = append(packets, pkt2)
					coalSize += 2 + len(pkt2)
					if coalSize >= batchSize {
						timer.Stop()
						flush()
					}
				default:
					drained = false
				}
			}

			if len(packets) > 0 {
				timer.Reset(batchTimeout)
			}

		case <-timer.C:
			flush()
		}
	}
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	buf := getFrameBuf()
	defer putFrameBuf(buf)

	for {
		f, err := protocol.ReadFrame(r.Body, *buf)
		if err != nil {
			break
		}
		if err := protocol.SkipPadding(r.Body, *buf); err != nil {
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
			c := activeConn.Load()
			if c != nil {
				c.markDone()
			}
		}
	}

	w.WriteHeader(http.StatusOK)
}

// heartbeat is sent via pktCh with nil marker to distinguish from data
func (c *conn) heartbeatLoop() {
	for {
		interval := protocol.RandInterval(protocol.HeartbeatMinInterval, protocol.HeartbeatMaxInterval)

		select {
		case <-c.done:
			return
		case <-time.After(interval):
		}

		// send nil as heartbeat signal to batchWriteLoop
		select {
		case c.pktCh <- nil:
		case <-c.done:
			return
		}
	}
}

func (c *conn) markDone() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

func getFrameBuf() *[]byte  { return framePool.Get().(*[]byte) }
func putFrameBuf(b *[]byte) { framePool.Put(b) }

var webHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}

func SetWebHandler(h http.HandlerFunc) { webHandler = h }

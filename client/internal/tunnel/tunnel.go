package tunnel

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eduard256/Bamboo-Tunnel/pkg/crypto"
	"github.com/eduard256/Bamboo-Tunnel/pkg/protocol"
)

const (
	batchSize   = 64 * 1024
	pktChanSize = 8192
)

var (
	log        *slog.Logger
	serverURL  string
	token      string
	tunnelPath string

	sessionMin     time.Duration
	sessionMax     time.Duration
	sessionOverlap time.Duration

	tunWriter func([]byte)

	activeConn atomic.Pointer[session]

	framePool = sync.Pool{New: func() any { b := make([]byte, protocol.MaxFrameSize); return &b }}
)

type session struct {
	client    *http.Client
	sessionID string
	body      io.ReadCloser
	uploadPW  *io.PipeWriter
	done      chan struct{}
	seq       atomic.Uint32
	pktCh     chan []byte // upload packet channel
}

func Init() {
	serverURL = os.Getenv("BAMBOO_SERVER_URL")
	if serverURL == "" {
		slog.Error("[tunnel] BAMBOO_SERVER_URL is not set")
		os.Exit(1)
	}

	token = os.Getenv("BAMBOO_TOKEN")
	if token == "" {
		slog.Error("[tunnel] BAMBOO_TOKEN is not set")
		os.Exit(1)
	}

	tunnelPath = os.Getenv("BAMBOO_TUNNEL_PATH")
	if tunnelPath == "" {
		tunnelPath = "/odr/v1/call"
	}

	sessionMin = parseDuration(os.Getenv("BAMBOO_SESSION_MIN"), 5*time.Minute)
	sessionMax = parseDuration(os.Getenv("BAMBOO_SESSION_MAX"), 20*time.Minute)
	sessionOverlap = parseDuration(os.Getenv("BAMBOO_SESSION_OVERLAP"), 30*time.Second)

	log = slog.Default().With("module", "tunnel")

	go sessionLoop()

	log.Info("tunnel started", "server", serverURL, "path", tunnelPath)
}

func SetTUNWriter(f func([]byte)) { tunWriter = f }

func SendPacket(pkt []byte) {
	s := activeConn.Load()
	if s == nil {
		return
	}

	// copy packet -- TUN read buffer is reused
	cp := make([]byte, len(pkt))
	copy(cp, pkt)

	select {
	case s.pktCh <- cp:
	default:
		// channel full, drop (TCP will retransmit)
	}
}

func sessionLoop() {
	var backoff time.Duration

	for {
		s, err := connect()
		if err != nil {
			if backoff == 0 {
				backoff = time.Second
			} else if backoff < 30*time.Second {
				backoff *= 2
			}
			log.Error("connect failed", "error", err, "retry", backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0

		prev := activeConn.Swap(s)
		if prev != nil {
			prev.close()
		}

		duration := randomDuration(sessionMin, sessionMax)
		log.Info("session started", "duration", duration, "id", s.sessionID)

		go s.readLoop()
		go s.batchUploadLoop()
		go s.heartbeatLoop()

		select {
		case <-time.After(duration - sessionOverlap):
			log.Info("session rotating")
		case <-s.done:
			log.Warn("session died, reconnecting")
			activeConn.CompareAndSwap(s, nil)
		}
	}
}

func connect() (*session, error) {
	client := newHTTPClient()
	sessionID := randSessionID()

	log.Info("connecting", "id", sessionID)

	// Step 1: GET /
	req, _ := http.NewRequest(http.MethodGet, serverURL+"/", nil)
	setBrowserHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tunnel: GET /: %w", err)
	}
	resp.Body.Close()

	// Step 2: GET fake asset
	req, _ = http.NewRequest(http.MethodGet, serverURL+"/assets/app.js", nil)
	setBrowserHeaders(req)
	resp, err = client.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	// Step 3: open download stream
	downloadPath := fmt.Sprintf("%s/%s/stream", tunnelPath, sessionID)
	req, _ = http.NewRequest(http.MethodPost, serverURL+downloadPath, nil)
	setBrowserHeaders(req)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Del("Accept-Encoding")
	req.AddCookie(&http.Cookie{Name: crypto.CookieName, Value: token})

	resp, err = client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tunnel: POST download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("tunnel: download returned %d", resp.StatusCode)
	}

	s := &session{
		client:    client,
		sessionID: sessionID,
		body:      resp.Body,
		done:      make(chan struct{}),
		pktCh:     make(chan []byte, pktChanSize),
	}

	// read server handshake
	buf := getFrameBuf()
	defer putFrameBuf(buf)
	for i := 0; i < 3; i++ {
		f, err := protocol.ReadFrame(resp.Body, *buf)
		if err != nil {
			s.close()
			return nil, fmt.Errorf("tunnel: handshake read %d: %w", i, err)
		}
		if err := protocol.SkipPadding(resp.Body, *buf); err != nil {
			s.close()
			return nil, fmt.Errorf("tunnel: handshake padding %d: %w", i, err)
		}
		if f.Type != protocol.TypeHandshake {
			break
		}
	}

	// Step 4: open persistent upload stream
	uploadPath := fmt.Sprintf("%s/%s/data", tunnelPath, sessionID)
	pr, pw := io.Pipe()
	s.uploadPW = pw

	uploadReq, _ := http.NewRequest(http.MethodPost, serverURL+uploadPath, pr)
	setBrowserHeaders(uploadReq)
	uploadReq.Header.Set("Content-Type", "application/x-protobuf")
	uploadReq.AddCookie(&http.Cookie{Name: crypto.CookieName, Value: token})

	go func() {
		resp, err := client.Do(uploadReq)
		if err != nil {
			log.Info("upload stream ended", "error", err)
		} else {
			resp.Body.Close()
		}
		s.markDone()
	}()

	log.Info("connected", "id", sessionID)
	return s, nil
}

func (s *session) batchUploadLoop() {
	coalBuf := make([]byte, batchSize+2*1500)
	frameBuf := make([]byte, protocol.MaxFrameSize)

	var packets [][]byte
	var coalSize int

	flush := func() {
		if len(packets) == 0 {
			return
		}
		off := protocol.CoalescePackets(coalBuf, packets)
		f := &protocol.Frame{
			Type:    protocol.TypeData,
			Seq:     s.seq.Add(1),
			Payload: coalBuf[:off],
		}
		n := protocol.MarshalFrame(frameBuf, f, 1)
		s.uploadPW.Write(frameBuf[:n])
		packets = packets[:0]
		coalSize = 0
	}

	sendHeartbeat := func() {
		flush()
		hb := &protocol.Frame{
			Type:    protocol.TypeHeartbeat,
			Seq:     s.seq.Add(1),
			Payload: protocol.MakeHeartbeatPayload(),
		}
		n := protocol.MarshalFrame(frameBuf, hb, protocol.RandPadding(5, 20))
		s.uploadPW.Write(frameBuf[:n])
	}

	for {
		select {
		case <-s.done:
			flush()
			return
		case pkt := <-s.pktCh:
			if pkt == nil {
				sendHeartbeat()
				continue
			}
			packets = append(packets, pkt)
			coalSize += 2 + len(pkt)
		}

		for coalSize < batchSize {
			select {
			case pkt := <-s.pktCh:
				if pkt == nil {
					continue
				}
				packets = append(packets, pkt)
				coalSize += 2 + len(pkt)
			default:
				goto flushNow
			}
		}
	flushNow:
		flush()
	}
}

func (s *session) readLoop() {
	buf := make([]byte, protocol.MaxFrameSize)
	for {
		f, err := protocol.ReadFrame(s.body, buf)
		if err != nil {
			s.markDone()
			return
		}
		if err := protocol.SkipPadding(s.body, buf); err != nil {
			s.markDone()
			return
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
			s.markDone()
			return
		}
	}
}

func (s *session) heartbeatLoop() {
	for {
		interval := protocol.RandInterval(protocol.HeartbeatMinInterval, protocol.HeartbeatMaxInterval)

		select {
		case <-s.done:
			return
		case <-time.After(interval):
		}

		// send nil as heartbeat signal to batchUploadLoop
		select {
		case s.pktCh <- nil:
		case <-s.done:
			return
		}
	}
}

func (s *session) close() {
	f := &protocol.Frame{Type: protocol.TypeClose, Seq: s.seq.Add(1)}
	buf := make([]byte, protocol.MaxFrameSize)
	n := protocol.MarshalFrame(buf, f, protocol.RandPadding(5, 15))
	s.uploadPW.Write(buf[:n])
	s.uploadPW.Close()
	s.markDone()
	s.body.Close()
}

func (s *session) markDone() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			WriteBufferSize:     64 * 1024,
			ReadBufferSize:      64 * 1024,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		Timeout: 0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Sec-Ch-Ua", `"Not_A Brand";v="8", "Chromium";v="131", "Google Chrome";v="131"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
}

func getFrameBuf() *[]byte  { return framePool.Get().(*[]byte) }
func putFrameBuf(b *[]byte) { framePool.Put(b) }

func randSessionID() string {
	const chars = "abcdef0123456789"
	b := make([]byte, 24)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

func randomDuration(min, max time.Duration) time.Duration {
	if max <= min {
		return min
	}
	return min + time.Duration(rand.Int64N(int64(max-min)))
}

func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func UseContext() context.Context {
	s := activeConn.Load()
	ctx, cancel := context.WithCancel(context.Background())
	if s == nil {
		cancel()
		return ctx
	}
	go func() {
		<-s.done
		cancel()
	}()
	return ctx
}

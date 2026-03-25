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

var (
	log       *slog.Logger
	serverURL string
	token     string
	tunnelPath string

	sessionMin     time.Duration
	sessionMax     time.Duration
	sessionOverlap time.Duration

	// TUN write callback -- set by tun module
	tunWriter func([]byte)

	// active session tracking
	activeMu   sync.Mutex
	activeConn *session
)

type session struct {
	resp    *http.Response
	body    io.ReadCloser
	writer  io.Writer
	done    chan struct{}
	seq     atomic.Uint32
	flusher http.Flusher
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

// SetTUNWriter sets the callback for writing packets to TUN device
func SetTUNWriter(f func([]byte)) {
	tunWriter = f
}

// SendPacket sends an IP packet through the active tunnel session
func SendPacket(pkt []byte) {
	activeMu.Lock()
	s := activeConn
	activeMu.Unlock()

	if s == nil {
		return
	}

	s.sendData(pkt)
}

// sessionLoop manages session rotation with hot swap
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

		// register as active
		activeMu.Lock()
		prev := activeConn
		activeConn = s
		activeMu.Unlock()

		if prev != nil {
			// graceful close previous session
			prev.close()
		}

		// random session duration
		duration := randomDuration(sessionMin, sessionMax)
		log.Info("session started", "duration", duration)

		// start reading from server in background
		go s.readLoop()

		// start heartbeat
		go s.heartbeatLoop()

		// wait until overlap time, then prepare next session
		select {
		case <-time.After(duration - sessionOverlap):
			// time to rotate -- next iteration will create new session
			// current session stays active until new one connects
			log.Info("session rotating")
		case <-s.done:
			// session died unexpectedly -- reconnect immediately
			log.Warn("session died, reconnecting")
			activeMu.Lock()
			if activeConn == s {
				activeConn = nil
			}
			activeMu.Unlock()
		}
	}
}

func connect() (*session, error) {
	// Step 1: mimic browser -- GET landing page
	client := newHTTPClient()

	path := fmt.Sprintf("%s/%s", tunnelPath, randSessionID())
	fullURL := serverURL + path

	req, err := http.NewRequest(http.MethodGet, serverURL+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("tunnel: build GET: %w", err)
	}
	setBrowserHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tunnel: GET /: %w", err)
	}
	resp.Body.Close()

	// Step 2: GET fake asset
	req, err = http.NewRequest(http.MethodGet, serverURL+"/assets/app.js", nil)
	if err != nil {
		return nil, fmt.Errorf("tunnel: build asset GET: %w", err)
	}
	setBrowserHeaders(req)
	resp, err = client.Do(req)
	if err != nil {
		// asset 404 is fine, server might not have it
		log.Debug("asset request failed (ok)", "error", err)
	} else {
		resp.Body.Close()
	}

	// Step 3: POST tunnel request with chunked body
	pr, pw := io.Pipe()

	req, err = http.NewRequest(http.MethodPost, fullURL, pr)
	if err != nil {
		pw.Close()
		return nil, fmt.Errorf("tunnel: build POST: %w", err)
	}

	setBrowserHeaders(req)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.AddCookie(&http.Cookie{
		Name:  crypto.CookieName,
		Value: token,
	})

	// send client handshake messages in request body
	go func() {
		for _, msg := range protocol.ClientHandshakeMessages() {
			f := &protocol.Frame{
				Type:    protocol.TypeHandshake,
				Seq:     0,
				Payload: msg,
			}
			buf := make([]byte, protocol.MaxFrameSize)
			n := protocol.MarshalFrame(buf, f, protocol.RandPadding(10, 40))
			pw.Write(buf[:n])
		}
	}()

	resp, err = client.Do(req)
	if err != nil {
		pw.Close()
		return nil, fmt.Errorf("tunnel: POST %s: %w", path, err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		pw.Close()
		return nil, fmt.Errorf("tunnel: server returned %d", resp.StatusCode)
	}

	s := &session{
		resp:   resp,
		body:   resp.Body,
		writer: pw,
		done:   make(chan struct{}),
	}

	// read and discard server handshake frames
	buf := make([]byte, protocol.MaxFrameSize)
	for i := 0; i < 3; i++ {
		f, err := protocol.ReadFrame(resp.Body, buf)
		if err != nil {
			s.close()
			return nil, fmt.Errorf("tunnel: handshake read: %w", err)
		}
		if err := protocol.SkipPadding(resp.Body, buf); err != nil {
			s.close()
			return nil, fmt.Errorf("tunnel: handshake padding: %w", err)
		}
		if f.Type != protocol.TypeHandshake {
			break
		}
	}

	log.Info("connected", "path", path)
	return s, nil
}

func (s *session) sendData(pkt []byte) {
	select {
	case <-s.done:
		return
	default:
	}

	f := &protocol.Frame{
		Type:    protocol.TypeData,
		Seq:     s.seq.Add(1),
		Payload: pkt,
	}

	buf := make([]byte, protocol.MaxFrameSize)
	padding := protocol.RandPadding(5, 50)
	n := protocol.MarshalFrame(buf, f, padding)
	s.writer.Write(buf[:n])
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

		f := &protocol.Frame{
			Type:    protocol.TypeHeartbeat,
			Seq:     s.seq.Add(1),
			Payload: protocol.MakeHeartbeatPayload(),
		}
		buf := make([]byte, protocol.MaxFrameSize)
		n := protocol.MarshalFrame(buf, f, protocol.RandPadding(5, 20))
		s.writer.Write(buf[:n])
	}
}

func (s *session) close() {
	// send graceful close frame
	f := &protocol.Frame{
		Type: protocol.TypeClose,
		Seq:  s.seq.Add(1),
	}
	buf := make([]byte, protocol.MaxFrameSize)
	n := protocol.MarshalFrame(buf, f, protocol.RandPadding(5, 15))
	s.writer.Write(buf[:n])

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
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		Timeout: 0, // no timeout for streaming
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// setBrowserHeaders makes the request look like Chrome
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

// UseContext returns a context that cancels when the active session dies.
// Used by tun module to detect disconnects.
func UseContext() context.Context {
	activeMu.Lock()
	s := activeConn
	activeMu.Unlock()

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

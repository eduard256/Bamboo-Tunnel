package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
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
	log        *slog.Logger
	serverURL  string
	token      string
	tunnelPath string

	sessionMin     time.Duration
	sessionMax     time.Duration
	sessionOverlap time.Duration

	tunWriter func([]byte)

	activeMu   sync.Mutex
	activeConn *session
)

type session struct {
	client    *http.Client
	sessionID string
	body      io.ReadCloser // download stream (server -> client)
	uploadPW  *io.PipeWriter // upload stream (client -> server)
	uploadMu  sync.Mutex
	done      chan struct{}
	seq       atomic.Uint32
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
	activeMu.Lock()
	s := activeConn
	activeMu.Unlock()

	if s == nil {
		return
	}

	s.sendData(pkt)
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

		activeMu.Lock()
		prev := activeConn
		activeConn = s
		activeMu.Unlock()

		if prev != nil {
			prev.close()
		}

		duration := randomDuration(sessionMin, sessionMax)
		log.Info("session started", "duration", duration, "id", s.sessionID)

		go s.readLoop()
		go s.heartbeatLoop()

		select {
		case <-time.After(duration - sessionOverlap):
			log.Info("session rotating")
		case <-s.done:
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
	client := newHTTPClient()
	sessionID := randSessionID()

	// Step 1: GET / (mimic browser)
	log.Info("connecting", "id", sessionID)
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

	log.Info("download stream opened", "proto", resp.Proto)

	s := &session{
		client:    client,
		sessionID: sessionID,
		body:      resp.Body,
		done:      make(chan struct{}),
	}

	// read server handshake
	buf := make([]byte, protocol.MaxFrameSize)
	for i := 0; i < 3; i++ {
		f, err := protocol.ReadFrame(resp.Body, buf)
		if err != nil {
			s.close()
			return nil, fmt.Errorf("tunnel: handshake read %d: %w", i, err)
		}
		if err := protocol.SkipPadding(resp.Body, buf); err != nil {
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

	// fire and forget -- we don't care about upload response
	go func() {
		resp, err := client.Do(uploadReq)
		if err != nil {
			log.Info("upload stream ended", "error", err)
		} else {
			resp.Body.Close()
			log.Info("upload stream ended", "status", resp.StatusCode)
		}
		s.markDone()
	}()

	log.Info("connected", "id", sessionID)
	return s, nil
}

func (s *session) sendData(pkt []byte) {
	select {
	case <-s.done:
		return
	default:
	}

	// wrap in coalesced format
	coalBuf := make([]byte, 2+len(pkt))
	cn := protocol.CoalescePackets(coalBuf, [][]byte{pkt})

	f := &protocol.Frame{
		Type:    protocol.TypeData,
		Seq:     s.seq.Add(1),
		Payload: coalBuf[:cn],
	}

	buf := make([]byte, protocol.MaxFrameSize)
	padding := protocol.RandPadding(5, 50)
	n := protocol.MarshalFrame(buf, f, padding)

	s.uploadMu.Lock()
	s.uploadPW.Write(buf[:n])
	s.uploadMu.Unlock()
}

func (s *session) readLoop() {
	log.Info("readLoop: started")
	buf := make([]byte, protocol.MaxFrameSize)
	for {
		f, err := protocol.ReadFrame(s.body, buf)
		if err != nil {
			log.Info("readLoop: error", "error", err)
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

		s.uploadMu.Lock()
		s.uploadPW.Write(buf[:n])
		s.uploadMu.Unlock()
	}
}

func (s *session) close() {
	f := &protocol.Frame{
		Type: protocol.TypeClose,
		Seq:  s.seq.Add(1),
	}
	buf := make([]byte, protocol.MaxFrameSize)
	n := protocol.MarshalFrame(buf, f, protocol.RandPadding(5, 15))

	s.uploadMu.Lock()
	s.uploadPW.Write(buf[:n])
	s.uploadPW.Close()
	s.uploadMu.Unlock()

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
			TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:  true,
			MaxIdleConns:       100,
			IdleConnTimeout:    90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
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

// unused but needed for compilation
var _ = bytes.NewReader
var _ = binary.BigEndian

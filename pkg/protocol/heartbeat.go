package protocol

import (
	"crypto/rand"
	"math/big"
	"time"
)

// Heartbeat settings disguised as audio silence frames.
// Real video call audio: 50-150 byte packets every 20-40ms during silence.
// We send heartbeats less frequently but with similar sizes.
const (
	HeartbeatMinInterval = 2 * time.Second
	HeartbeatMaxInterval = 5 * time.Second
	HeartbeatMinSize     = 60  // bytes, mimics audio frame
	HeartbeatMaxSize     = 150 // bytes
	HeartbeatTimeout     = 15 * time.Second // 3x max interval
)

// RandInterval returns random duration between min and max
func RandInterval(min, max time.Duration) time.Duration {
	diff := max - min
	if diff <= 0 {
		return min
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(diff)))
	return min + time.Duration(n.Int64())
}

// RandPadding returns random int between min and max (inclusive)
func RandPadding(min, max int) int {
	if max <= min {
		return min
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(max-min+1)))
	return min + int(n.Int64())
}

// MakeHeartbeatPayload creates random bytes mimicking audio silence frame
func MakeHeartbeatPayload() []byte {
	size := RandPadding(HeartbeatMinSize, HeartbeatMaxSize)
	buf := make([]byte, size)
	rand.Read(buf)
	return buf
}

package protocol

import (
	"encoding/binary"
	"errors"
)

// Coalesced packet format:
//   [2 bytes pkt_len][N bytes pkt_data] repeated
//
// Multiple IP packets are packed into a single frame payload.
// This mimics video call behavior where multiple media frames
// are batched together, and eliminates small-packet fingerprinting.

// CoalescePackets packs multiple IP packets into one payload.
// Returns bytes written into buf.
func CoalescePackets(buf []byte, packets [][]byte) int {
	off := 0
	for _, pkt := range packets {
		n := len(pkt)
		if off+2+n > len(buf) {
			break
		}
		binary.BigEndian.PutUint16(buf[off:], uint16(n))
		copy(buf[off+2:], pkt)
		off += 2 + n
	}
	return off
}

// SplitPackets extracts individual IP packets from coalesced payload.
func SplitPackets(data []byte) ([][]byte, error) {
	var packets [][]byte
	off := 0
	for off+2 <= len(data) {
		n := int(binary.BigEndian.Uint16(data[off:]))
		if n == 0 {
			break
		}
		off += 2
		if off+n > len(data) {
			return packets, errors.New("protocol: truncated coalesced packet")
		}
		packets = append(packets, data[off:off+n])
		off += n
	}
	return packets, nil
}

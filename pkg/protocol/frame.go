package protocol

import (
	"encoding/binary"
	"io"
)

// Frame types
const (
	TypeData      byte = 0x01 // TUN packet(s)
	TypeHeartbeat byte = 0x02 // keepalive disguised as audio frame
	TypeHandshake byte = 0x03 // initial fake "join room" exchange
	TypeClose     byte = 0x04 // graceful session close
)

// Frame wire format:
//   [1 byte type][4 bytes seq][2 bytes length][N bytes payload][M bytes padding]
//
// Total header = 7 bytes. Max payload = 65535 bytes.
// Padding length is encoded in last byte of the frame (padding[len-1] = len(padding)).
const headerSize = 7

// MaxFrameSize = header + max payload + max padding (256 bytes)
const MaxFrameSize = headerSize + 65535 + 256

// Frame represents a single protocol frame
type Frame struct {
	Type    byte
	Seq     uint32
	Payload []byte
}

// MarshalFrame writes frame into buf with random padding.
// Returns number of bytes written. buf must be large enough.
func MarshalFrame(buf []byte, f *Frame, padding int) int {
	payloadLen := len(f.Payload)
	total := headerSize + payloadLen + padding

	buf[0] = f.Type
	binary.BigEndian.PutUint32(buf[1:5], f.Seq)
	binary.BigEndian.PutUint16(buf[5:7], uint16(payloadLen))

	if payloadLen > 0 {
		copy(buf[headerSize:], f.Payload)
	}

	// fill padding: first byte = padding length, rest = random-ish fill
	if padding > 0 {
		off := headerSize + payloadLen
		buf[off] = byte(padding)
		for i := 1; i < padding; i++ {
			buf[off+i] = byte(i * 7) // cheap non-zero fill
		}
	}

	return total
}

// ReadFrame reads one frame from r. Returns parsed frame or error.
func ReadFrame(r io.Reader, buf []byte) (*Frame, error) {
	if _, err := io.ReadFull(r, buf[:headerSize]); err != nil {
		return nil, err
	}

	f := &Frame{
		Type: buf[0],
		Seq:  binary.BigEndian.Uint32(buf[1:5]),
	}

	payloadLen := int(binary.BigEndian.Uint16(buf[5:7]))
	if payloadLen == 0 {
		return f, nil
	}

	f.Payload = make([]byte, payloadLen)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return nil, err
	}

	return f, nil
}

// SkipPadding reads and discards padding bytes after frame payload.
// The padding length is unknown to the reader, so we read 1 byte to get length,
// then skip the rest. This is called after ReadFrame when padding is expected.
func SkipPadding(r io.Reader, buf []byte) error {
	// read one byte to get padding length
	if _, err := io.ReadFull(r, buf[:1]); err != nil {
		return err
	}
	padLen := int(buf[0])
	if padLen <= 1 {
		return nil
	}
	// skip remaining padding bytes
	if _, err := io.ReadFull(r, buf[:padLen-1]); err != nil {
		return err
	}
	return nil
}

package tunnel

import "sync"

// RingBuffer is a fixed-size circular buffer for IP packets.
// When full, oldest packets are dropped (TCP will retransmit).
// Prioritizes latency over throughput -- better to drop than to queue.
type RingBuffer struct {
	mu    sync.Mutex
	buf   [][]byte
	head  int
	tail  int
	count int
	size  int
}

func NewRingBuffer(size int) *RingBuffer {
	return &RingBuffer{
		buf:  make([][]byte, size),
		size: size,
	}
}

// Push adds packet to buffer. Returns false if buffer was full (oldest dropped).
func (r *RingBuffer) Push(pkt []byte) bool {
	cp := make([]byte, len(pkt))
	copy(cp, pkt)

	r.mu.Lock()
	defer r.mu.Unlock()

	dropped := false
	if r.count == r.size {
		// drop oldest
		r.buf[r.tail] = nil
		r.tail = (r.tail + 1) % r.size
		r.count--
		dropped = true
	}

	r.buf[r.head] = cp
	r.head = (r.head + 1) % r.size
	r.count++
	return !dropped
}

// Pop removes and returns oldest packet. Returns nil if empty.
func (r *RingBuffer) Pop() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil
	}

	pkt := r.buf[r.tail]
	r.buf[r.tail] = nil
	r.tail = (r.tail + 1) % r.size
	r.count--
	return pkt
}

// Len returns current number of packets in buffer
func (r *RingBuffer) Len() int {
	r.mu.Lock()
	n := r.count
	r.mu.Unlock()
	return n
}

// Usage returns buffer utilization as fraction 0.0-1.0
func (r *RingBuffer) Usage() float64 {
	r.mu.Lock()
	u := float64(r.count) / float64(r.size)
	r.mu.Unlock()
	return u
}

// Drain returns all packets and resets buffer
func (r *RingBuffer) Drain() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.count == 0 {
		return nil
	}

	result := make([][]byte, 0, r.count)
	for r.count > 0 {
		result = append(result, r.buf[r.tail])
		r.buf[r.tail] = nil
		r.tail = (r.tail + 1) % r.size
		r.count--
	}
	return result
}

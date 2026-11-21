package cdgate

import (
	"io"
	"sync"
)

// MaxCaptureBytes caps the output a deploy step keeps for classification. Past
// the cap the capture drops its bytes and marks itself overflowed.
const MaxCaptureBytes = 1024 * 1024

// OrderedCapture shares one capped buffer across both streams. Each stream gets
// its own writer, so the recorded output follows the copy goroutines' arrival order.
type OrderedCapture struct {
	mu       sync.Mutex
	chunks   [][]byte
	bytes    int
	overflow bool
	Stdout   io.Writer
	Stderr   io.Writer
}

// NewOrderedCapture returns a capture with one writer per stream.
func NewOrderedCapture() *OrderedCapture {
	oc := &OrderedCapture{}
	oc.Stdout = orderedWriter{oc}
	oc.Stderr = orderedWriter{oc}
	return oc
}

// Result returns the captured output and whether it overflowed the cap.
func (oc *OrderedCapture) Result() ([]byte, bool) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.overflow {
		return nil, true
	}
	out := make([]byte, 0, oc.bytes)
	for _, chunk := range oc.chunks {
		out = append(out, chunk...)
	}
	return out, false
}

func (oc *OrderedCapture) add(p []byte) {
	if oc.overflow {
		return
	}
	oc.bytes += len(p)
	if oc.bytes > MaxCaptureBytes {
		oc.overflow = true
		oc.chunks = nil
		return
	}
	oc.chunks = append(oc.chunks, append([]byte(nil), p...))
}

type orderedWriter struct{ oc *OrderedCapture }

func (w orderedWriter) Write(p []byte) (int, error) {
	w.oc.mu.Lock()
	defer w.oc.mu.Unlock()
	w.oc.add(p)
	return len(p), nil
}

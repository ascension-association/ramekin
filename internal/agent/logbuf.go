package agent

import (
	"bytes"
	"io"
	"sync"
)

// LogBuffer tees writes to an underlying writer (gokrazy captures stdout/
// stderr for its web UI) and keeps the most recent lines for the log API.
type LogBuffer struct {
	out io.Writer
	max int

	mu      sync.Mutex
	lines   [][]byte
	partial []byte
	subs    map[chan []byte]struct{}
}

func NewLogBuffer(out io.Writer, maxLines int) *LogBuffer {
	return &LogBuffer{out: out, max: maxLines, subs: map[chan []byte]struct{}{}}
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.out.Write(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	data := append(b.partial, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), data[:i+1]...)
		data = data[i+1:]
		b.lines = append(b.lines, line)
		if len(b.lines) > b.max {
			b.lines = b.lines[len(b.lines)-b.max:]
		}
		for ch := range b.subs {
			select {
			case ch <- line:
			default: // slow reader; drop rather than block the container
			}
		}
	}
	b.partial = append([]byte(nil), data...)
	return len(p), nil
}

// Subscribe returns the buffered lines and a channel of new ones.
func (b *LogBuffer) Subscribe() (backlog [][]byte, ch chan []byte, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	backlog = append([][]byte(nil), b.lines...)
	ch = make(chan []byte, 256)
	b.subs[ch] = struct{}{}
	return backlog, ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

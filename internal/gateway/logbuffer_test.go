package gateway

import (
	"bytes"
	"sync"
)

// logBuffer collects log output safely: the gateway logs from server and
// health-check goroutines while a test reads what has been written so far.
// (A plain bytes.Buffer here is a data race; -race caught it.)
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

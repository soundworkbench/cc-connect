package core

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

var streamTrace struct {
	mu sync.Mutex
	f  *os.File
}

// SetStreamTraceFile starts (or, with an empty path, stops) appending one JSON
// line per streaming event: upstream text deltas, preview body updates and card
// API calls. It exists to measure how smooth a streamed reply is.
func SetStreamTraceFile(path string) error {
	streamTrace.mu.Lock()
	defer streamTrace.mu.Unlock()
	if streamTrace.f != nil {
		streamTrace.f.Close()
		streamTrace.f = nil
	}
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	streamTrace.f = f
	return nil
}

// StreamTraceOn reports whether tracing is active.
func StreamTraceOn() bool {
	streamTrace.mu.Lock()
	defer streamTrace.mu.Unlock()
	return streamTrace.f != nil
}

// StreamTrace records one event with the current time in unix milliseconds.
func StreamTrace(kind string, fields map[string]any) {
	streamTrace.mu.Lock()
	defer streamTrace.mu.Unlock()
	if streamTrace.f == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["k"] = kind
	fields["t"] = float64(time.Now().UnixMicro()) / 1000
	b, _ := json.Marshal(fields)
	streamTrace.f.Write(append(b, '\n'))
}

package continuation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Tracing records the exact body each provider request was compiled into.
//
// It exists because the interesting failures in a layered system are almost
// never in the code that decides - they are in what the deciding model was
// shown. A signal that arrived late, a field that went stale, a transcript
// that was dropped the instant its speaker stopped: each of those produces a
// perfectly reasonable answer to a question nobody meant to ask, and none of
// them is visible from the outside. The only way to see them is to read the
// bytes that went over the wire.
//
// It records the compiled body rather than the trajectory it came from,
// because those differ, and the difference is exactly where this class of bug
// lives. Off unless OPENREALTIME_CONTEXT_TRACE names a directory.
var (
	traceOnce sync.Once
	traceDir  string
	traceSeq  atomic.Uint64
	traceMu   sync.Mutex
)

func traceTarget() string {
	traceOnce.Do(func() {
		traceDir = os.Getenv("OPENREALTIME_CONTEXT_TRACE")
		if traceDir == "" {
			return
		}
		if err := os.MkdirAll(traceDir, 0o755); err != nil {
			traceDir = ""
		}
	})
	return traceDir
}

// TraceRequest records one compiled request. It is a no-op unless tracing is
// configured, and it never returns an error: a trace that could fail a request
// would be a debugging aid that changes what it is trying to observe.
func TraceRequest(descriptor Descriptor, invocationID string, body []byte) {
	target := traceTarget()
	if target == "" {
		return
	}
	entry := map[string]any{
		"seq":        traceSeq.Add(1),
		"at":         time.Now().UTC().Format(time.RFC3339Nano),
		"phase":      string(descriptor.Phase),
		"provider":   descriptor.Provider,
		"model":      descriptor.Model,
		"invocation": invocationID,
		"body":       json.RawMessage(body),
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	file, err := os.OpenFile(filepath.Join(target, "context.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s\n", encoded)
}

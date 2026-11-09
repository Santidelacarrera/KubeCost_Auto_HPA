package execute

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Event is one audit record. Secrets never go in Details.
type Event struct {
	Time             time.Time      `json:"time"`
	Type             string         `json:"type"` // decision, dry_run, applied, verified, degraded, rolled_back, apply_failed, rollback_failed, inspect_failed
	Actor            string         `json:"actor"`
	RecommendationID string         `json:"recommendationId"`
	Target           string         `json:"target"`
	Details          map[string]any `json:"details,omitempty"`
}

// Sink receives audit events. Implementations must not block the caller for long.
type Sink interface{ Record(Event) }

// JSONL writes one JSON object per line (append-only by construction).
type JSONL struct {
	mu sync.Mutex
	W  io.Writer
}

func (j *JSONL) Record(e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, _ = j.W.Write(append(b, '\n'))
}

// Memory keeps events in memory (tests).
type Memory struct {
	mu     sync.Mutex
	Events []Event
}

func (m *Memory) Record(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Events = append(m.Events, e)
}

// Types returns the event types in order.
func (m *Memory) Types() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.Events))
	for i, e := range m.Events {
		out[i] = e.Type
	}
	return out
}

// Multi fans out to several sinks.
type Multi []Sink

func (m Multi) Record(e Event) {
	for _, s := range m {
		s.Record(e)
	}
}

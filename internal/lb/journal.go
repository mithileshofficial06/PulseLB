package lb

import (
	"fmt"
	"sync"
	"time"
)

// Event levels.
const (
	LevelInfo = "info"
	LevelGood = "good"
	LevelWarn = "warn"
	LevelCrit = "crit"
)

// Event is one entry in the decision journal: a plain-English explanation of
// something PulseLB decided or noticed.
type Event struct {
	Seq   uint64 `json:"seq"`
	T     int64  `json:"t"` // unix ms
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Journal keeps the most recent events in a ring buffer.
type Journal struct {
	mu     sync.Mutex
	events []Event
	seq    uint64
	last   map[string]time.Time
}

const journalSize = 300

func newJournal() *Journal {
	return &Journal{last: make(map[string]time.Time)}
}

// Add records an event.
func (j *Journal) Add(level, format string, args ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	e := Event{Seq: j.seq, T: time.Now().UnixMilli(), Level: level, Msg: fmt.Sprintf(format, args...)}
	if len(j.events) >= journalSize {
		copy(j.events, j.events[1:])
		j.events[len(j.events)-1] = e
	} else {
		j.events = append(j.events, e)
	}
}

// AddLimited records an event unless one with the same key was recorded
// within the last interval.
func (j *Journal) AddLimited(key string, every time.Duration, level, format string, args ...any) {
	j.mu.Lock()
	if t, ok := j.last[key]; ok && time.Since(t) < every {
		j.mu.Unlock()
		return
	}
	j.last[key] = time.Now()
	j.mu.Unlock()
	j.Add(level, format, args...)
}

// Since returns events with Seq > seq, oldest first.
func (j *Journal) Since(seq uint64) []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := []Event{}
	for _, e := range j.events {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out
}

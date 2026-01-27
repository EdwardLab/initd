package logging

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type Level string

const (
	LevelInfo  Level = "INFO"
	LevelError Level = "ERROR"
)

type Entry struct {
	Timestamp time.Duration
	Unit      string
	PID       int
	Level     Level
	Message   string
}

type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	max     int
}

func NewBuffer(maxEntries int) *Buffer {
	return &Buffer{
		entries: make([]Entry, 0, maxEntries),
		max:     maxEntries,
	}
}

func (b *Buffer) Add(entry Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max <= 0 {
		return
	}
	if len(b.entries) >= b.max {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:b.max-1]
	}
	b.entries = append(b.entries, entry)
}

func (b *Buffer) Entries() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries := make([]Entry, len(b.entries))
	copy(entries, b.entries)
	return entries
}

type LineLogger struct {
	Unit   string
	PID    int
	Level  Level
	Buffer *Buffer
	Output io.Writer
}

func (l *LineLogger) Write(p []byte) (int, error) {
	if l.Buffer == nil {
		return len(p), nil
	}
	reader := bufio.NewScanner(strings.NewReader(string(p)))
	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}
		entry := Entry{
			Timestamp: MonotonicNow(),
			Unit:      l.Unit,
			PID:       l.PID,
			Level:     l.Level,
			Message:   line,
		}
		l.Buffer.Add(entry)
		if l.Output != nil {
			_, _ = fmt.Fprintf(l.Output, "%s\n", FormatEntry(entry))
		}
	}
	return len(p), nil
}

func FormatEntry(entry Entry) string {
	return fmt.Sprintf("[%s] %s[%d]: %s", formatMonotonic(entry.Timestamp), entry.Unit, entry.PID, entry.Message)
}

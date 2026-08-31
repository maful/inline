package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type logEntry struct {
	sequence   uint64
	raw        string
	searchable string
}

// logBuffer keeps a bounded history and a sorted index of the entries that
// match the process's current filter.
type logBuffer struct {
	entries         []logEntry
	start           int
	length          int
	nextSequence    uint64
	query           string
	normalizedQuery string
	matches         []uint64
	matchStart      int
}

func newLogBuffer(capacity int) logBuffer {
	return logBuffer{entries: make([]logEntry, max(1, capacity))}
}

func (b *logBuffer) append(raw string) bool {
	entry := logEntry{
		sequence:   b.nextSequence,
		raw:        raw,
		searchable: normalizeForSearch(raw),
	}
	b.nextSequence++
	visibleChanged := b.normalizedQuery == ""

	if b.length == len(b.entries) {
		evicted := b.entries[b.start]
		visibleChanged = b.dropMatchThrough(evicted.sequence) || visibleChanged
		b.entries[b.start] = entry
		b.start = (b.start + 1) % len(b.entries)
	} else {
		index := (b.start + b.length) % len(b.entries)
		b.entries[index] = entry
		b.length++
	}

	if b.normalizedQuery != "" && strings.Contains(entry.searchable, b.normalizedQuery) {
		b.matches = append(b.matches, entry.sequence)
		visibleChanged = true
	}
	return visibleChanged
}

func (b *logBuffer) setQuery(query string) bool {
	normalized := normalizeForSearch(query)
	if query == b.query {
		return false
	}
	if normalized == b.normalizedQuery {
		b.query = query
		return true
	}

	previous := b.normalizedQuery
	b.query = query
	b.normalizedQuery = normalized

	if normalized == "" {
		b.matches = nil
		b.matchStart = 0
		return true
	}

	matches := make([]uint64, 0)
	if previous != "" && strings.HasPrefix(normalized, previous) {
		matches = make([]uint64, 0, b.matchCount())
		for _, sequence := range b.activeMatches() {
			entry, ok := b.entry(sequence)
			if ok && strings.Contains(entry.searchable, normalized) {
				matches = append(matches, sequence)
			}
		}
	} else {
		matches = make([]uint64, 0, b.length)
		b.each(func(entry logEntry) {
			if strings.Contains(entry.searchable, normalized) {
				matches = append(matches, entry.sequence)
			}
		})
	}

	b.matches = matches
	b.matchStart = 0
	return true
}

func (b *logBuffer) count() int {
	return b.length
}

func (b *logBuffer) visibleCount() int {
	if b.normalizedQuery == "" {
		return b.length
	}
	return b.matchCount()
}

func (b *logBuffer) visibleLines() []string {
	lines := make([]string, 0, b.visibleCount())
	if b.normalizedQuery == "" {
		b.each(func(entry logEntry) {
			lines = append(lines, entry.raw)
		})
		return lines
	}

	for _, sequence := range b.activeMatches() {
		if entry, ok := b.entry(sequence); ok {
			lines = append(lines, entry.raw)
		}
	}
	return lines
}

func (b *logBuffer) each(yield func(logEntry)) {
	for offset := 0; offset < b.length; offset++ {
		yield(b.entries[(b.start+offset)%len(b.entries)])
	}
}

func (b *logBuffer) entry(sequence uint64) (logEntry, bool) {
	if b.length == 0 {
		return logEntry{}, false
	}
	oldest := b.entries[b.start].sequence
	if sequence < oldest {
		return logEntry{}, false
	}
	offset := sequence - oldest
	if offset >= uint64(b.length) {
		return logEntry{}, false
	}
	return b.entries[(b.start+int(offset))%len(b.entries)], true
}

func (b *logBuffer) activeMatches() []uint64 {
	return b.matches[b.matchStart:]
}

func (b *logBuffer) matchCount() int {
	return len(b.matches) - b.matchStart
}

func (b *logBuffer) dropMatchThrough(sequence uint64) bool {
	dropped := false
	for b.matchStart < len(b.matches) && b.matches[b.matchStart] <= sequence {
		b.matchStart++
		dropped = true
	}
	if b.matchStart >= 1024 && b.matchStart*2 >= len(b.matches) {
		b.matches = append([]uint64(nil), b.matches[b.matchStart:]...)
		b.matchStart = 0
	}
	return dropped
}

func normalizeForSearch(value string) string {
	return strings.ToLower(ansi.Strip(value))
}

package ui

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

type logEntry struct {
	sequence   uint64
	raw        string
	searchable string
}

type matchSpan struct {
	sequence uint64
	start    int
	end      int
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
	occurrences     []matchSpan
	occurrenceStart int
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
		b.dropOccurrenceThrough(evicted.sequence)
		b.entries[b.start] = entry
		b.start = (b.start + 1) % len(b.entries)
	} else {
		index := (b.start + b.length) % len(b.entries)
		b.entries[index] = entry
		b.length++
	}

	spans := findOccurrenceSpans(entry.sequence, entry.searchable, b.normalizedQuery)
	if len(spans) > 0 {
		b.matches = append(b.matches, entry.sequence)
		b.occurrences = append(b.occurrences, spans...)
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
		b.occurrences = nil
		b.occurrenceStart = 0
		return true
	}

	matches := make([]uint64, 0)
	occurrences := make([]matchSpan, 0)
	if previous != "" && strings.HasPrefix(normalized, previous) {
		matches = make([]uint64, 0, b.matchCount())
		for _, sequence := range b.activeMatches() {
			entry, ok := b.entry(sequence)
			if !ok {
				continue
			}
			spans := findOccurrenceSpans(sequence, entry.searchable, normalized)
			if len(spans) > 0 {
				matches = append(matches, sequence)
				occurrences = append(occurrences, spans...)
			}
		}
	} else {
		matches = make([]uint64, 0, b.length)
		b.each(func(entry logEntry) {
			spans := findOccurrenceSpans(entry.sequence, entry.searchable, normalized)
			if len(spans) > 0 {
				matches = append(matches, entry.sequence)
				occurrences = append(occurrences, spans...)
			}
		})
	}

	b.matches = matches
	b.matchStart = 0
	b.occurrences = occurrences
	b.occurrenceStart = 0
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
	entries := b.visibleEntries()
	lines := make([]string, len(entries))
	for index, entry := range entries {
		lines[index] = entry.raw
	}
	return lines
}

func (b *logBuffer) visibleEntries() []logEntry {
	entries := make([]logEntry, 0, b.visibleCount())
	if b.normalizedQuery == "" {
		b.each(func(entry logEntry) {
			entries = append(entries, entry)
		})
		return entries
	}

	for _, sequence := range b.activeMatches() {
		if entry, ok := b.entry(sequence); ok {
			entries = append(entries, entry)
		}
	}
	return entries
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

func (b *logBuffer) activeOccurrences() []matchSpan {
	return b.occurrences[b.occurrenceStart:]
}

func (b *logBuffer) occurrenceCount() int {
	return len(b.occurrences) - b.occurrenceStart
}

func (b *logBuffer) occurrenceIndex(target matchSpan) int {
	occurrences := b.activeOccurrences()
	index := sort.Search(len(occurrences), func(index int) bool {
		candidate := occurrences[index]
		return candidate.sequence > target.sequence ||
			(candidate.sequence == target.sequence && candidate.start >= target.start)
	})
	if index < len(occurrences) && occurrences[index] == target {
		return index
	}
	return -1
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

func (b *logBuffer) dropOccurrenceThrough(sequence uint64) {
	for b.occurrenceStart < len(b.occurrences) && b.occurrences[b.occurrenceStart].sequence <= sequence {
		b.occurrenceStart++
	}
	if b.occurrenceStart >= 1024 && b.occurrenceStart*2 >= len(b.occurrences) {
		b.occurrences = append([]matchSpan(nil), b.occurrences[b.occurrenceStart:]...)
		b.occurrenceStart = 0
	}
}

func findOccurrenceSpans(sequence uint64, searchable, query string) []matchSpan {
	if query == "" {
		return nil
	}

	spans := make([]matchSpan, 0, 1)
	byteOffset := 0
	runeOffset := 0
	for byteOffset < len(searchable) {
		relative := strings.Index(searchable[byteOffset:], query)
		if relative < 0 {
			break
		}
		startByte := byteOffset + relative
		endByte := startByte + len(query)
		runeOffset += utf8.RuneCountInString(searchable[byteOffset:startByte])
		matchRunes := utf8.RuneCountInString(searchable[startByte:endByte])
		spans = append(spans, matchSpan{
			sequence: sequence,
			start:    runeOffset,
			end:      runeOffset + matchRunes,
		})
		byteOffset = endByte
		runeOffset += matchRunes
	}
	return spans
}

func normalizeForSearch(value string) string {
	return strings.ToLower(ansi.Strip(value))
}

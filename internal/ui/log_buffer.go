package ui

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const maxIndexedOccurrences = 10_000

type logEntry struct {
	sequence   uint64
	arrival    uint64
	raw        string
	searchable string
}

type logMatch struct {
	sequence    uint64
	occurrences int
}

type matchSpan struct {
	sequence uint64
	start    int
	end      int
}

// logBuffer keeps history bounded by both entry count and retained string
// bytes. Its occurrence span cache has a separate fixed upper bound.
type logBuffer struct {
	entries          []logEntry
	start            int
	length           int
	nextSequence     uint64
	retainedBytes    int
	maxRetainedBytes int
	query            string
	normalizedQuery  string
	matches          []logMatch
	matchStart       int
	occurrences      []matchSpan
	occurrenceStart  int
	totalOccurrences int
}

func newLogBuffer(capacity int) logBuffer {
	return newLogBufferWithBudget(capacity, maxProcessLogBytes)
}

func newLogBufferWithBudget(capacity, maxBytes int) logBuffer {
	return logBuffer{
		entries:          make([]logEntry, max(1, capacity)),
		maxRetainedBytes: max(1, maxBytes),
	}
}

func (b *logBuffer) clear() {
	clear(b.entries)
	b.start = 0
	b.length = 0
	b.retainedBytes = 0
	b.matches = nil
	b.matchStart = 0
	b.occurrences = nil
	b.occurrenceStart = 0
	b.totalOccurrences = 0
}

func (b *logBuffer) append(raw string) bool {
	return b.appendWithArrival(raw, b.nextSequence)
}

func (b *logBuffer) appendWithArrival(raw string, arrival uint64) bool {
	raw = truncateLogLine(sanitizeLogLine(raw), maxLogLineBytes)
	entry := logEntry{
		sequence:   b.nextSequence,
		arrival:    arrival,
		raw:        raw,
		searchable: normalizeForSearch(raw),
	}
	b.nextSequence++
	visibleChanged := b.normalizedQuery == ""

	for b.length > 0 && (b.length == len(b.entries) || b.retainedBytes+entry.retainedBytes() > b.maxRetainedBytes) {
		_, changed := b.evictOldest()
		visibleChanged = changed || visibleChanged
	}

	index := (b.start + b.length) % len(b.entries)
	b.entries[index] = entry
	b.length++
	b.retainedBytes += entry.retainedBytes()

	remaining := maxIndexedOccurrences - b.occurrenceCount()
	spans, count := findOccurrenceSpans(entry.sequence, entry.searchable, b.normalizedQuery, remaining)
	if count > 0 {
		b.matches = append(b.matches, logMatch{sequence: entry.sequence, occurrences: count})
		b.totalOccurrences += count
		b.appendOccurrences(spans)
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
		b.totalOccurrences = 0
		return true
	}

	matches := make([]logMatch, 0)
	occurrences := make([]matchSpan, 0, maxIndexedOccurrences)
	totalOccurrences := 0
	indexEntry := func(entry logEntry) {
		remaining := maxIndexedOccurrences - len(occurrences)
		spans, count := findOccurrenceSpans(entry.sequence, entry.searchable, normalized, remaining)
		if count == 0 {
			return
		}
		matches = append(matches, logMatch{sequence: entry.sequence, occurrences: count})
		occurrences = append(occurrences, spans...)
		totalOccurrences += count
	}

	if previous != "" && strings.HasPrefix(normalized, previous) {
		matches = make([]logMatch, 0, b.matchCount())
		for _, match := range b.activeMatches() {
			if entry, ok := b.entry(match.sequence); ok {
				indexEntry(entry)
			}
		}
	} else {
		matches = make([]logMatch, 0, b.length)
		b.each(indexEntry)
	}

	b.matches = matches
	b.matchStart = 0
	b.occurrences = occurrences
	b.occurrenceStart = 0
	b.totalOccurrences = totalOccurrences
	return true
}

func (b *logBuffer) count() int { return b.length }

func (b *logBuffer) byteCount() int { return b.retainedBytes }

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
		b.each(func(entry logEntry) { entries = append(entries, entry) })
		return entries
	}

	for _, match := range b.activeMatches() {
		if entry, ok := b.entry(match.sequence); ok {
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

func (b *logBuffer) oldest() (logEntry, bool) {
	if b.length == 0 {
		return logEntry{}, false
	}
	return b.entries[b.start], true
}

func (b *logBuffer) evictOldest() (logEntry, bool) {
	entry, ok := b.oldest()
	if !ok {
		return logEntry{}, false
	}
	visibleChanged := b.normalizedQuery == ""
	visibleChanged = b.dropMatchThrough(entry.sequence) || visibleChanged
	b.dropOccurrenceThrough(entry.sequence)
	b.retainedBytes -= entry.retainedBytes()
	b.entries[b.start] = logEntry{}
	b.start = (b.start + 1) % len(b.entries)
	b.length--
	return entry, visibleChanged
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

func (e logEntry) retainedBytes() int { return len(e.raw) + len(e.searchable) }

func (b *logBuffer) activeMatches() []logMatch { return b.matches[b.matchStart:] }

func (b *logBuffer) matchCount() int { return len(b.matches) - b.matchStart }

func (b *logBuffer) activeOccurrences() []matchSpan {
	return b.occurrences[b.occurrenceStart:]
}

func (b *logBuffer) occurrenceCount() int { return len(b.occurrences) - b.occurrenceStart }

func (b *logBuffer) totalOccurrenceCount() int { return b.totalOccurrences }

func (b *logBuffer) occurrencesLimited() bool {
	return b.totalOccurrences > b.occurrenceCount()
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

func (b *logBuffer) occurrencesFor(sequence uint64) ([]matchSpan, int) {
	occurrences := b.activeOccurrences()
	start := sort.Search(len(occurrences), func(index int) bool {
		return occurrences[index].sequence >= sequence
	})
	end := start
	for end < len(occurrences) && occurrences[end].sequence == sequence {
		end++
	}
	return occurrences[start:end], start
}

func (b *logBuffer) dropMatchThrough(sequence uint64) bool {
	dropped := false
	for b.matchStart < len(b.matches) && b.matches[b.matchStart].sequence <= sequence {
		b.totalOccurrences -= b.matches[b.matchStart].occurrences
		b.matchStart++
		dropped = true
	}
	if b.matchStart >= 1024 && b.matchStart*2 >= len(b.matches) {
		b.matches = append([]logMatch(nil), b.matches[b.matchStart:]...)
		b.matchStart = 0
	}
	return dropped
}

func (b *logBuffer) dropOccurrenceThrough(sequence uint64) {
	for b.occurrenceStart < len(b.occurrences) && b.occurrences[b.occurrenceStart].sequence <= sequence {
		b.occurrenceStart++
	}
	if b.occurrenceStart >= 1024 && b.occurrenceStart*2 >= len(b.occurrences) {
		b.compactOccurrences()
	}
}

func (b *logBuffer) appendOccurrences(spans []matchSpan) {
	if len(spans) == 0 {
		return
	}
	if len(b.occurrences)+len(spans) > cap(b.occurrences) && b.occurrenceStart > 0 {
		b.compactOccurrences()
	}
	if b.occurrences == nil {
		b.occurrences = make([]matchSpan, 0, maxIndexedOccurrences)
	}
	b.occurrences = append(b.occurrences, spans...)
}

func (b *logBuffer) compactOccurrences() {
	active := b.activeOccurrences()
	copy(b.occurrences, active)
	b.occurrences = b.occurrences[:len(active)]
	b.occurrenceStart = 0
}

func findOccurrenceSpans(sequence uint64, searchable, query string, limit int) ([]matchSpan, int) {
	if query == "" {
		return nil, 0
	}

	spans := make([]matchSpan, 0, min(1, max(0, limit)))
	byteOffset := 0
	runeOffset := 0
	total := 0
	for byteOffset < len(searchable) {
		relative := strings.Index(searchable[byteOffset:], query)
		if relative < 0 {
			break
		}
		startByte := byteOffset + relative
		endByte := startByte + len(query)
		total++
		if len(spans) < limit {
			runeOffset += utf8.RuneCountInString(searchable[byteOffset:startByte])
			matchRunes := utf8.RuneCountInString(searchable[startByte:endByte])
			spans = append(spans, matchSpan{
				sequence: sequence,
				start:    runeOffset,
				end:      runeOffset + matchRunes,
			})
			runeOffset += matchRunes
		}
		byteOffset = endByte
	}
	return spans, total
}

func normalizeForSearch(value string) string {
	return strings.ToLower(ansi.Strip(value))
}

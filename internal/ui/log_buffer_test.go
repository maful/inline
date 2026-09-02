package ui

import (
	"fmt"
	"slices"
	"testing"
)

func TestLogBufferRetainsNewestLinesInOrder(t *testing.T) {
	buffer := newLogBuffer(3)
	for _, line := range []string{"one", "two", "three", "four"} {
		buffer.append(line)
	}

	if got, want := buffer.visibleLines(), []string{"two", "three", "four"}; !slices.Equal(got, want) {
		t.Fatalf("visible lines = %v, want %v", got, want)
	}
}

func TestLogBufferMatchesCaseInsensitivelyWithoutANSI(t *testing.T) {
	buffer := newLogBuffer(10)
	buffer.append("\x1b[31mERROR\x1b[0m database unavailable")
	buffer.append("request complete")

	buffer.setQuery("error")

	if got, want := buffer.visibleLines(), []string{"\x1b[31mERROR\x1b[0m database unavailable"}; !slices.Equal(got, want) {
		t.Fatalf("visible lines = %q, want %q", got, want)
	}
}

func TestLogBufferIndexesEveryOccurrenceWithoutANSIOffsets(t *testing.T) {
	buffer := newLogBuffer(10)
	buffer.append("\x1b[31mERROR\x1b[0m then Error and café CAFÉ")

	buffer.setQuery("error")
	if got, want := buffer.activeOccurrences(), []matchSpan{
		{sequence: 0, start: 0, end: 5},
		{sequence: 0, start: 11, end: 16},
	}; !slices.Equal(got, want) {
		t.Fatalf("error occurrences = %#v, want %#v", got, want)
	}

	buffer.setQuery("café")
	if got, want := buffer.activeOccurrences(), []matchSpan{
		{sequence: 0, start: 21, end: 25},
		{sequence: 0, start: 26, end: 30},
	}; !slices.Equal(got, want) {
		t.Fatalf("unicode occurrences = %#v, want %#v", got, want)
	}
}

func TestLogBufferUpdatesMatchesForQueryChangesAndNewLines(t *testing.T) {
	buffer := newLogBuffer(10)
	for _, line := range []string{"server error", "worker error", "server ready"} {
		buffer.append(line)
	}

	buffer.setQuery("error")
	buffer.setQuery("server error")
	buffer.append("server error: timeout")
	if got, want := buffer.visibleLines(), []string{"server error", "server error: timeout"}; !slices.Equal(got, want) {
		t.Fatalf("narrowed visible lines = %v, want %v", got, want)
	}

	buffer.setQuery("server")
	if got, want := buffer.visibleLines(), []string{"server error", "server ready", "server error: timeout"}; !slices.Equal(got, want) {
		t.Fatalf("rescanned visible lines = %v, want %v", got, want)
	}

	buffer.setQuery("")
	if got := buffer.visibleCount(); got != 4 {
		t.Fatalf("visible count after clearing = %d, want 4", got)
	}
}

func TestLogBufferDropsEvictedMatches(t *testing.T) {
	buffer := newLogBuffer(2)
	buffer.setQuery("error")
	buffer.append("old error")
	buffer.append("ready")
	buffer.append("new error")

	if got, want := buffer.visibleLines(), []string{"new error"}; !slices.Equal(got, want) {
		t.Fatalf("visible lines = %v, want %v", got, want)
	}
}

func TestLogBufferDropsEveryOccurrenceFromEvictedLines(t *testing.T) {
	buffer := newLogBuffer(2)
	buffer.setQuery("error")
	buffer.append("old error error")
	buffer.append("ready")
	buffer.append("new error")

	if got, want := buffer.activeOccurrences(), []matchSpan{{sequence: 2, start: 4, end: 9}}; !slices.Equal(got, want) {
		t.Fatalf("active occurrences = %#v, want %#v", got, want)
	}
}

func TestLogBufferReportsOnlyVisibleChangesWhileFiltered(t *testing.T) {
	buffer := newLogBuffer(2)
	buffer.setQuery("error")
	if changed := buffer.append("ready"); changed {
		t.Fatal("unmatched append reported a visible change")
	}
	if changed := buffer.append("new error"); !changed {
		t.Fatal("matched append did not report a visible change")
	}
	if changed := buffer.append("still ready"); changed {
		t.Fatal("evicting an unmatched line reported a visible change")
	}
	if changed := buffer.append("done"); !changed {
		t.Fatal("evicting a matched line did not report a visible change")
	}
}

func BenchmarkLogBufferFilter20K(b *testing.B) {
	buffer := newLogBuffer(maxLogLines)
	for index := 0; index < maxLogLines; index++ {
		buffer.append(fmt.Sprintf("request=%d status=%d path=/api/orders", index, 200+index%5))
	}

	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		buffer.setQuery(fmt.Sprintf("status=%d", 200+index%5))
	}
}

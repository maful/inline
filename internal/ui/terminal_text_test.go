package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/maful/inline/internal/process"
	"github.com/maful/inline/internal/procfile"
)

func TestSanitizeLogLinePreservesTextAndSGR(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "printable Unicode and tab", input: "ready\tcafé 👩‍💻", want: "ready\tcafé 👩‍💻"},
		{name: "basic color and reset", input: "\x1b[1;31merror\x1b[0m", want: "\x1b[1;31merror\x1b[0m"},
		{name: "256 color", input: "\x1b[38;5;214mwarning", want: "\x1b[38;5;214mwarning\x1b[0m"},
		{name: "true color with colon syntax", input: "\x1b[38:2::12:34:56mvalue", want: "\x1b[38:2::12:34:56mvalue\x1b[0m"},
		{name: "invalid UTF-8", input: string([]byte{'a', 0xff, 'b'}), want: "a�b"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeLogLine(test.input); got != test.want {
				t.Fatalf("sanitizeLogLine() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSanitizeLogLineDiscardsTerminalControls(t *testing.T) {
	tests := []struct {
		name    string
		control string
	}{
		{name: "OSC 52 with BEL", control: "\x1b]52;c;dGVzdA==\x07"},
		{name: "window title with ST", control: "\x1b]0;inline-test\x1b\\"},
		{name: "hyperlink", control: "\x1b]8;;https://example.com\x1b\\"},
		{name: "screen erase", control: "\x1b[2J"},
		{name: "cursor movement", control: "\x1b[4A"},
		{name: "terminal mode", control: "\x1b[?25l"},
		{name: "device control string", control: "\x1bP1;2|payload\x1b\\"},
		{name: "application program command", control: "\x1b_payload\x1b\\"},
		{name: "privacy message", control: "\x1b^payload\x1b\\"},
		{name: "start of string", control: "\x1bXpayload\x1b\\"},
		{name: "C0 controls", control: "\a\b\r\n"},
		{name: "C1 OSC", control: string([]byte{oscByte}) + "0;title" + string([]byte{stByte})},
		{name: "C1 CSI", control: string([]byte{csiByte}) + "2J"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "before" + test.control + "after"
			if got, want := sanitizeLogLine(input), "beforeafter"; got != want {
				t.Fatalf("sanitizeLogLine() = %q, want %q", got, want)
			}
		})
	}
}

func TestSanitizeLogLineDiscardsIncompleteSequences(t *testing.T) {
	for _, input := range []string{
		"prefix\x1b",
		"prefix\x1b[31",
		"prefix\x1b]52;c;payload",
		"prefix\x1bPpayload",
	} {
		if got, want := sanitizeLogLine(input), "prefix"; got != want {
			t.Errorf("sanitizeLogLine(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSanitizeDisplayTextRemovesStylesAndWhitespaceControls(t *testing.T) {
	input := "web\t\x1b[31mred\x1b[0m\n"
	if got, want := sanitizeDisplayText(input), "webred"; got != want {
		t.Fatalf("sanitizeDisplayText() = %q, want %q", got, want)
	}
}

func TestValidSGRSequenceRejectsOtherCSIAndOversizedParameters(t *testing.T) {
	if validSGRSequence("\x1b[2J") || validSGRSequence("\x1b[?25m") {
		t.Fatal("accepted a non-SGR control sequence")
	}
	if validSGRSequence("\x1b[" + strings.Repeat("1", maxSGRSequenceBytes) + "m") {
		t.Fatal("accepted an oversized SGR sequence")
	}
}

func TestModelSanitizesProcessOutputBeforeStorageAndRendering(t *testing.T) {
	payloads := map[string]string{
		"OSC 52":       "\x1b]52;c;dGVzdA==\x07",
		"screen erase": "\x1b[2J",
		"window title": "\x1b]0;inline-test\x07",
	}

	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			model := newTestModel()
			updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			model = updated.(Model)
			updated, _ = model.Update(process.Event{Index: 0, Line: "before" + payload + "after"})
			model = updated.(Model)

			if got, want := model.processes[0].logs.visibleLines(), []string{"beforeafter"}; len(got) != 1 || got[0] != want[0] {
				t.Fatalf("stored logs = %q, want %q", got, want)
			}
			if strings.Contains(model.View(), payload) {
				t.Fatal("control sequence survived the unfiltered view")
			}

			model.processes[0].logs.setQuery("beforeafter")
			model.processes[0].dirty = true
			model.refreshSelected()
			if strings.Contains(model.View(), payload) {
				t.Fatal("control sequence survived the filtered view")
			}
		})
	}
}

func TestModelSanitizesDisplayMetadataWithoutChangingSourceDefinitions(t *testing.T) {
	payload := "\x1b]0;inline-test\x07"
	definitions := []procfile.Process{{Name: "web" + payload, Command: "echo" + payload + " done"}}
	original := definitions[0]
	model := New(
		definitions,
		&fakeSource{events: make(chan process.Event)},
		"Procfile"+payload,
		"/tmp/project"+payload,
		"main"+payload,
		"v1"+payload,
	)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	view := updated.(Model).View()

	if strings.Contains(view, payload) {
		t.Fatal("control sequence survived metadata rendering")
	}
	plain := ansi.Strip(view)
	for _, want := range []string{"web", "$ echo done", "Procfile", "/tmp/project", "main", "v1"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered metadata does not contain %q", want)
		}
	}
	if definitions[0] != original {
		t.Fatalf("source definition changed from %#v to %#v", original, definitions[0])
	}
}

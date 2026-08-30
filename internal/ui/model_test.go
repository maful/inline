package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/maful/inline/internal/process"
	"github.com/maful/inline/internal/procfile"
)

type fakeSource struct{ events chan process.Event }

func (f *fakeSource) StartAll()                    {}
func (f *fakeSource) Events() <-chan process.Event { return f.events }

func newTestModel() Model {
	definitions := []procfile.Process{
		{Name: "web", Command: "bin/rails server"},
		{Name: "worker", Command: "bundle exec sidekiq"},
	}
	return New(definitions, &fakeSource{events: make(chan process.Event)}, "Procfile")
}

func TestProcessNavigation(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	if model.selected != 1 {
		t.Fatalf("selected = %d, want 1", model.selected)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(Model)
	if model.selected != 0 {
		t.Fatalf("selected after up = %d, want 0", model.selected)
	}
}

func TestEventAppearsOnlyInItsProcess(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 1, Line: "sidekiq ready", State: process.Running, PID: 4242})
	model = updated.(Model)

	if len(model.processes[0].lines) != 0 {
		t.Fatalf("web lines = %v, want none", model.processes[0].lines)
	}
	if got := strings.Join(model.processes[1].lines, "\n"); got != "sidekiq ready" {
		t.Fatalf("worker lines = %q", got)
	}
	if !model.processes[1].follow || !model.processes[1].viewport.AtBottom() {
		t.Fatal("worker viewport should follow new output")
	}
	if model.processes[1].pid != 4242 {
		t.Fatalf("worker pid = %d, want 4242", model.processes[1].pid)
	}
}

func TestQuietProcessStillShowsRunningStatus(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 0, State: process.Running, PID: 1234})
	view := updated.(Model).View()

	if !strings.Contains(view, "running · pid 1234 · 0 lines · waiting for output") {
		t.Fatalf("View() does not show running PID:\n%s", view)
	}
	if !strings.Contains(view, "No output yet") {
		t.Fatalf("View() does not explain the empty pane:\n%s", view)
	}
}

func TestStatusShowsReceivedLineCount(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 0, State: process.Running, PID: 1234, Line: "first"})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 0, Line: "second"})
	view := updated.(Model).View()

	if !strings.Contains(view, "running · pid 1234 · 2 lines") {
		t.Fatalf("View() does not show received line count:\n%s", view)
	}
	if strings.Contains(view, "waiting for output") {
		t.Fatalf("View() still says waiting after receiving output:\n%s", view)
	}
}

func TestLongANSIOutputWrapsToViewportWidth(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model = updated.(Model)
	line := "\x1b[36mSELECT\x1b[0m merchants.id, merchants.active_at FROM merchants WHERE identifier=abcdefghijklmnopqrstuvwxyz"
	updated, _ = model.Update(process.Event{Index: 0, State: process.Running, PID: 1234, Line: line})
	model = updated.(Model)

	wrapped := model.processes[0].rendered[0]
	visualLines := strings.Split(wrapped, "\n")
	if len(visualLines) < 2 {
		t.Fatalf("wrapped output has %d visual line, want at least 2: %q", len(visualLines), wrapped)
	}
	for _, visualLine := range visualLines {
		if width := lipgloss.Width(visualLine); width > model.processes[0].viewport.Width {
			t.Fatalf("wrapped line width = %d, viewport width = %d: %q", width, model.processes[0].viewport.Width, visualLine)
		}
	}
}

func TestResizeRewrapsExistingOutput(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 0, Line: strings.Repeat("x", 60)})
	model = updated.(Model)
	if strings.Contains(model.processes[0].rendered[0], "\n") {
		t.Fatal("line unexpectedly wrapped at the initial width")
	}

	updated, _ = model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	model = updated.(Model)
	if !strings.Contains(model.processes[0].rendered[0], "\n") {
		t.Fatal("line was not rewrapped after narrowing the terminal")
	}
}

func TestViewContainsProcessNamesAndCommand(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := updated.(Model).View()
	for _, want := range []string{"inline", "web", "worker", "$ bin/rails server", "following"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() does not contain %q", want)
		}
	}
	if width := lipgloss.Width(view); width != 100 {
		t.Errorf("View() width = %d, want 100", width)
	}
	if height := lipgloss.Height(view); height != 30 {
		t.Errorf("View() height = %d, want 30", height)
	}
}

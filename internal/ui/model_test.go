package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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
	return New(definitions, &fakeSource{events: make(chan process.Event)}, "Procfile", "/Users/example/project", "v1.2.3")
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
	for _, want := range []string{"inline", "web", "worker", "$ bin/rails server", "v1.2.3", "following"} {
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

func TestFooterSeparatesHelpAndShowsVersion(t *testing.T) {
	model := newTestModel()
	model.width = 100
	footer := ansi.Strip(model.renderFooter())

	for _, want := range []string{
		"↑/↓ process · pgup/pgdn scroll · f follow · q quit",
		"v1.2.3 · following · 100% ",
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer does not contain %q: %q", want, footer)
		}
	}
	if width := lipgloss.Width(footer); width != model.width {
		t.Errorf("footer width = %d, want %d", width, model.width)
	}
}

func TestFooterTruncatesHelpWithoutOverflow(t *testing.T) {
	model := newTestModel()
	model.width = 60
	footer := ansi.Strip(model.renderFooter())

	if !strings.Contains(footer, "…") {
		t.Fatalf("footer does not truncate help: %q", footer)
	}
	if !strings.HasSuffix(footer, "v1.2.3 · following · 100% ") {
		t.Fatalf("footer does not preserve right-side status: %q", footer)
	}
	if width := lipgloss.Width(footer); width != model.width {
		t.Errorf("footer width = %d, want %d", width, model.width)
	}
}

func TestViewLeavesOneRowBetweenHeaderAndBody(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	lines := strings.Split(ansi.Strip(updated.(Model).View()), "\n")

	if got := strings.TrimSpace(lines[1]); got != "" {
		t.Fatalf("line below header = %q, want blank", got)
	}
	if got := strings.Count(lines[2], "╭"); got != 2 {
		t.Fatalf("body does not begin with two aligned panel borders: %q", lines[2])
	}
	if !strings.Contains(lines[3], "web") || !strings.Contains(lines[3], "$ bin/rails server") {
		t.Fatalf("first process and log panel content are not aligned: %q", lines[3])
	}
}

func TestHeaderShowsWorkingDirectoryAtRight(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	header := strings.Split(ansi.Strip(updated.(Model).View()), "\n")[0]

	if !strings.HasSuffix(header, "/Users/example/project ") {
		t.Fatalf("header does not show the working directory at the right: %q", header)
	}
	if width := lipgloss.Width(header); width != 100 {
		t.Fatalf("header width = %d, want 100", width)
	}
}

func TestHeaderTruncatesLongWorkingDirectoryWithoutOverflow(t *testing.T) {
	model := newTestModel()
	model.workingDirectory = "/Users/example/a/very/long/path/to/project"
	model.width = 48
	header := strings.Split(ansi.Strip(model.renderHeader()), "\n")[0]

	if !strings.Contains(header, "…") || !strings.HasSuffix(header, "/project ") {
		t.Fatalf("header does not preserve the end of a long working directory: %q", header)
	}
	if width := lipgloss.Width(header); width != 48 {
		t.Fatalf("header width = %d, want 48", width)
	}
}

func TestMouseSelectionAccountsForHeaderGap(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)

	updated, _ = model.Update(tea.MouseMsg{
		X:      1,
		Y:      headerHeight + panelTopBorderHeight + 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	if selected := updated.(Model).selected; selected != 1 {
		t.Fatalf("selected = %d, want 1", selected)
	}
}

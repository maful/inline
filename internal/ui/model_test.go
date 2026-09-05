package ui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/maful/inline/internal/process"
	"github.com/maful/inline/internal/procfile"
)

type fakeSource struct {
	events   chan process.Event
	restarts []int
}

func (f *fakeSource) StartAll()                    {}
func (f *fakeSource) Restart(index int)            { f.restarts = append(f.restarts, index) }
func (f *fakeSource) Events() <-chan process.Event { return f.events }

func newTestModel() Model {
	definitions := []procfile.Process{
		{Name: "web", Command: "bin/rails server"},
		{Name: "worker", Command: "bundle exec sidekiq"},
	}
	return New(definitions, &fakeSource{events: make(chan process.Event)}, "Procfile", "/Users/example/project", "main", "v1.2.3")
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

func TestRestartKeyRestartsSelectedProcess(t *testing.T) {
	source := &fakeSource{events: make(chan process.Event)}
	model := New([]procfile.Process{
		{Name: "web", Command: "bin/rails server"},
		{Name: "worker", Command: "bundle exec sidekiq"},
	}, source, "Procfile", "/Users/example/project", "main", "v1.2.3")
	model.selected = 1

	updated, command := model.Update(keyRunes("r"))
	if command == nil {
		t.Fatal("restart key returned no command")
	}
	command()
	model = updated.(Model)

	if got, want := source.restarts, []int{1}; !slices.Equal(got, want) {
		t.Fatalf("restarts = %v, want %v", got, want)
	}
	if model.selected != 1 {
		t.Fatalf("selected = %d, want 1", model.selected)
	}
}

func TestStaleProcessEventDoesNotReplaceRestartedState(t *testing.T) {
	model := newTestModel()
	model.applyEvent(process.Event{Index: 0, Generation: 2, State: process.Running, PID: 2222})
	model.applyEvent(process.Event{Index: 0, Generation: 1, State: process.Failed, Err: errors.New("old process exited")})

	item := model.processes[0]
	if item.state != process.Running || item.pid != 2222 {
		t.Fatalf("state = %s, pid = %d; want running with pid 2222", item.state, item.pid)
	}
	if item.logs.count() != 0 {
		t.Fatalf("stale event appended %d log lines, want 0", item.logs.count())
	}
}

func TestEventAppearsOnlyInItsProcess(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 1, Line: "sidekiq ready", State: process.Running, PID: 4242})
	model = updated.(Model)

	if model.processes[0].logs.count() != 0 {
		t.Fatalf("web lines = %v, want none", model.processes[0].logs.visibleLines())
	}
	if got := strings.Join(model.processes[1].logs.visibleLines(), "\n"); got != "sidekiq ready" {
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

func TestStartingSidebarUsesAnimatedPoints(t *testing.T) {
	model := newTestModel()
	model.width = 100
	model.height = 30
	model.ready = true

	firstFrame := ansi.Strip(model.renderSidebar())
	if !strings.Contains(firstFrame, "∙∙∙") {
		t.Fatalf("starting sidebar does not show the first Points frame:\n%s", firstFrame)
	}

	updated, command := model.Update(model.startupSpinner.Tick())
	model = updated.(Model)
	if command == nil {
		t.Fatal("spinner tick did not schedule the next frame")
	}
	secondFrame := ansi.Strip(model.renderSidebar())
	if !strings.Contains(secondFrame, "●∙∙") {
		t.Fatalf("starting sidebar does not show the second Points frame:\n%s", secondFrame)
	}
}

func TestRunningSidebarUsesStaticSuccessMarker(t *testing.T) {
	model := newTestModel()
	model.width = 100
	model.height = 30
	model.ready = true
	for index := range model.processes {
		model.processes[index].state = process.Running
	}

	sidebar := ansi.Strip(model.renderSidebar())
	if strings.Contains(sidebar, "∙") {
		t.Fatalf("running sidebar still shows the Points spinner:\n%s", sidebar)
	}
	if !strings.Contains(sidebar, "●") {
		t.Fatalf("running sidebar does not show the static success marker:\n%s", sidebar)
	}
}

func TestSidebarMarkersKeepProcessNamesAligned(t *testing.T) {
	model := newTestModel()
	model.width = 100
	model.height = 30
	model.ready = true
	model.processes[0].state = process.Starting
	model.processes[1].state = process.Failed

	rows := strings.Split(ansi.Strip(model.renderSidebar()), "\n")
	var webColumn, workerColumn int
	for _, row := range rows {
		if index := strings.Index(row, "web"); index >= 0 {
			webColumn = lipgloss.Width(row[:index])
		}
		if index := strings.Index(row, "worker"); index >= 0 {
			workerColumn = lipgloss.Width(row[:index])
		}
	}
	if webColumn == 0 || workerColumn == 0 {
		t.Fatalf("sidebar does not contain both process rows:\n%s", strings.Join(rows, "\n"))
	}
	if webColumn != workerColumn {
		t.Fatalf("process names start at columns %d and %d, want equal columns", webColumn, workerColumn)
	}
}

func TestSpinnerStopsWhenNoProcessIsStarting(t *testing.T) {
	model := newTestModel()
	for index := range model.processes {
		model.processes[index].state = process.Running
	}
	firstFrame := model.startupSpinner.View()

	updated, command := model.Update(model.startupSpinner.Tick())
	model = updated.(Model)
	if command != nil {
		t.Fatal("spinner tick scheduled another frame without a starting process")
	}
	if secondFrame := model.startupSpinner.View(); secondFrame != firstFrame {
		t.Fatalf("spinner advanced from %q to %q without a starting process", firstFrame, secondFrame)
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

func TestFiltersAreLiveAndIndependentPerProcess(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	for _, event := range []process.Event{
		{Index: 0, Line: "web ready"},
		{Index: 0, Line: "web ERROR timeout"},
		{Index: 1, Line: "worker ready"},
		{Index: 1, Line: "worker retry"},
	} {
		updated, _ = model.Update(event)
		model = updated.(Model)
	}

	updated, _ = model.Update(keyRunes("/"))
	model = updated.(Model)
	updated, _ = model.Update(keyRunes("error"))
	model = updated.(Model)
	if got, want := model.processes[0].logs.visibleLines(), []string{"web ERROR timeout"}; !slices.Equal(got, want) {
		t.Fatalf("web visible lines = %v, want %v", got, want)
	}
	plainView := ansi.Strip(model.View())
	if !strings.Contains(plainView, "web ERROR timeout") || strings.Contains(plainView, "web ready") {
		t.Fatalf("web filter was not previewed live:\n%s", model.View())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, _ = model.Update(keyRunes("/"))
	model = updated.(Model)
	updated, _ = model.Update(keyRunes("ready"))
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)

	if got := model.processes[0].logs.query; got != "error" {
		t.Fatalf("web query = %q, want error", got)
	}
	if got := model.processes[1].logs.query; got != "ready" {
		t.Fatalf("worker query = %q, want ready", got)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if got := model.processes[0].logs.visibleCount(); got != 2 {
		t.Fatalf("web visible count after clearing = %d, want 2", got)
	}
	if got := model.processes[1].logs.query; got != "ready" {
		t.Fatalf("clearing web filter changed worker query to %q", got)
	}
}

func TestEscapeCancelsFilterEdit(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	model.processes[0].logs.setQuery("error")

	updated, _ = model.Update(keyRunes("/"))
	model = updated.(Model)
	updated, _ = model.Update(keyRunes(" timeout"))
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)

	if got := model.processes[0].logs.query; got != "error" {
		t.Fatalf("query after cancel = %q, want error", got)
	}
	if model.filterEditing {
		t.Fatal("filter remains in editing mode after escape")
	}
}

func TestFilteredViewExplainsNoMatches(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 0, State: process.Running, Line: "server ready"})
	model = updated.(Model)
	model.processes[0].logs.setQuery("error")
	model.processes[0].dirty = true
	model.refreshSelected()

	view := model.View()
	if !strings.Contains(view, `0/1 lines · filter: "error"`) {
		t.Fatalf("view does not show match count and filter:\n%s", view)
	}
	if !strings.Contains(view, `No lines match "error". 1 line retained.`) {
		t.Fatalf("view does not explain empty filtered output:\n%s", view)
	}
}

func TestFilterHighlightsEveryOccurrenceAndPreservesANSI(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	line := "\x1b[31mERROR\x1b[32m then error\x1b[0m"
	updated, _ = model.Update(process.Event{Index: 0, Line: line})
	model = updated.(Model)

	updated, _ = model.Update(keyRunes("/"))
	model = updated.(Model)
	updated, _ = model.Update(keyRunes("error"))
	model = updated.(Model)

	rendered := model.processes[0].rendered[0]
	if got, want := ansi.Strip(rendered), ansi.Strip(line); got != want {
		t.Fatalf("highlighted line stripped to %q, want %q", got, want)
	}
	if !strings.Contains(rendered, matchHighlightSequence(false)) {
		t.Fatalf("line does not contain the regular match highlight: %q", rendered)
	}
	if !strings.Contains(rendered, matchHighlightSequence(true)) {
		t.Fatalf("line does not contain the current match highlight: %q", rendered)
	}
	for _, color := range []string{"\x1b[31m", "\x1b[32m"} {
		if !strings.Contains(rendered, color) {
			t.Fatalf("highlighting removed original ANSI color %q: %q", color, rendered)
		}
	}
	if strings.Contains(rendered, activeMatchMarker) {
		t.Fatalf("rendered output contains the internal match marker: %q", rendered)
	}
}

func TestHighlightRestoresOriginalBackground(t *testing.T) {
	line := "\x1b[44merror tail\x1b[0m"
	rendered := highlightLogLine(line, []matchSpan{{sequence: 0, start: 0, end: 5}}, -1)

	if !strings.Contains(rendered, "\x1b[44m tail") {
		t.Fatalf("highlight did not restore the original background: %q", rendered)
	}
}

func TestMatchNavigationWrapsAndRevealsCurrentOccurrence(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model = updated.(Model)
	for index := range 12 {
		updated, _ = model.Update(process.Event{Index: 0, Line: fmt.Sprintf("error %02d", index)})
		model = updated.(Model)
	}

	updated, _ = model.Update(keyRunes("/"))
	model = updated.(Model)
	updated, _ = model.Update(keyRunes("error"))
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if got := model.processes[0].matchCursor; got != 11 {
		t.Fatalf("initial match cursor = %d, want 11", got)
	}
	if !strings.Contains(ansi.Strip(model.renderFooter()), "[12/12] matches") {
		t.Fatalf("footer does not show the initial match position: %q", ansi.Strip(model.renderFooter()))
	}

	updated, _ = model.Update(keyRunes("n"))
	model = updated.(Model)
	item := model.processes[0]
	if item.matchCursor != 0 || item.follow || item.viewport.YOffset != 0 {
		t.Fatalf("next did not wrap to the first match: cursor=%d follow=%v offset=%d", item.matchCursor, item.follow, item.viewport.YOffset)
	}

	updated, _ = model.Update(keyRunes("N"))
	model = updated.(Model)
	item = model.processes[0]
	if item.matchCursor != 11 {
		t.Fatalf("previous cursor = %d, want 11", item.matchCursor)
	}
	if item.activeMatchRow < item.viewport.YOffset || item.activeMatchRow >= item.viewport.YOffset+item.viewport.Height {
		t.Fatalf("current match row %d is outside viewport [%d,%d)", item.activeMatchRow, item.viewport.YOffset, item.viewport.YOffset+item.viewport.Height)
	}
}

func TestMatchNavigationMovesBetweenOccurrencesOnOneLine(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 0, Line: "error then error"})
	model = updated.(Model)
	model.processes[0].logs.setQuery("error")
	model.resetMatchCursor(&model.processes[0])
	model.processes[0].dirty = true
	model.refreshSelected()

	before := model.processes[0].rendered[0]
	if strings.Index(before, matchHighlightSequence(false)) >= strings.Index(before, matchHighlightSequence(true)) {
		t.Fatalf("last occurrence is not current before navigation: %q", before)
	}
	updated, _ = model.Update(keyRunes("N"))
	model = updated.(Model)
	after := model.processes[0].rendered[0]
	if strings.Index(after, matchHighlightSequence(true)) >= strings.Index(after, matchHighlightSequence(false)) {
		t.Fatalf("previous did not select the first occurrence: %q", after)
	}
}

func TestCurrentMatchRowTracksWrappedOutput(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	model = updated.(Model)
	updated, _ = model.Update(process.Event{Index: 0, Line: strings.Repeat("prefix ", 20) + "error"})
	model = updated.(Model)
	model.processes[0].logs.setQuery("error")
	model.resetMatchCursor(&model.processes[0])
	model.processes[0].dirty = true
	model.refreshSelected()

	item := model.processes[0]
	if item.activeMatchRow <= 0 {
		t.Fatalf("active match row = %d, want a wrapped row", item.activeMatchRow)
	}
	if item.activeMatchRow < item.viewport.YOffset || item.activeMatchRow >= item.viewport.YOffset+item.viewport.Height {
		t.Fatalf("wrapped match row %d is outside viewport [%d,%d)", item.activeMatchRow, item.viewport.YOffset, item.viewport.YOffset+item.viewport.Height)
	}
}

func TestPausedMatchSelectionSurvivesIncomingMatches(t *testing.T) {
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	for _, line := range []string{"first error", "second error"} {
		updated, _ = model.Update(process.Event{Index: 0, Line: line})
		model = updated.(Model)
	}
	model.processes[0].logs.setQuery("error")
	model.resetMatchCursor(&model.processes[0])
	model.processes[0].dirty = true
	model.refreshSelected()

	updated, _ = model.Update(keyRunes("n"))
	model = updated.(Model)
	selected, ok := selectedOccurrence(&model.processes[0])
	if !ok {
		t.Fatal("no match selected after navigation")
	}
	updated, _ = model.Update(process.Event{Index: 0, Line: "third error"})
	model = updated.(Model)
	if current, ok := selectedOccurrence(&model.processes[0]); !ok || current != selected {
		t.Fatalf("incoming match changed paused selection from %#v to %#v", selected, current)
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
		"↑/↓ select · r restart · / filter · pgup/dn scroll · f follow · q quit",
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
	t.Setenv("HOME", "/Users/example")
	model := newTestModel()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	header := strings.Split(ansi.Strip(updated.(Model).View()), "\n")[0]

	if !strings.HasSuffix(header, "~/project · ⎇ main ") {
		t.Fatalf("header does not show the working directory and branch at the right: %q", header)
	}
	if strings.Contains(header, "/Users/example") {
		t.Fatalf("header exposes the home directory: %q", header)
	}
	if width := lipgloss.Width(header); width != 100 {
		t.Fatalf("header width = %d, want 100", width)
	}
}

func TestAbbreviateHomeDirectory(t *testing.T) {
	tests := []struct {
		name      string
		directory string
		home      string
		want      string
	}{
		{name: "home directory", directory: "/Users/example", home: "/Users/example", want: "~"},
		{name: "inside home directory", directory: "/Users/example/project", home: "/Users/example", want: "~/project"},
		{name: "similar prefix", directory: "/Users/example-work/project", home: "/Users/example", want: "/Users/example-work/project"},
		{name: "outside home directory", directory: "/opt/project", home: "/Users/example", want: "/opt/project"},
		{name: "unknown home directory", directory: "/Users/example/project", home: "", want: "/Users/example/project"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := abbreviateHomeDirectory(test.directory, test.home); got != test.want {
				t.Fatalf("abbreviateHomeDirectory(%q, %q) = %q, want %q", test.directory, test.home, got, test.want)
			}
		})
	}
}

func TestHeaderOmitsBranchOutsideGitRepository(t *testing.T) {
	model := newTestModel()
	model.branch = ""
	model.width = 100
	header := strings.Split(ansi.Strip(model.renderHeader()), "\n")[0]

	if strings.Contains(header, "⎇") {
		t.Fatalf("header shows a branch marker without a branch: %q", header)
	}
	if !strings.HasSuffix(header, "/Users/example/project ") {
		t.Fatalf("header does not preserve the working directory: %q", header)
	}
}

func TestHeaderTruncatesLongBranchWithoutOverflow(t *testing.T) {
	model := newTestModel()
	model.branch = "feature/a-very-long-branch-name"
	model.width = 48
	header := strings.Split(ansi.Strip(model.renderHeader()), "\n")[0]

	if !strings.Contains(header, "⎇ feature/") || !strings.Contains(header, "…") {
		t.Fatalf("header does not preserve the branch marker and branch prefix: %q", header)
	}
	if width := lipgloss.Width(header); width != 48 {
		t.Fatalf("header width = %d, want 48", width)
	}
}

func TestHeaderTruncatesLongWorkingDirectoryWithoutOverflow(t *testing.T) {
	model := newTestModel()
	model.workingDirectory = "/Users/example/a/very/long/path/to/project"
	model.width = 48
	header := strings.Split(ansi.Strip(model.renderHeader()), "\n")[0]

	if !strings.Contains(header, "…") || !strings.HasSuffix(header, "/project · ⎇ main ") {
		t.Fatalf("header does not preserve the branch and end of a long working directory: %q", header)
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

func keyRunes(value string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
}

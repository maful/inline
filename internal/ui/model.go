package ui

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/maful/inline/internal/process"
	"github.com/maful/inline/internal/procfile"
)

const (
	maxLogLines           = 20_000
	headerHeight          = 2
	footerHeight          = 1
	panelTopBorderHeight  = 1
	panelBorderHeight     = 2
	panelHeaderHeight     = 3
	panelVerticalChrome   = panelBorderHeight + panelHeaderHeight
	minimumTerminalHeight = headerHeight + footerHeight + panelVerticalChrome + 1
	maxEventBatch         = 256
)

var (
	colorText    = lipgloss.AdaptiveColor{Light: "#20242c", Dark: "#d8dee9"}
	colorMuted   = lipgloss.AdaptiveColor{Light: "#68707c", Dark: "#7f8794"}
	colorPrimary = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#7D56F4"}
	colorBorder  = lipgloss.AdaptiveColor{Light: "#7D79E8", Dark: "#6C63C7"}
	colorSuccess = lipgloss.AdaptiveColor{Light: "#2f855a", Dark: "#68d391"}
	colorError   = lipgloss.AdaptiveColor{Light: "#c53030", Dark: "#fc8181"}
	colorWarning = lipgloss.AdaptiveColor{Light: "#b7791f", Dark: "#f6c768"}
	colorMatch   = lipgloss.AdaptiveColor{Light: "#e8dc8e", Dark: "#5f5b00"}
	colorCurrent = lipgloss.AdaptiveColor{Light: "#f0a43a", Dark: "#b7791f"}

	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary)
	mutedStyle   = lipgloss.NewStyle().Foreground(colorMuted)
	commandStyle = lipgloss.NewStyle().Foreground(colorMuted)
	branchStyle  = lipgloss.NewStyle().Foreground(colorBorder)
	panelStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder)
	activeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(colorPrimary).Bold(true)
	errorStyle   = lipgloss.NewStyle().Foreground(colorError)
	matchStyle   = lipgloss.NewStyle().Background(colorMatch)
	currentStyle = lipgloss.NewStyle().Background(colorCurrent)
)

const activeMatchMarker = "\x1b]1337;inline-active-match\x07"

type processSource interface {
	StartAll()
	Restart(index int)
	Events() <-chan process.Event
}

type processView struct {
	definition     procfile.Process
	viewport       viewport.Model
	logs           logBuffer
	rendered       []string
	state          process.State
	pid            int
	generation     uint64
	follow         bool
	dirty          bool
	matchCursor    int
	activeMatchRow int
}

// Model is Inline's Bubble Tea application state.
type Model struct {
	processes        []processView
	source           processSource
	path             string
	workingDirectory string
	branch           string
	version          string
	selected         int
	width            int
	height           int
	ready            bool
	filterInput      textinput.Model
	filterEditing    bool
	filterProcess    int
	filterOriginal   string
}

func New(definitions []procfile.Process, source processSource, path, workingDirectory, branch, version string) Model {
	homeDirectory, _ := os.UserHomeDir()
	filterInput := textinput.New()
	filterInput.Prompt = "/ "
	filterInput.Placeholder = "filter logs"
	filterInput.CharLimit = 256
	views := make([]processView, len(definitions))
	for index, definition := range definitions {
		view := viewport.New(0, 0)
		view.MouseWheelDelta = 3
		views[index] = processView{
			definition:     definition,
			viewport:       view,
			logs:           newLogBuffer(maxLogLines),
			state:          process.Starting,
			follow:         true,
			dirty:          true,
			matchCursor:    -1,
			activeMatchRow: -1,
		}
	}
	return Model{
		processes:        views,
		source:           source,
		path:             path,
		workingDirectory: abbreviateHomeDirectory(workingDirectory, homeDirectory),
		branch:           branch,
		version:          version,
		filterInput:      filterInput,
		filterProcess:    -1,
	}
}

func abbreviateHomeDirectory(directory, homeDirectory string) string {
	if homeDirectory == "" {
		return directory
	}

	relative, err := filepath.Rel(homeDirectory, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return directory
	}
	if relative == "." {
		return "~"
	}
	return filepath.Join("~", relative)
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			m.source.StartAll()
			return nil
		},
		waitForEvent(m.source.Events()),
	)
}

type processEventBatch []process.Event

func waitForEvent(events <-chan process.Event) tea.Cmd {
	return func() tea.Msg {
		first, ok := <-events
		if !ok {
			return nil
		}
		batch := processEventBatch{first}
		for len(batch) < maxEventBatch {
			select {
			case event, open := <-events:
				if !open {
					return batch
				}
				batch = append(batch, event)
			default:
				return batch
			}
		}
		return batch
	}
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		m.ready = true
		m.resize()
		return m, nil

	case process.Event:
		m.applyEvent(message)
		m.refreshSelected()
		return m, waitForEvent(m.source.Events())

	case processEventBatch:
		for _, event := range message {
			m.applyEvent(event)
		}
		m.refreshSelected()
		return m, waitForEvent(m.source.Events())

	case tea.KeyMsg:
		if message.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.filterEditing {
			return m.updateFilterInput(message)
		}

		switch message.String() {
		case "q":
			return m, tea.Quit
		case "r":
			index := m.selected
			return m, func() tea.Msg {
				m.source.Restart(index)
				return nil
			}
		case "/":
			return m, m.beginFilter()
		case "esc":
			item := &m.processes[m.selected]
			if item.logs.setQuery("") {
				item.matchCursor = -1
				item.dirty = true
				m.refreshSelected()
			}
			return m, nil
		case "n":
			m.navigateMatch(1)
			return m, nil
		case "N":
			m.navigateMatch(-1)
			return m, nil
		case "tab", "right", "l":
			m.selectRelative(1)
			m.refreshSelected()
			return m, nil
		case "shift+tab", "left", "h":
			m.selectRelative(-1)
			m.refreshSelected()
			return m, nil
		case "down", "j":
			m.selectRelative(1)
			m.refreshSelected()
			return m, nil
		case "up", "k":
			m.selectRelative(-1)
			m.refreshSelected()
			return m, nil
		case "end", "G":
			item := &m.processes[m.selected]
			item.follow = true
			item.viewport.GotoBottom()
			return m, nil
		case "home", "g":
			item := &m.processes[m.selected]
			item.follow = false
			item.viewport.GotoTop()
			return m, nil
		case "f":
			item := &m.processes[m.selected]
			item.follow = !item.follow
			if item.follow {
				item.viewport.GotoBottom()
			}
			return m, nil
		}

		if index := numberKey(message.String()); index >= 0 && index < len(m.processes) {
			m.selected = index
			m.refreshSelected()
			return m, nil
		}

		if isScrollKey(message.String()) {
			item := &m.processes[m.selected]
			item.follow = false
			updated, command := item.viewport.Update(message)
			item.viewport = updated
			if item.viewport.AtBottom() {
				item.follow = true
			}
			return m, command
		}

	case tea.MouseMsg:
		if m.filterEditing {
			return m, nil
		}
		if message.Action == tea.MouseActionPress && message.Button == tea.MouseButtonLeft {
			bodyY := message.Y - headerHeight - panelTopBorderHeight
			if message.X > 0 && message.X < m.sidebarWidth()-1 && bodyY >= 0 && bodyY < len(m.processes) {
				m.selected = bodyY
				m.refreshSelected()
				return m, nil
			}
		}
		if message.Button == tea.MouseButtonWheelUp || message.Button == tea.MouseButtonWheelDown {
			item := &m.processes[m.selected]
			if message.Button == tea.MouseButtonWheelUp {
				item.follow = false
			}
			updated, command := item.viewport.Update(message)
			item.viewport = updated
			if item.viewport.AtBottom() {
				item.follow = true
			}
			return m, command
		}
	}

	return m, nil
}

func (m Model) View() string {
	if !m.ready || m.width == 0 || m.height == 0 {
		return "Starting inline…"
	}
	if m.width < 48 || m.height < minimumTerminalHeight {
		return lipgloss.NewStyle().Width(m.width).Height(m.height).Align(lipgloss.Center, lipgloss.Center).
			Render(fmt.Sprintf("Terminal is too small\nResize to at least 48x%d", minimumTerminalHeight))
	}

	header := m.renderHeader()
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), " ", m.renderPanel())
	footer := m.renderFooter()
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *Model) applyEvent(event process.Event) {
	if event.Index < 0 || event.Index >= len(m.processes) {
		return
	}
	item := &m.processes[event.Index]
	if event.Generation > 0 {
		if event.Generation < item.generation {
			return
		}
		item.generation = event.Generation
	}
	if event.State != "" {
		item.state = event.State
		switch event.State {
		case process.Starting, process.Exited, process.Failed:
			item.pid = 0
		}
	}
	if event.PID > 0 {
		item.pid = event.PID
	}
	if event.Line != "" {
		m.appendLine(item, event.Line)
	}
	if event.Err != nil {
		m.appendLine(item, errorStyle.Render("inline: "+event.Err.Error()))
	}
}

func (m *Model) appendLine(item *processView, line string) {
	current, hadCurrent := selectedOccurrence(item)
	if item.logs.append(line) {
		m.reconcileMatchCursor(item, current, hadCurrent)
		item.dirty = true
	}
}

func (m *Model) beginFilter() tea.Cmd {
	item := &m.processes[m.selected]
	m.filterEditing = true
	m.filterProcess = m.selected
	m.filterOriginal = item.logs.query
	m.filterInput.SetValue(item.logs.query)
	m.filterInput.CursorEnd()
	return m.filterInput.Focus()
}

func (m *Model) updateFilterInput(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filterProcess < 0 || m.filterProcess >= len(m.processes) {
		m.filterEditing = false
		m.filterProcess = -1
		m.filterInput.Blur()
		return *m, nil
	}

	item := &m.processes[m.filterProcess]
	switch message.String() {
	case "enter":
		m.filterEditing = false
		m.filterProcess = -1
		m.filterInput.Blur()
		return *m, nil
	case "esc":
		if item.logs.setQuery(m.filterOriginal) {
			m.resetMatchCursor(item)
			item.dirty = true
			m.refreshSelected()
			m.revealActiveMatch(item)
		}
		m.filterEditing = false
		m.filterProcess = -1
		m.filterInput.Blur()
		return *m, nil
	}

	input, command := m.filterInput.Update(message)
	m.filterInput = input
	if item.logs.setQuery(m.filterInput.Value()) {
		m.resetMatchCursor(item)
		item.dirty = true
		m.refreshSelected()
		m.revealActiveMatch(item)
	}
	return *m, command
}

func (m *Model) refreshSelected() {
	if !m.ready || len(m.processes) == 0 {
		return
	}
	m.refreshViewport(&m.processes[m.selected])
}

func (m *Model) refreshViewport(item *processView) {
	if !item.dirty {
		return
	}
	entries := item.logs.visibleEntries()
	item.rendered = make([]string, len(entries))
	item.activeMatchRow = -1
	occurrences := item.logs.activeOccurrences()
	occurrenceIndex := 0
	visualRow := 0
	for index, entry := range entries {
		start := occurrenceIndex
		for occurrenceIndex < len(occurrences) && occurrences[occurrenceIndex].sequence == entry.sequence {
			occurrenceIndex++
		}
		spans := occurrences[start:occurrenceIndex]
		active := -1
		if item.matchCursor >= start && item.matchCursor < occurrenceIndex {
			active = item.matchCursor - start
		}
		line := highlightLogLine(entry.raw, spans, active)
		wrapped := wrapLogLine(line, item.viewport.Width)
		if marker := strings.Index(wrapped, activeMatchMarker); marker >= 0 {
			item.activeMatchRow = visualRow + strings.Count(wrapped[:marker], "\n")
			wrapped = strings.Replace(wrapped, activeMatchMarker, "", 1)
		}
		item.rendered[index] = wrapped
		visualRow += lipgloss.Height(wrapped)
	}
	item.viewport.SetContent(strings.Join(item.rendered, "\n"))
	item.dirty = false
	if item.follow {
		item.viewport.GotoBottom()
	}
}

func selectedOccurrence(item *processView) (matchSpan, bool) {
	occurrences := item.logs.activeOccurrences()
	if item.matchCursor < 0 || item.matchCursor >= len(occurrences) {
		return matchSpan{}, false
	}
	return occurrences[item.matchCursor], true
}

func (m *Model) resetMatchCursor(item *processView) {
	count := item.logs.occurrenceCount()
	if count == 0 {
		item.matchCursor = -1
		return
	}
	if item.follow {
		item.matchCursor = count - 1
		return
	}
	item.matchCursor = 0
}

func (m *Model) reconcileMatchCursor(item *processView, current matchSpan, hadCurrent bool) {
	count := item.logs.occurrenceCount()
	if count == 0 {
		item.matchCursor = -1
		return
	}
	if item.follow {
		item.matchCursor = count - 1
		return
	}
	if hadCurrent {
		if index := item.logs.occurrenceIndex(current); index >= 0 {
			item.matchCursor = index
			return
		}
	}
	item.matchCursor = min(max(item.matchCursor, 0), count-1)
}

func (m *Model) navigateMatch(delta int) {
	if len(m.processes) == 0 {
		return
	}
	item := &m.processes[m.selected]
	count := item.logs.occurrenceCount()
	if item.logs.normalizedQuery == "" || count == 0 {
		return
	}
	if item.matchCursor < 0 || item.matchCursor >= count {
		item.matchCursor = 0
	} else {
		item.matchCursor = (item.matchCursor + delta + count) % count
	}
	item.follow = false
	item.dirty = true
	m.refreshSelected()
	m.revealActiveMatch(item)
}

func (m *Model) revealActiveMatch(item *processView) {
	row := item.activeMatchRow
	if row < 0 {
		return
	}
	if row < item.viewport.YOffset {
		item.viewport.SetYOffset(row)
	} else if row >= item.viewport.YOffset+item.viewport.Height {
		item.viewport.SetYOffset(row - item.viewport.Height + 1)
	}
}

func (m *Model) selectRelative(delta int) {
	count := len(m.processes)
	if count == 0 {
		return
	}
	m.selected = (m.selected + delta + count) % count
}

func (m *Model) resize() {
	if len(m.processes) == 0 {
		return
	}
	bodyHeight := m.bodyHeight()
	mainWidth := max(1, m.width-m.sidebarWidth()-1)
	viewportWidth := max(1, mainWidth-4)
	viewportHeight := max(1, bodyHeight-panelVerticalChrome)
	m.filterInput.Width = max(1, viewportWidth-2)
	for index := range m.processes {
		item := &m.processes[index]
		widthChanged := item.viewport.Width != viewportWidth
		item.viewport.Width = viewportWidth
		item.viewport.Height = viewportHeight
		if widthChanged {
			item.dirty = true
		}
	}
	m.refreshSelected()
	if m.processes[m.selected].follow {
		m.processes[m.selected].viewport.GotoBottom()
	} else {
		m.revealActiveMatch(&m.processes[m.selected])
	}
}

func wrapLogLine(line string, width int) string {
	if width < 1 {
		return line
	}
	return ansi.Wrap(line, width, "")
}

func highlightLogLine(line string, spans []matchSpan, active int) string {
	if len(spans) == 0 {
		return line
	}

	var output strings.Builder
	state := byte(0)
	byteOffset := 0
	runeOffset := 0
	spanIndex := 0
	highlighted := -1
	background := "\x1b[49m"

	closeHighlight := func() {
		if highlighted >= 0 {
			output.WriteString(background)
			highlighted = -1
		}
	}
	openHighlight := func(index int) {
		highlighted = index
		output.WriteString(matchHighlightSequence(index == active))
		if index == active {
			output.WriteString(activeMatchMarker)
		}
	}

	for byteOffset < len(line) {
		sequence, _, consumed, nextState := ansi.GraphemeWidth.DecodeSequenceInString(line[byteOffset:], state, nil)
		if consumed <= 0 {
			break
		}
		byteOffset += consumed
		state = nextState

		plain := ansi.Strip(sequence)
		if plain == "" {
			output.WriteString(sequence)
			if restored, changed := backgroundAfterSGR(sequence, background); changed {
				background = restored
				if highlighted >= 0 {
					output.WriteString(matchHighlightSequence(highlighted == active))
				}
			}
			continue
		}

		plainRunes := utf8.RuneCountInString(plain)
		for spanIndex < len(spans) && runeOffset >= spans[spanIndex].end {
			spanIndex++
		}
		desired := -1
		if spanIndex < len(spans) && runeOffset < spans[spanIndex].end && runeOffset+plainRunes > spans[spanIndex].start {
			desired = spanIndex
		}
		if desired != highlighted {
			closeHighlight()
			if desired >= 0 {
				openHighlight(desired)
			}
		}
		output.WriteString(sequence)
		runeOffset += plainRunes
		if highlighted >= 0 && runeOffset >= spans[highlighted].end {
			closeHighlight()
			spanIndex++
		}
	}
	closeHighlight()
	if byteOffset < len(line) {
		output.WriteString(line[byteOffset:])
	}
	return output.String()
}

func matchHighlightSequence(active bool) string {
	style := matchStyle
	if active {
		style = currentStyle
	}
	const marker = "\ue000"
	rendered := style.Render(marker)
	if index := strings.Index(rendered, marker); index > 0 {
		return rendered[:index]
	}
	if active {
		return "\x1b[48;5;178m"
	}
	return "\x1b[48;5;58m"
}

func backgroundAfterSGR(sequence, current string) (string, bool) {
	if !strings.HasPrefix(sequence, "\x1b[") || !strings.HasSuffix(sequence, "m") {
		return current, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(sequence, "\x1b["), "m")
	if body == "" {
		return "\x1b[49m", true
	}

	parameters := strings.Split(body, ";")
	restored := current
	changed := false
	for index := 0; index < len(parameters); index++ {
		parameter := parameters[index]
		if strings.HasPrefix(parameter, "48:") {
			restored = "\x1b[" + parameter + "m"
			changed = true
			continue
		}
		value, err := strconv.Atoi(parameter)
		if err != nil {
			continue
		}
		switch {
		case value == 0 || value == 49:
			restored = "\x1b[49m"
			changed = true
		case value >= 40 && value <= 47, value >= 100 && value <= 107:
			restored = "\x1b[" + parameter + "m"
			changed = true
		case value == 48 && index+2 < len(parameters) && parameters[index+1] == "5":
			restored = "\x1b[" + strings.Join(parameters[index:index+3], ";") + "m"
			changed = true
			index += 2
		case value == 48 && index+4 < len(parameters) && parameters[index+1] == "2":
			restored = "\x1b[" + strings.Join(parameters[index:index+5], ";") + "m"
			changed = true
			index += 4
		}
	}
	return restored, changed
}

func (m Model) renderHeader() string {
	const (
		horizontalPadding = 2
		minimumGap        = 1
	)
	contentWidth := max(1, m.width-horizontalPadding)
	leftMaxWidth := max(len("inline"), contentWidth/2)
	file := truncate(" · "+filepath.Base(m.path), leftMaxWidth-len("inline"))
	left := titleStyle.Render("inline") + mutedStyle.Render(file)

	rightWidth := max(1, contentWidth-lipgloss.Width(left)-minimumGap)
	right := m.renderHeaderRight(rightWidth)
	gap := max(minimumGap, m.width-horizontalPadding-lipgloss.Width(left)-lipgloss.Width(right))
	return lipgloss.NewStyle().Height(headerHeight).Render(
		" " + left + strings.Repeat(" ", gap) + right + " ",
	)
}

func (m Model) renderHeaderRight(width int) string {
	if m.branch == "" {
		return mutedStyle.Render(truncateLeft(m.workingDirectory, width))
	}

	const (
		separator = " · "
		marker    = "⎇ "
	)
	branchWidth := width - lipgloss.Width(marker)
	if branchWidth <= 0 {
		return branchStyle.Render(truncate(marker, width))
	}
	branch := marker + truncate(m.branch, branchWidth)
	if lipgloss.Width(branch)+lipgloss.Width(separator) >= width {
		return branchStyle.Render(branch)
	}

	directoryWidth := width - lipgloss.Width(separator) - lipgloss.Width(branch)
	directory := truncateLeft(m.workingDirectory, directoryWidth)
	return mutedStyle.Render(directory+separator) + branchStyle.Render(branch)
}

func (m Model) renderSidebar() string {
	width := m.sidebarWidth()
	height := m.bodyHeight()
	innerWidth := max(1, width-panelBorderHeight)
	rows := make([]string, 0, len(m.processes))
	for index, item := range m.processes {
		marker, markerStyle := stateMarker(item.state)
		style := lipgloss.NewStyle().Width(innerWidth).MaxWidth(innerWidth)
		if index == m.selected {
			// Keep the selected label plain before applying its background. A nested
			// marker style emits an ANSI reset that would punch a hole in the row.
			label := fmt.Sprintf(" %d %s %s", index+1, marker, item.definition.Name)
			style = style.Inherit(activeStyle)
			rows = append(rows, style.Render(label))
		} else {
			label := fmt.Sprintf(" %d %s %s", index+1, markerStyle.Render(marker), item.definition.Name)
			style = style.Foreground(colorText)
			rows = append(rows, style.Render(label))
		}
	}
	return panelStyle.
		Width(innerWidth).
		Height(max(1, height-panelBorderHeight)).
		Render(strings.Join(rows, "\n"))
}

func (m Model) renderPanel() string {
	item := m.processes[m.selected]
	mainWidth := max(1, m.width-m.sidebarWidth()-1)
	innerWidth := max(1, mainWidth-4)
	command := commandStyle.Render(truncate("$ "+item.definition.Command, innerWidth))
	status := renderStatus(item, innerWidth)
	if m.filterEditing && m.filterProcess == m.selected {
		status = m.filterInput.View()
	}
	divider := mutedStyle.Render(strings.Repeat("─", innerWidth))
	content := lipgloss.JoinVertical(lipgloss.Left, command, status, divider, renderLogs(item))
	return panelStyle.Width(mainWidth - 2).Render(content)
}

func renderStatus(item processView, width int) string {
	marker, style := stateMarker(item.state)
	label := marker + " " + string(item.state)
	if item.pid > 0 {
		label += fmt.Sprintf(" · pid %d", item.pid)
	}
	if item.logs.normalizedQuery != "" {
		label += fmt.Sprintf(" · %d/%d lines · filter: %q", item.logs.visibleCount(), item.logs.count(), item.logs.query)
	} else {
		label += fmt.Sprintf(" · %d lines", item.logs.count())
	}
	if item.state == process.Running && item.logs.count() == 0 {
		label += " · waiting for output"
	}
	return style.Render(truncate(label, width))
}

func renderLogs(item processView) string {
	if item.logs.visibleCount() > 0 {
		return item.viewport.View()
	}
	if item.logs.count() > 0 {
		retained := fmt.Sprintf("%d lines", item.logs.count())
		if item.logs.count() == 1 {
			retained = "1 line"
		}
		return lipgloss.NewStyle().
			Foreground(colorMuted).
			Width(item.viewport.Width).
			Height(item.viewport.Height).
			Render(fmt.Sprintf("No lines match %q. %s retained.", item.logs.query, retained))
	}
	return lipgloss.NewStyle().
		Foreground(colorMuted).
		Width(item.viewport.Width).
		Height(item.viewport.Height).
		Render("No output yet. The process has not written to stdout or stderr.")
}

func (m Model) renderFooter() string {
	item := m.processes[m.selected]
	if m.filterEditing {
		return m.renderFooterParts(" type to filter · enter apply · esc cancel", "filtering ")
	}
	mode := "paused"
	if item.follow {
		mode = "following"
	}
	right := fmt.Sprintf("%s · %s · %3.0f%% ", m.version, mode, math.Round(item.viewport.ScrollPercent()*100))
	left := " ↑/↓ select · r restart · / filter · pgup/dn scroll · f follow · q quit"
	if item.logs.normalizedQuery != "" {
		current := 0
		if item.matchCursor >= 0 && item.matchCursor < item.logs.occurrenceCount() {
			current = item.matchCursor + 1
		}
		left = fmt.Sprintf(" [%d/%d] matches · n next · N prev · esc clear · / edit", current, item.logs.occurrenceCount())
	}
	return m.renderFooterParts(left, right)
}

func (m Model) renderFooterParts(left, right string) string {
	rightWidth := lipgloss.Width(right)
	if rightWidth >= m.width {
		return mutedStyle.Render(truncateLeft(right, m.width))
	}

	leftWidth := m.width - rightWidth - 1
	if leftWidth == 0 {
		left = ""
	} else {
		left = truncate(left, leftWidth)
	}
	space := m.width - lipgloss.Width(left) - rightWidth
	return mutedStyle.Render(left) + strings.Repeat(" ", space) + mutedStyle.Render(right)
}

func (m Model) sidebarWidth() int {
	if m.width < 72 {
		return 18
	}
	return min(26, max(18, m.width/4))
}

func (m Model) bodyHeight() int {
	return max(1, m.height-headerHeight-footerHeight)
}

func stateMarker(state process.State) (string, lipgloss.Style) {
	switch state {
	case process.Running:
		return "●", lipgloss.NewStyle().Foreground(colorSuccess)
	case process.Failed:
		return "●", lipgloss.NewStyle().Foreground(colorError)
	case process.Stopping:
		return "◐", lipgloss.NewStyle().Foreground(colorWarning)
	case process.Exited:
		return "○", mutedStyle
	default:
		return "◌", mutedStyle
	}
}

func numberKey(key string) int {
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		return int(key[0] - '1')
	}
	return -1
}

func isScrollKey(key string) bool {
	switch key {
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		return true
	default:
		return false
	}
}

func truncate(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func truncateLeft(value string, width int) string {
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	return "…" + string(runes[len(runes)-width+1:])
}

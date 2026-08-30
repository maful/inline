package ui

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"

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
)

var (
	colorText    = lipgloss.AdaptiveColor{Light: "#20242c", Dark: "#d8dee9"}
	colorMuted   = lipgloss.AdaptiveColor{Light: "#68707c", Dark: "#7f8794"}
	colorPrimary = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#7D56F4"}
	colorBorder  = lipgloss.AdaptiveColor{Light: "#7D79E8", Dark: "#6C63C7"}
	colorSuccess = lipgloss.AdaptiveColor{Light: "#2f855a", Dark: "#68d391"}
	colorError   = lipgloss.AdaptiveColor{Light: "#c53030", Dark: "#fc8181"}
	colorWarning = lipgloss.AdaptiveColor{Light: "#b7791f", Dark: "#f6c768"}

	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary)
	mutedStyle   = lipgloss.NewStyle().Foreground(colorMuted)
	commandStyle = lipgloss.NewStyle().Foreground(colorMuted)
	panelStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorBorder)
	activeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(colorPrimary).Bold(true)
	errorStyle   = lipgloss.NewStyle().Foreground(colorError)
)

type processSource interface {
	StartAll()
	Events() <-chan process.Event
}

type processView struct {
	definition procfile.Process
	viewport   viewport.Model
	lines      []string
	rendered   []string
	state      process.State
	pid        int
	follow     bool
}

// Model is Inline's Bubble Tea application state.
type Model struct {
	processes []processView
	source    processSource
	path      string
	selected  int
	width     int
	height    int
	ready     bool
}

func New(definitions []procfile.Process, source processSource, path string) Model {
	views := make([]processView, len(definitions))
	for index, definition := range definitions {
		view := viewport.New(0, 0)
		view.MouseWheelDelta = 3
		views[index] = processView{
			definition: definition,
			viewport:   view,
			state:      process.Starting,
			follow:     true,
		}
	}
	return Model{processes: views, source: source, path: path}
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

func waitForEvent(events <-chan process.Event) tea.Cmd {
	return func() tea.Msg { return <-events }
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
		if message.Index >= 0 && message.Index < len(m.processes) {
			item := &m.processes[message.Index]
			if message.State != "" {
				item.state = message.State
			}
			if message.PID > 0 {
				item.pid = message.PID
			}
			if message.Line != "" {
				m.appendLine(item, message.Line)
			}
			if message.Err != nil {
				m.appendLine(item, errorStyle.Render("inline: "+message.Err.Error()))
			}
		}
		return m, waitForEvent(m.source.Events())

	case tea.KeyMsg:
		switch message.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab", "right", "l":
			m.selectRelative(1)
			return m, nil
		case "shift+tab", "left", "h":
			m.selectRelative(-1)
			return m, nil
		case "down", "j":
			m.selectRelative(1)
			return m, nil
		case "up", "k":
			m.selectRelative(-1)
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
		if message.Action == tea.MouseActionPress && message.Button == tea.MouseButtonLeft {
			bodyY := message.Y - headerHeight - panelTopBorderHeight
			if message.X > 0 && message.X < m.sidebarWidth()-1 && bodyY >= 0 && bodyY < len(m.processes) {
				m.selected = bodyY
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

func (m *Model) appendLine(item *processView, line string) {
	item.lines = append(item.lines, line)
	item.rendered = append(item.rendered, wrapLogLine(line, item.viewport.Width))
	if len(item.lines) > maxLogLines {
		item.lines = append([]string(nil), item.lines[len(item.lines)-maxLogLines:]...)
		item.rendered = append([]string(nil), item.rendered[len(item.rendered)-maxLogLines:]...)
	}
	item.viewport.SetContent(strings.Join(item.rendered, "\n"))
	if item.follow {
		item.viewport.GotoBottom()
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
	for index := range m.processes {
		item := &m.processes[index]
		widthChanged := item.viewport.Width != viewportWidth
		item.viewport.Width = viewportWidth
		item.viewport.Height = viewportHeight
		if widthChanged {
			m.rewrap(item)
		}
		if item.follow {
			item.viewport.GotoBottom()
		}
	}
}

func (m *Model) rewrap(item *processView) {
	item.rendered = make([]string, len(item.lines))
	for index, line := range item.lines {
		item.rendered[index] = wrapLogLine(line, item.viewport.Width)
	}
	item.viewport.SetContent(strings.Join(item.rendered, "\n"))
}

func wrapLogLine(line string, width int) string {
	if width < 1 {
		return line
	}
	return ansi.Wrap(line, width, "")
}

func (m Model) renderHeader() string {
	name := titleStyle.Render("inline")
	file := mutedStyle.Render(" · " + filepath.Base(m.path))
	return lipgloss.NewStyle().Width(m.width).Height(headerHeight).Render(" " + name + file)
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
	label += fmt.Sprintf(" · %d lines", len(item.lines))
	if item.state == process.Running && len(item.lines) == 0 {
		label += " · waiting for output"
	}
	return style.Render(truncate(label, width))
}

func renderLogs(item processView) string {
	if len(item.lines) > 0 {
		return item.viewport.View()
	}
	return lipgloss.NewStyle().
		Foreground(colorMuted).
		Width(item.viewport.Width).
		Height(item.viewport.Height).
		Render("No output yet. The process has not written to stdout or stderr.")
}

func (m Model) renderFooter() string {
	item := m.processes[m.selected]
	mode := "paused"
	if item.follow {
		mode = "following"
	}
	left := mutedStyle.Render(" ↑/↓ process  pgup/pgdn scroll  f follow  q quit")
	right := mutedStyle.Render(fmt.Sprintf("%s · %3.0f%% ", mode, math.Round(item.viewport.ScrollPercent()*100)))
	space := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", space) + right
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

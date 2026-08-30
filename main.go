package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maful/inline/internal/process"
	"github.com/maful/inline/internal/procfile"
	"github.com/maful/inline/internal/ui"
)

func main() {
	procfilePath := flag.String("f", "Procfile", "path to a Procfile")
	flag.Parse()

	file, err := os.Open(*procfilePath)
	if err != nil {
		fatal("open %s: %v", *procfilePath, err)
	}
	defer file.Close()

	processes, err := procfile.Parse(file)
	if err != nil {
		fatal("parse %s: %v", *procfilePath, err)
	}

	supervisor := process.NewSupervisor(processes)
	model := ui.New(processes, supervisor, *procfilePath)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())

	if _, err := program.Run(); err != nil {
		supervisor.StopAll()
		fatal("run inline: %v", err)
	}
	supervisor.StopAll()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "inline: "+format+"\n", args...)
	os.Exit(1)
}

package procfile

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Process is one named command from a Procfile.
type Process struct {
	Name    string
	Command string
	Line    int
}

// Parse reads Procfile-compatible "name: command" entries.
func Parse(r io.Reader) ([]Process, error) {
	scanner := bufio.NewScanner(r)
	processes := make([]Process, 0)
	seen := make(map[string]int)

	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		name, command, ok := strings.Cut(text, ":")
		name = strings.TrimSpace(name)
		command = strings.TrimSpace(command)
		if !ok || name == "" || command == "" {
			return nil, fmt.Errorf("line %d: expected name: command", line)
		}
		if previous, exists := seen[name]; exists {
			return nil, fmt.Errorf("line %d: duplicate process %q (first defined on line %d)", line, name, previous)
		}

		seen[name] = line
		processes = append(processes, Process{Name: name, Command: command, Line: line})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Procfile: %w", err)
	}
	if len(processes) == 0 {
		return nil, fmt.Errorf("no processes found")
	}
	return processes, nil
}

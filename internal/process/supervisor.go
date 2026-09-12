package process

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/maful/inline/internal/procfile"
)

const (
	// MaxOutputLineBytes bounds each line before it enters the event queue.
	MaxOutputLineBytes = 64 * 1024
	eventBufferSize    = 256
	truncationBoundary = "\x1b\\"
	truncationMarker   = "... [output truncated]"
)

type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Exited   State = "exited"
	Failed   State = "failed"
	Stopping State = "stopping"
)

type Event struct {
	Index      int
	Generation uint64
	Line       string
	State      State
	Err        error
	PID        int
}

type runningProcess struct {
	lifecycle     sync.Mutex
	mu            sync.Mutex
	command       *exec.Cmd
	done          chan struct{}
	running       bool
	stopRequested bool
	generation    uint64
}

// Supervisor starts, observes, and stops all Procfile commands.
type Supervisor struct {
	definitions []procfile.Process
	processes   []runningProcess
	events      chan Event
	stopOnce    sync.Once
	mu          sync.Mutex
	stopping    bool
	shell       string
	interactive bool
}

func NewSupervisor(definitions []procfile.Process) *Supervisor {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return newSupervisor(definitions, shell, true)
}

func newSupervisor(definitions []procfile.Process, shell string, interactive bool) *Supervisor {
	return &Supervisor{
		definitions: definitions,
		processes:   make([]runningProcess, len(definitions)),
		events:      make(chan Event, eventBufferSize),
		shell:       shell,
		interactive: interactive,
	}
}

func (s *Supervisor) Events() <-chan Event { return s.events }

func (s *Supervisor) StartAll() {
	for index, definition := range s.definitions {
		s.start(index, definition, false)
	}
}

// Restart stops and starts one Procfile command while leaving the others alone.
func (s *Supervisor) Restart(index int) {
	if index < 0 || index >= len(s.processes) {
		return
	}

	item := &s.processes[index]
	item.lifecycle.Lock()
	defer item.lifecycle.Unlock()
	if s.isStopping() {
		return
	}

	s.stopAndWait(index)
	if s.isStopping() {
		return
	}
	s.startLocked(index, s.definitions[index], true)
}

func (s *Supervisor) start(index int, definition procfile.Process, restarted bool) {
	item := &s.processes[index]
	item.lifecycle.Lock()
	defer item.lifecycle.Unlock()
	if s.isStopping() {
		return
	}
	s.startLocked(index, definition, restarted)
}

func (s *Supervisor) startLocked(index int, definition procfile.Process, restarted bool) {
	command := s.buildCommand(definition.Command)
	// Interactive shells try to access their controlling terminal for job
	// control. Because each managed command runs in the background, that can
	// suspend the shell with SIGTTOU before the Procfile command starts. A new
	// session removes the controlling terminal and also makes the process PID
	// its process-group ID, preserving grouped shutdown via kill(-pid, signal).
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	item := &s.processes[index]
	item.mu.Lock()
	item.generation++
	generation := item.generation
	item.command = command
	item.done = make(chan struct{})
	item.running = false
	item.stopRequested = false
	done := item.done
	item.mu.Unlock()

	starting := Event{Index: index, Generation: generation, State: Starting}
	if restarted {
		starting.Line = "inline: restarting…"
	}
	s.events <- starting

	reader, writer, err := os.Pipe()
	if err != nil {
		item.mu.Lock()
		item.command = nil
		close(done)
		item.done = nil
		item.mu.Unlock()
		s.events <- Event{Index: index, Generation: generation, State: Failed, Err: fmt.Errorf("create output pipe: %w", err)}
		return
	}
	command.Stdout = writer
	command.Stderr = writer

	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		item.mu.Lock()
		item.command = nil
		close(done)
		item.done = nil
		item.mu.Unlock()
		s.events <- Event{Index: index, Generation: generation, State: Failed, Err: err}
		return
	}

	item.mu.Lock()
	item.running = true
	item.mu.Unlock()
	s.events <- Event{Index: index, Generation: generation, State: Running, PID: command.Process.Pid}
	go s.observe(index, generation, command, done, reader, writer)
}

func (s *Supervisor) buildCommand(script string) *exec.Cmd {
	flag := "-c"
	if s.interactive {
		flag = "-ic"
	}
	return exec.Command(s.shell, flag, script)
}

func (s *Supervisor) observe(index int, generation uint64, command *exec.Cmd, done chan struct{}, reader *os.File, writer *os.File) {
	type outputResult struct {
		dropped int
		err     error
	}
	linesDone := make(chan outputResult, 1)
	go func() {
		dropped := 0
		emit := func(line string) {
			if dropped > 0 {
				marker := fmt.Sprintf("inline: dropped %d log lines while the UI was busy", dropped)
				if !s.sendOutputLine(Event{Index: index, Generation: generation, Line: marker}) {
					dropped++
					return
				}
				dropped = 0
			}
			if !s.sendOutputLine(Event{Index: index, Generation: generation, Line: line}) {
				dropped++
			}
		}
		err := readOutputLines(reader, func(line string) {
			emit(line)
		})
		if errors.Is(err, os.ErrClosed) {
			err = nil
		}
		linesDone <- outputResult{dropped: dropped, err: err}
	}()

	err := command.Wait()
	writer.Close()
	output := <-linesDone
	reader.Close()

	item := &s.processes[index]
	item.mu.Lock()
	stopRequested := item.stopRequested
	if item.command == command && item.generation == generation {
		item.command = nil
		item.done = nil
		item.running = false
	}
	close(done)
	item.mu.Unlock()

	if stopRequested {
		return
	}
	line := ""
	if output.dropped > 0 {
		line = fmt.Sprintf("inline: dropped %d log lines while the UI was busy", output.dropped)
	}
	if output.err != nil {
		if line != "" {
			line += "; "
		}
		line += fmt.Sprintf("inline: read output: %v", output.err)
	}
	if err != nil {
		s.events <- Event{Index: index, Generation: generation, Line: line, State: Failed, Err: err}
		return
	}
	s.events <- Event{Index: index, Generation: generation, Line: line, State: Exited}
}

func (s *Supervisor) sendOutputLine(event Event) bool {
	select {
	case s.events <- event:
		return true
	default:
		return false
	}
}

// readOutputLines retains a bounded prefix of each line and drains the rest.
// Continuing to read is required because abandoning a full output pipe can
// leave the managed process blocked in write while command.Wait waits for it.
func readOutputLines(reader io.Reader, yield func(string)) error {
	buffer := bufio.NewReaderSize(reader, MaxOutputLineBytes)
	line := make([]byte, 0, MaxOutputLineBytes)
	truncated := false
	sawData := false

	for {
		fragment, err := buffer.ReadSlice('\n')
		hadNewline := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if hadNewline {
			fragment = fragment[:len(fragment)-1]
		}
		if len(fragment) > 0 {
			sawData = true
		}

		remaining := MaxOutputLineBytes - len(line)
		if remaining > 0 {
			kept := min(remaining, len(fragment))
			line = append(line, fragment[:kept]...)
			fragment = fragment[kept:]
		}
		if len(fragment) > 0 {
			truncated = true
		}

		complete := hadNewline || (err != nil && !errors.Is(err, bufio.ErrBufferFull))
		if complete && (sawData || hadNewline) {
			if !truncated && len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			yield(formatOutputLine(line, truncated))
			line = line[:0]
			truncated = false
			sawData = false
		}

		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

func formatOutputLine(line []byte, truncated bool) string {
	if !truncated {
		return string(line)
	}
	prefixBytes := MaxOutputLineBytes - len(truncationBoundary) - len(truncationMarker)
	return string(line[:min(len(line), prefixBytes)]) + truncationBoundary + truncationMarker
}

// StopAll asks every process group to exit, then force-stops any survivors.
func (s *Supervisor) StopAll() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		s.mu.Unlock()
		for index := range s.processes {
			item := &s.processes[index]
			item.lifecycle.Lock()
			s.signal(index, syscall.SIGTERM)
			item.lifecycle.Unlock()
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && s.anyRunning() {
			time.Sleep(25 * time.Millisecond)
		}
		for index := range s.processes {
			s.signal(index, syscall.SIGKILL)
		}
	})
}

func (s *Supervisor) signal(index int, signal syscall.Signal) {
	item := &s.processes[index]
	item.mu.Lock()
	defer item.mu.Unlock()
	if !item.running || item.command == nil || item.command.Process == nil {
		return
	}
	if signal == syscall.SIGTERM {
		item.stopRequested = true
		select {
		case s.events <- Event{Index: index, Generation: item.generation, State: Stopping}:
		default:
		}
	}
	_ = syscall.Kill(-item.command.Process.Pid, signal)
}

func (s *Supervisor) stopAndWait(index int) {
	item := &s.processes[index]
	item.mu.Lock()
	if !item.running || item.command == nil || item.command.Process == nil {
		item.mu.Unlock()
		return
	}
	item.stopRequested = true
	command := item.command
	done := item.done
	generation := item.generation
	item.mu.Unlock()

	s.events <- Event{Index: index, Generation: generation, State: Stopping}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func (s *Supervisor) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

func (s *Supervisor) anyRunning() bool {
	for index := range s.processes {
		item := &s.processes[index]
		item.mu.Lock()
		running := item.running
		item.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

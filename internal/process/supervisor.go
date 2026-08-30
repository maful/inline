package process

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/maful/inline/internal/procfile"
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
	Index int
	Line  string
	State State
	Err   error
	PID   int
}

type runningProcess struct {
	mu      sync.Mutex
	command *exec.Cmd
	running bool
}

// Supervisor starts, observes, and stops all Procfile commands.
type Supervisor struct {
	definitions []procfile.Process
	processes   []runningProcess
	events      chan Event
	stopOnce    sync.Once
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
		events:      make(chan Event, 2048),
		shell:       shell,
		interactive: interactive,
	}
}

func (s *Supervisor) Events() <-chan Event { return s.events }

func (s *Supervisor) StartAll() {
	for index, definition := range s.definitions {
		s.start(index, definition)
	}
}

func (s *Supervisor) start(index int, definition procfile.Process) {
	command := s.buildCommand(definition.Command)
	// Interactive shells try to access their controlling terminal for job
	// control. Because each managed command runs in the background, that can
	// suspend the shell with SIGTTOU before the Procfile command starts. A new
	// session removes the controlling terminal and also makes the process PID
	// its process-group ID, preserving grouped shutdown via kill(-pid, signal).
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	reader, writer, err := os.Pipe()
	if err != nil {
		s.events <- Event{Index: index, State: Failed, Err: fmt.Errorf("create output pipe: %w", err)}
		return
	}
	command.Stdout = writer
	command.Stderr = writer

	item := &s.processes[index]
	item.mu.Lock()
	item.command = command
	item.running = true
	item.mu.Unlock()

	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		item.mu.Lock()
		item.running = false
		item.mu.Unlock()
		s.events <- Event{Index: index, State: Failed, Err: err}
		return
	}

	s.events <- Event{Index: index, State: Running, PID: command.Process.Pid}
	go s.observe(index, command, reader, writer)
}

func (s *Supervisor) buildCommand(script string) *exec.Cmd {
	flag := "-c"
	if s.interactive {
		flag = "-ic"
	}
	return exec.Command(s.shell, flag, script)
}

func (s *Supervisor) observe(index int, command *exec.Cmd, reader *os.File, writer *os.File) {
	linesDone := make(chan struct{})
	go func() {
		defer close(linesDone)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			s.events <- Event{Index: index, Line: scanner.Text()}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
			s.events <- Event{Index: index, Line: fmt.Sprintf("inline: read output: %v", err)}
		}
	}()

	err := command.Wait()
	writer.Close()
	<-linesDone
	reader.Close()

	item := &s.processes[index]
	item.mu.Lock()
	item.running = false
	item.mu.Unlock()

	if err != nil {
		s.events <- Event{Index: index, State: Failed, Err: err}
		return
	}
	s.events <- Event{Index: index, State: Exited}
}

// StopAll asks every process group to exit, then force-stops any survivors.
func (s *Supervisor) StopAll() {
	s.stopOnce.Do(func() {
		for index := range s.processes {
			s.signal(index, syscall.SIGTERM)
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
		select {
		case s.events <- Event{Index: index, State: Stopping}:
		default:
		}
	}
	_ = syscall.Kill(-item.command.Process.Pid, signal)
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

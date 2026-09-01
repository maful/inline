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
		events:      make(chan Event, 2048),
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
	linesDone := make(chan struct{})
	go func() {
		defer close(linesDone)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			s.events <- Event{Index: index, Generation: generation, Line: scanner.Text()}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
			s.events <- Event{Index: index, Generation: generation, Line: fmt.Sprintf("inline: read output: %v", err)}
		}
	}()

	err := command.Wait()
	writer.Close()
	<-linesDone
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
	if err != nil {
		s.events <- Event{Index: index, Generation: generation, State: Failed, Err: err}
		return
	}
	s.events <- Event{Index: index, Generation: generation, State: Exited}
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

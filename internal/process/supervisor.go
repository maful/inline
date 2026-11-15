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

// notStarted is the zero State: the supervisor has not launched the command
// yet. It is never sent in an Event, where an empty State means "unchanged".
const notStarted State = ""

// alive reports whether the process has been spawned and not yet reaped, which
// is exactly when it can be signalled.
func (state State) alive() bool { return state == Running || state == Stopping }

// runningProcess is the supervisor's record of one Procfile command. state is
// its only lifecycle field and moves along these edges and no others:
//
//	notStarted, Exited, Failed → Starting   startLocked
//	Starting → Running | Failed             startLocked
//	Running  → Stopping                     signal, stopAndWait
//	Running  → Exited | Failed              observe
//	Stopping → Exited                       observe
//
// command and done are set exactly while state is Starting, Running, or
// Stopping. startLocked refuses a record in any of those states, so a record
// is never replaced while its observer is still running. lifecycle serializes whole start and restart sequences; mu guards
// the fields below it.
type runningProcess struct {
	lifecycle  sync.Mutex
	mu         sync.Mutex
	state      State
	command    *exec.Cmd
	done       chan struct{}
	generation uint64
}

// finish records a terminal state and releases everything waiting on done.
// The caller must hold mu.
func (p *runningProcess) finish(state State) {
	p.state = state
	p.command = nil
	close(p.done)
	p.done = nil
}

// Supervisor starts, observes, and stops all Procfile commands.
type Supervisor struct {
	definitions []procfile.Process
	processes   []runningProcess
	events      chan Event
	stopOnce    sync.Once
	stopped     chan struct{}
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
		stopped:     make(chan struct{}),
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
// A call that arrives while a start, restart, or shutdown of the same command
// is in flight is dropped, so repeated requests merge into one restart.
func (s *Supervisor) Restart(index int) {
	if index < 0 || index >= len(s.processes) {
		return
	}

	item := &s.processes[index]
	if !item.lifecycle.TryLock() {
		return
	}
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

// startLocked launches the command unless the record is still live. The caller
// must hold the record's lifecycle lock.
func (s *Supervisor) startLocked(index int, definition procfile.Process, restarted bool) {
	item := &s.processes[index]
	item.mu.Lock()
	if item.state == Starting || item.state.alive() {
		item.mu.Unlock()
		return
	}
	item.mu.Unlock()

	command := s.buildCommand(definition.Command)
	// Interactive shells try to access their controlling terminal for job
	// control. Because each managed command runs in the background, that can
	// suspend the shell with SIGTTOU before the Procfile command starts. A new
	// session removes the controlling terminal and also makes the process PID
	// its process-group ID, preserving grouped shutdown via kill(-pid, signal).
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	item.mu.Lock()
	item.generation++
	generation := item.generation
	item.state = Starting
	item.command = command
	item.done = make(chan struct{})
	item.mu.Unlock()

	starting := Event{Index: index, Generation: generation, State: Starting}
	if restarted {
		starting.Line = "inline: restarting…"
	}
	if !s.send(starting) {
		// Shutdown began before anything was spawned, so there is nothing
		// for StopAll to signal.
		item.mu.Lock()
		item.finish(Failed)
		item.mu.Unlock()
		return
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		item.mu.Lock()
		item.finish(Failed)
		item.mu.Unlock()
		s.send(Event{Index: index, Generation: generation, State: Failed, Err: fmt.Errorf("create output pipe: %w", err)})
		return
	}
	command.Stdout = writer
	command.Stderr = writer

	if err := command.Start(); err != nil {
		reader.Close()
		writer.Close()
		item.mu.Lock()
		item.finish(Failed)
		item.mu.Unlock()
		s.send(Event{Index: index, Generation: generation, State: Failed, Err: err})
		return
	}

	item.mu.Lock()
	item.state = Running
	item.mu.Unlock()
	s.send(Event{Index: index, Generation: generation, State: Running, PID: command.Process.Pid})
	go s.observe(index, generation, command, reader, writer)
}

func (s *Supervisor) buildCommand(script string) *exec.Cmd {
	flag := "-c"
	if s.interactive {
		flag = "-ic"
	}
	return exec.Command(s.shell, flag, script)
}

func (s *Supervisor) observe(index int, generation uint64, command *exec.Cmd, reader *os.File, writer *os.File) {
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
	stopped := item.state == Stopping
	if stopped || err == nil {
		item.finish(Exited)
	} else {
		item.finish(Failed)
	}
	item.mu.Unlock()

	// A requested stop is reported by whoever asked for it, not as an exit.
	if stopped {
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
		s.send(Event{Index: index, Generation: generation, Line: line, State: Failed, Err: err})
		return
	}
	s.send(Event{Index: index, Generation: generation, Line: line, State: Exited})
}

// send delivers a lifecycle event, waiting for room in the queue. It gives up
// and reports false once StopAll has begun: nothing reads events after the UI
// has quit, and a sender that holds a lifecycle lock would otherwise block
// StopAll forever.
func (s *Supervisor) send(event Event) bool {
	select {
	case s.events <- event:
		return true
	case <-s.stopped:
		return false
	}
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
		close(s.stopped)
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
	if !item.state.alive() {
		return
	}
	if signal == syscall.SIGTERM {
		item.state = Stopping
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
	if !item.state.alive() {
		item.mu.Unlock()
		return
	}
	item.state = Stopping
	command := item.command
	done := item.done
	generation := item.generation
	item.mu.Unlock()

	s.send(Event{Index: index, Generation: generation, State: Stopping})
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
	select {
	case <-s.stopped:
		return true
	default:
		return false
	}
}

func (s *Supervisor) anyRunning() bool {
	for index := range s.processes {
		item := &s.processes[index]
		item.mu.Lock()
		alive := item.state.alive()
		item.mu.Unlock()
		if alive {
			return true
		}
	}
	return false
}

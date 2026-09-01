package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/maful/inline/internal/procfile"
)

func TestSupervisorCapturesOutputAndExit(t *testing.T) {
	supervisor := newSupervisor([]procfile.Process{
		{Name: "one", Command: "printf 'stdout line\\n'; printf 'stderr line\\n' >&2"},
	}, "/bin/sh", false)
	supervisor.StartAll()
	t.Cleanup(supervisor.StopAll)

	deadline := time.After(3 * time.Second)
	var lines []string
	for {
		select {
		case event := <-supervisor.Events():
			if event.State == Running && event.PID <= 0 {
				t.Fatalf("running event PID = %d, want a positive PID", event.PID)
			}
			if event.Line != "" {
				lines = append(lines, event.Line)
			}
			if event.State == Failed {
				t.Fatalf("process failed: %v", event.Err)
			}
			if event.State == Exited {
				output := strings.Join(lines, "\n")
				if !strings.Contains(output, "stdout line") || !strings.Contains(output, "stderr line") {
					t.Fatalf("captured output = %q", output)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for process to exit")
		}
	}
}

func TestStopAllTerminatesProcessGroup(t *testing.T) {
	supervisor := newSupervisor([]procfile.Process{{Name: "long", Command: "sleep 30"}}, "/bin/sh", false)
	supervisor.StartAll()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-supervisor.Events():
			if event.State == Running {
				pgid, err := syscall.Getpgid(event.PID)
				if err != nil {
					t.Fatalf("Getpgid(%d): %v", event.PID, err)
				}
				if pgid != event.PID {
					t.Fatalf("process group = %d, want session leader PID %d", pgid, event.PID)
				}
				started := time.Now()
				supervisor.StopAll()
				if elapsed := time.Since(started); elapsed > 3*time.Second {
					t.Fatalf("StopAll() took %s", elapsed)
				}
				if supervisor.anyRunning() {
					t.Fatal("process is still running after StopAll()")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for process to start")
		}
	}
}

func TestRestartReplacesSelectedProcess(t *testing.T) {
	supervisor := newSupervisor([]procfile.Process{
		{Name: "long", Command: "trap 'exit 0' TERM; while :; do sleep 1; done"},
		{Name: "other", Command: "trap 'exit 0' TERM; while :; do sleep 1; done"},
	}, "/bin/sh", false)
	supervisor.StartAll()
	t.Cleanup(supervisor.StopAll)

	first := waitForProcessEvent(t, supervisor.Events(), func(event Event) bool {
		return event.Index == 0 && event.State == Running
	})
	if first.Generation != 1 {
		t.Fatalf("first generation = %d, want 1", first.Generation)
	}
	other := waitForProcessEvent(t, supervisor.Events(), func(event Event) bool {
		return event.Index == 1 && event.State == Running
	})

	started := time.Now()
	supervisor.Restart(0)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Restart() took %s", elapsed)
	}

	var sawStopping, sawStarting bool
	second := waitForProcessEvent(t, supervisor.Events(), func(event Event) bool {
		if event.Index != 0 {
			return false
		}
		if event.State == Failed {
			t.Fatalf("restart emitted failed state: %v", event.Err)
		}
		switch event.State {
		case Stopping:
			sawStopping = true
		case Starting:
			sawStarting = true
			if event.Line != "inline: restarting…" {
				t.Fatalf("restart line = %q", event.Line)
			}
		}
		return event.State == Running && event.Generation == 2
	})

	if !sawStopping || !sawStarting {
		t.Fatalf("restart states: stopping=%t, starting=%t", sawStopping, sawStarting)
	}
	if second.PID == first.PID {
		t.Fatalf("restarted PID = %d, want a PID different from %d", second.PID, first.PID)
	}
	if err := syscall.Kill(-first.PID, 0); err != syscall.ESRCH {
		t.Fatalf("old process group still exists: kill(-%d, 0) = %v", first.PID, err)
	}
	otherItem := &supervisor.processes[1]
	otherItem.mu.Lock()
	otherPID := 0
	if otherItem.command != nil && otherItem.command.Process != nil {
		otherPID = otherItem.command.Process.Pid
	}
	otherGeneration := otherItem.generation
	otherRunning := otherItem.running
	otherItem.mu.Unlock()
	if !otherRunning || otherPID != other.PID || otherGeneration != 1 {
		t.Fatalf("other process changed: running=%t, pid=%d, generation=%d", otherRunning, otherPID, otherGeneration)
	}
}

func TestConcurrentRestartsAreSerialized(t *testing.T) {
	supervisor := newSupervisor([]procfile.Process{
		{Name: "long", Command: "trap 'exit 0' TERM; while :; do sleep 1; done"},
	}, "/bin/sh", false)
	supervisor.StartAll()
	t.Cleanup(supervisor.StopAll)
	waitForProcessEvent(t, supervisor.Events(), func(event Event) bool {
		return event.State == Running && event.Generation == 1
	})

	var restarts sync.WaitGroup
	restarts.Add(2)
	for range 2 {
		go func() {
			defer restarts.Done()
			supervisor.Restart(0)
		}()
	}
	restarts.Wait()

	final := waitForProcessEvent(t, supervisor.Events(), func(event Event) bool {
		if event.State == Failed {
			t.Fatalf("concurrent restart emitted failed state: %v", event.Err)
		}
		return event.State == Running && event.Generation == 3
	})
	if final.PID <= 0 {
		t.Fatalf("final PID = %d, want a positive PID", final.PID)
	}
}

func TestRestartStartsExitedProcess(t *testing.T) {
	supervisor := newSupervisor([]procfile.Process{{Name: "short", Command: "printf 'done\\n'"}}, "/bin/sh", false)
	supervisor.StartAll()
	t.Cleanup(supervisor.StopAll)

	waitForProcessEvent(t, supervisor.Events(), func(event Event) bool {
		return event.State == Exited && event.Generation == 1
	})
	supervisor.Restart(0)

	restarted := waitForProcessEvent(t, supervisor.Events(), func(event Event) bool {
		return event.State == Running && event.Generation == 2
	})
	if restarted.PID <= 0 {
		t.Fatalf("restarted PID = %d, want a positive PID", restarted.PID)
	}
}

func TestNewSupervisorUsesInteractiveUserShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")

	supervisor := NewSupervisor([]procfile.Process{{Name: "alias", Command: "wngrok localhost:4242"}})
	command := supervisor.buildCommand("wngrok localhost:4242")
	if command.Path != "/bin/zsh" {
		t.Fatalf("command path = %q, want /bin/zsh", command.Path)
	}
	want := []string{"/bin/zsh", "-ic", "wngrok localhost:4242"}
	if strings.Join(command.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
}

func TestInteractiveZshLoadsAliases(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}

	dotdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dotdir, ".zshrc"), []byte("alias wngrok='printf alias-loaded'\n"), 0o600); err != nil {
		t.Fatalf("write .zshrc: %v", err)
	}
	t.Setenv("ZDOTDIR", dotdir)

	supervisor := newSupervisor([]procfile.Process{{Name: "alias", Command: "wngrok"}}, zsh, true)
	supervisor.StartAll()
	t.Cleanup(supervisor.StopAll)

	deadline := time.After(3 * time.Second)
	var output strings.Builder
	for {
		select {
		case event := <-supervisor.Events():
			output.WriteString(event.Line)
			if event.State == Failed {
				t.Fatalf("aliased process failed: %v; output: %s", event.Err, output.String())
			}
			if event.State == Exited {
				if !strings.Contains(output.String(), "alias-loaded") {
					t.Fatalf("output = %q, want alias-loaded", output.String())
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for aliased process")
		}
	}
}

func waitForProcessEvent(t *testing.T, events <-chan Event, matches func(Event) bool) Event {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if matches(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("timed out waiting for process event")
		}
	}
}

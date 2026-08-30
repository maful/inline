package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

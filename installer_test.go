package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestInstallerCreatesStandaloneLayout(t *testing.T) {
	home := t.TempDir()
	output, err := runInstaller(t, home)
	if err != nil {
		t.Fatalf("run installer: %v\n%s", err, output)
	}

	installed := filepath.Join(home, ".local", "bin", "inline")
	resolved, err := filepath.EvalSymlinks(installed)
	if err != nil {
		t.Fatalf("resolve installed binary: %v\n%s", err, output)
	}
	wantSuffix := filepath.Join("inline", "releases", "v1.2.3-"+installerOS(t)+"-"+installerArch(t), "bin", "inline")
	if !strings.HasSuffix(resolved, wantSuffix) {
		t.Fatalf("installed binary = %q, want suffix %q", resolved, wantSuffix)
	}
	if !strings.Contains(output, "Checksum verified") || !strings.Contains(output, "Installed Inline v1.2.3") {
		t.Fatalf("installer output = %q", output)
	}
	if _, err := os.Stat(installerLock(home)); !os.IsNotExist(err) {
		t.Fatalf("install lock still exists after installing: stat = %v", err)
	}
}

func TestInstallerRefusesToRunWhileLockOwnerIsAlive(t *testing.T) {
	home := t.TempDir()
	// This test process stands in for a running "inline update".
	createInstallerLock(t, home, strconv.Itoa(os.Getpid()))

	output, err := runInstaller(t, home)
	if err == nil || !strings.Contains(output, "another Inline installation or update is running") {
		t.Fatalf("installer error = %v, want a running installation or update\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(installerLock(home), "pid")); err != nil {
		t.Fatalf("installer removed a lock it does not own: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "inline")); !os.IsNotExist(err) {
		t.Fatalf("installer installed despite the lock: lstat = %v", err)
	}
}

func TestInstallerTakesOverStaleLock(t *testing.T) {
	finished := exec.Command("true")
	if err := finished.Run(); err != nil {
		t.Fatalf("run short process: %v", err)
	}
	for name, prepare := range map[string]func(t *testing.T, home string){
		"owner exited": func(t *testing.T, home string) {
			createInstallerLock(t, home, strconv.Itoa(finished.Process.Pid))
		},
		"no owner and old": func(t *testing.T, home string) {
			createInstallerLock(t, home, "")
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(installerLock(home), old, old); err != nil {
				t.Fatalf("age install lock: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			prepare(t, home)

			output, err := runInstaller(t, home)
			if err != nil {
				t.Fatalf("run installer: %v\n%s", err, output)
			}
			if !strings.Contains(output, "Installed Inline v1.2.3") {
				t.Fatalf("installer output = %q", output)
			}
			if _, err := os.Stat(installerLock(home)); !os.IsNotExist(err) {
				t.Fatalf("install lock still exists after installing: stat = %v", err)
			}
		})
	}
}

func TestInstallerLeavesFreshLockWithoutOwnerAlone(t *testing.T) {
	home := t.TempDir()
	createInstallerLock(t, home, "")

	output, err := runInstaller(t, home)
	if err == nil || !strings.Contains(output, "another Inline installation or update is running") {
		t.Fatalf("installer error = %v, want a running installation or update\n%s", err, output)
	}
	if _, err := os.Stat(installerLock(home)); err != nil {
		t.Fatalf("installer removed a fresh lock: %v", err)
	}
}

func installerLock(home string) string {
	return filepath.Join(home, "data", "inline", "install.lock")
}

// createInstallerLock leaves an install lock behind, owned by pid when given.
func createInstallerLock(t *testing.T, home, pid string) {
	t.Helper()
	if err := os.MkdirAll(installerLock(home), 0o700); err != nil {
		t.Fatalf("create install lock: %v", err)
	}
	if pid == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(installerLock(home), "pid"), []byte(pid+"\n"), 0o600); err != nil {
		t.Fatalf("write install lock owner: %v", err)
	}
}

// runInstaller runs install.sh against a local release fixture with home as
// the user's home directory.
func runInstaller(t *testing.T, home string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required for the installer integration test")
	}
	archiveName := "inline_" + installerOS(t) + "_" + installerArch(t) + ".tar.gz"
	archive := installerArchive(t)
	sum := sha256.Sum256(archive)
	checksums := hex.EncodeToString(sum[:]) + "  " + archiveName + "\n"

	releaseDirectory := filepath.Join(home, "release-assets")
	if err := os.Mkdir(releaseDirectory, 0o755); err != nil {
		t.Fatalf("create release fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(releaseDirectory, archiveName), archive, 0o600); err != nil {
		t.Fatalf("write release archive: %v", err)
	}
	if err := os.WriteFile(filepath.Join(releaseDirectory, "checksums.txt"), []byte(checksums), 0o600); err != nil {
		t.Fatalf("write checksums: %v", err)
	}
	command := exec.Command("sh", "install.sh")
	command.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"INLINE_RELEASE_BASE_URL=file://"+releaseDirectory,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func installerArchive(t *testing.T) []byte {
	t.Helper()
	binary := []byte("#!/bin/sh\nif [ \"${1:-}\" = version ] && [ \"${2:-}\" = --short ]; then echo 1.2.3; exit 0; fi\nexit 1\n")
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "inline", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write archive header: %v", err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatalf("write archive body: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buffer.Bytes()
}

func installerOS(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("uname", "-s").Output()
	if err != nil {
		t.Fatalf("uname -s: %v", err)
	}
	switch strings.TrimSpace(string(output)) {
	case "Darwin":
		return "darwin"
	case "Linux":
		return "linux"
	default:
		t.Skip("installer does not support this operating system")
		return ""
	}
}

func installerArch(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("uname", "-m").Output()
	if err != nil {
		t.Fatalf("uname -m: %v", err)
	}
	switch strings.TrimSpace(string(output)) {
	case "x86_64", "amd64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		t.Skip("installer does not support this architecture")
		return ""
	}
}

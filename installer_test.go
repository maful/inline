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
	"strings"
	"testing"
)

func TestInstallerCreatesStandaloneLayout(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required for the installer integration test")
	}
	archiveName := "inline_" + installerOS(t) + "_" + installerArch(t) + ".tar.gz"
	archive := installerArchive(t)
	sum := sha256.Sum256(archive)
	checksums := hex.EncodeToString(sum[:]) + "  " + archiveName + "\n"

	home := t.TempDir()
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
	if !strings.Contains(string(output), "Checksum verified") || !strings.Contains(string(output), "Installed Inline v1.2.3") {
		t.Fatalf("installer output = %q", output)
	}
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

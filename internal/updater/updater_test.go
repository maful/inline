package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateInstallsAndActivatesRelease(t *testing.T) {
	root, executable := createInstallation(t, "v1.0.0-darwin-arm64")
	archive := releaseArchive(t, []byte("new inline binary"))
	archiveName := "inline_darwin_arm64.tar.gz"
	client, releaseAPI := releaseClient("v1.1.0", archiveName, archive, checksumLine(archiveName, archive))

	var output bytes.Buffer
	err := Update(context.Background(), Options{
		CurrentVersion: "1.0.0",
		Stdout:         &output,
		HTTPClient:     client,
		ReleaseAPI:     releaseAPI,
		Executable:     executable,
		GOOS:           "darwin",
		GOARCH:         "arm64",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	current, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil {
		t.Fatalf("read current link: %v", err)
	}
	if got, want := current, filepath.Join("releases", "v1.1.0-darwin-arm64"); got != want {
		t.Fatalf("current link = %q, want %q", got, want)
	}
	installed, err := os.ReadFile(filepath.Join(root, current, "bin", "inline"))
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if got, want := string(installed), "new inline binary"; got != want {
		t.Fatalf("installed binary = %q, want %q", got, want)
	}
	if !strings.Contains(output.String(), "Checksum verified") || !strings.Contains(output.String(), "v1.0.0 -> v1.1.0") {
		t.Fatalf("update output = %q", output.String())
	}
}

func TestUpdateRejectsChecksumMismatch(t *testing.T) {
	root, executable := createInstallation(t, "v1.0.0-linux-amd64")
	archive := releaseArchive(t, []byte("tampered binary"))
	archiveName := "inline_linux_amd64.tar.gz"
	client, releaseAPI := releaseClient("v1.1.0", archiveName, archive, strings.Repeat("0", 64)+"  "+archiveName+"\n")

	err := Update(context.Background(), Options{
		CurrentVersion: "1.0.0",
		HTTPClient:     client,
		ReleaseAPI:     releaseAPI,
		Executable:     executable,
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("update error = %v, want checksum mismatch", err)
	}
	current, readErr := os.Readlink(filepath.Join(root, "current"))
	if readErr != nil {
		t.Fatalf("read current link: %v", readErr)
	}
	if got, want := current, filepath.Join("releases", "v1.0.0-linux-amd64"); got != want {
		t.Fatalf("current link = %q, want %q", got, want)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{left: "v1.2.3", right: "1.2.3", want: 0},
		{left: "1.2.2", right: "1.2.3", want: -1},
		{left: "2.0.0", right: "1.9.9", want: 1},
		{left: "1.0.0-rc.1", right: "1.0.0", want: -1},
		{left: "1.0.0-rc.2", right: "1.0.0-rc.10", want: -1},
	}
	for _, test := range tests {
		got, err := compareVersions(test.left, test.right)
		if err != nil {
			t.Fatalf("compare %q and %q: %v", test.left, test.right, err)
		}
		if got != test.want {
			t.Errorf("compare %q and %q = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestCompareVersionsRejectsUnsafeTag(t *testing.T) {
	if _, err := compareVersions("1.0.0", "1.1.0+../../outside"); err == nil {
		t.Fatal("compareVersions accepted a tag containing a path")
	}
}

func createInstallation(t *testing.T, releaseName string) (string, string) {
	t.Helper()
	root := t.TempDir()
	releaseDir := filepath.Join(root, "releases", releaseName)
	if err := os.MkdirAll(filepath.Join(releaseDir, "bin"), 0o755); err != nil {
		t.Fatalf("create release: %v", err)
	}
	executable := filepath.Join(releaseDir, "bin", "inline")
	if err := os.WriteFile(executable, []byte("old inline binary"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	if err := os.Symlink(filepath.Join("releases", releaseName), filepath.Join(root, "current")); err != nil {
		t.Fatalf("create current link: %v", err)
	}
	return root, executable
}

func releaseArchive(t *testing.T, contents []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "inline", Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write archive header: %v", err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
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

func checksumLine(name string, contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]) + "  " + name + "\n"
}

func releaseClient(tag, archiveName string, archive []byte, checksums string) (*http.Client, string) {
	const baseURL = "https://releases.inline.test"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body []byte
		status := http.StatusOK
		switch request.URL.Path {
		case "/release":
			var buffer bytes.Buffer
			_ = json.NewEncoder(&buffer).Encode(release{
				TagName: tag,
				Assets: []asset{
					{Name: archiveName, BrowserDownloadURL: baseURL + "/" + archiveName},
					{Name: "checksums.txt", BrowserDownloadURL: baseURL + "/checksums.txt"},
				},
			})
			body = buffer.Bytes()
		case "/" + archiveName:
			body = archive
		case "/checksums.txt":
			body = []byte(checksums)
		default:
			status = http.StatusNotFound
			body = []byte("not found")
		}
		return &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})
	return &http.Client{Transport: transport}, baseURL + "/release"
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

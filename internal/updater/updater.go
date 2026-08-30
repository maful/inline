package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultReleaseAPI = "https://api.github.com/repos/maful/inline/releases/latest"

const maxDownloadSize = 128 << 20

type Options struct {
	CurrentVersion string
	Stdout         io.Writer
	HTTPClient     *http.Client
	ReleaseAPI     string
	Executable     string
	GOOS           string
	GOARCH         string
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func Update(ctx context.Context, options Options) error {
	options = defaults(options)
	root, err := installRoot(options.Executable)
	if err != nil {
		return err
	}

	unlock, err := lock(root)
	if err != nil {
		return err
	}
	defer unlock()

	latest, err := fetchRelease(ctx, options)
	if err != nil {
		return err
	}
	comparison, err := compareVersions(options.CurrentVersion, latest.TagName)
	if err != nil {
		return err
	}
	if comparison >= 0 {
		fmt.Fprintf(options.Stdout, "Inline %s is already up to date.\n", displayVersion(options.CurrentVersion))
		return nil
	}

	archiveName := fmt.Sprintf("inline_%s_%s.tar.gz", options.GOOS, options.GOARCH)
	archiveAsset, ok := findAsset(latest.Assets, archiveName)
	if !ok {
		return fmt.Errorf("release %s does not contain %s", latest.TagName, archiveName)
	}
	checksumAsset, ok := findAsset(latest.Assets, "checksums.txt")
	if !ok {
		return fmt.Errorf("release %s does not contain checksums.txt", latest.TagName)
	}

	releaseName := fmt.Sprintf("%s-%s-%s", normalizeVersion(latest.TagName), options.GOOS, options.GOARCH)
	releaseDir := filepath.Join(root, "releases", releaseName)
	if executableExists(releaseDir) {
		if err := switchCurrent(root, releaseName); err != nil {
			return err
		}
		fmt.Fprintf(options.Stdout, "Updated Inline %s -> %s.\n", displayVersion(options.CurrentVersion), displayVersion(latest.TagName))
		return nil
	}

	fmt.Fprintf(options.Stdout, "Updating Inline %s -> %s\n", displayVersion(options.CurrentVersion), displayVersion(latest.TagName))
	temporaryDir, err := os.MkdirTemp(filepath.Join(root, "releases"), ".update-")
	if err != nil {
		return fmt.Errorf("create update directory: %w", err)
	}
	defer os.RemoveAll(temporaryDir)

	archivePath := filepath.Join(temporaryDir, archiveName)
	checksumPath := filepath.Join(temporaryDir, "checksums.txt")
	if err := download(ctx, options.HTTPClient, archiveAsset.BrowserDownloadURL, archivePath); err != nil {
		return fmt.Errorf("download %s: %w", archiveName, err)
	}
	if err := download(ctx, options.HTTPClient, checksumAsset.BrowserDownloadURL, checksumPath); err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}
	if err := verifyChecksum(archivePath, checksumPath, archiveName); err != nil {
		return err
	}

	stagedRelease := filepath.Join(temporaryDir, "release")
	if err := os.MkdirAll(filepath.Join(stagedRelease, "bin"), 0o755); err != nil {
		return fmt.Errorf("create release directory: %w", err)
	}
	if err := extractBinary(archivePath, filepath.Join(stagedRelease, "bin", "inline")); err != nil {
		return err
	}
	if err := os.Rename(stagedRelease, releaseDir); err != nil {
		if !executableExists(releaseDir) {
			return fmt.Errorf("install release: %w", err)
		}
	}
	if err := switchCurrent(root, releaseName); err != nil {
		return err
	}

	fmt.Fprintln(options.Stdout, "Checksum verified.")
	fmt.Fprintf(options.Stdout, "Updated Inline %s -> %s.\n", displayVersion(options.CurrentVersion), displayVersion(latest.TagName))
	return nil
}

func defaults(options Options) Options {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if options.ReleaseAPI == "" {
		options.ReleaseAPI = defaultReleaseAPI
	}
	if options.Executable == "" {
		options.Executable, _ = os.Executable()
	}
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	return options
}

func installRoot(executable string) (string, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	releaseDir := filepath.Dir(filepath.Dir(resolved))
	releasesDir := filepath.Dir(releaseDir)
	if filepath.Base(releasesDir) != "releases" || filepath.Base(filepath.Dir(resolved)) != "bin" {
		return "", errors.New("this standalone binary is not managed by the Inline installer; reinstall with install.sh")
	}
	return filepath.Dir(releasesDir), nil
}

func lock(root string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(root, "update.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open update lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another Inline update is already running")
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func fetchRelease(ctx context.Context, options Options) (release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, options.ReleaseAPI, nil)
	if err != nil {
		return release{}, fmt.Errorf("create release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "inline/"+options.CurrentVersion)
	response, err := options.HTTPClient.Do(request)
	if err != nil {
		return release{}, fmt.Errorf("check latest release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("check latest release: server returned %s", response.Status)
	}
	var result release
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result); err != nil {
		return release{}, fmt.Errorf("decode latest release: %w", err)
	}
	if result.TagName == "" {
		return release{}, errors.New("latest release has no tag")
	}
	return result, nil
}

func findAsset(assets []asset, name string) (asset, bool) {
	for _, candidate := range assets {
		if candidate.Name == name && candidate.BrowserDownloadURL != "" {
			return candidate, true
		}
	}
	return asset{}, false
}

func download(ctx context.Context, client *http.Client, url, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "inline-updater")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", response.Status)
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, maxDownloadSize+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if written > maxDownloadSize {
		return fmt.Errorf("download exceeds %d MiB", maxDownloadSize>>20)
	}
	return closeErr
}

func verifyChecksum(archivePath, checksumPath, archiveName string) error {
	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	var expected string
	for _, line := range strings.Split(string(checksumData), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archiveName {
			expected = fields[0]
			break
		}
	}
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("checksums.txt does not contain a valid checksum for %s", archiveName)
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("checksum for %s is invalid", archiveName)
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open downloaded archive: %w", err)
	}
	defer archive.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, archive); err != nil {
		return fmt.Errorf("checksum downloaded archive: %w", err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	return nil
}

func extractBinary(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open compressed release archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read release archive: %w", err)
		}
		cleanName := filepath.ToSlash(filepath.Clean(header.Name))
		if header.Typeflag != tar.TypeReg || (cleanName != "inline" && !strings.HasSuffix(cleanName, "/inline")) {
			continue
		}
		if header.Size <= 0 || header.Size > maxDownloadSize {
			return fmt.Errorf("inline binary has invalid size %d", header.Size)
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("create updated binary: %w", err)
		}
		written, copyErr := io.Copy(output, io.LimitReader(tarReader, header.Size))
		closeErr := output.Close()
		if copyErr != nil {
			return fmt.Errorf("extract updated binary: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close updated binary: %w", closeErr)
		}
		if written != header.Size {
			return fmt.Errorf("updated binary is truncated: got %d bytes, want %d", written, header.Size)
		}
		return nil
	}
	return errors.New("release archive does not contain the inline binary")
}

func switchCurrent(root, releaseName string) error {
	temporaryLink := filepath.Join(root, fmt.Sprintf(".current-%d", os.Getpid()))
	_ = os.Remove(temporaryLink)
	if err := os.Symlink(filepath.Join("releases", releaseName), temporaryLink); err != nil {
		return fmt.Errorf("create current release link: %w", err)
	}
	if err := os.Rename(temporaryLink, filepath.Join(root, "current")); err != nil {
		_ = os.Remove(temporaryLink)
		return fmt.Errorf("activate release: %w", err)
	}
	return nil
}

func executableExists(releaseDir string) bool {
	info, err := os.Stat(filepath.Join(releaseDir, "bin", "inline"))
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func displayVersion(value string) string {
	if strings.HasPrefix(value, "v") {
		return value
	}
	return "v" + value
}

func normalizeVersion(value string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(value), "v")
}

func compareVersions(left, right string) (int, error) {
	leftVersion, err := parseVersion(left)
	if err != nil {
		return 0, fmt.Errorf("parse current version: %w", err)
	}
	rightVersion, err := parseVersion(right)
	if err != nil {
		return 0, fmt.Errorf("parse latest version: %w", err)
	}
	for index := range leftVersion.numbers {
		if leftVersion.numbers[index] < rightVersion.numbers[index] {
			return -1, nil
		}
		if leftVersion.numbers[index] > rightVersion.numbers[index] {
			return 1, nil
		}
	}
	return comparePrerelease(leftVersion.prerelease, rightVersion.prerelease), nil
}

type semanticVersion struct {
	numbers    [3]int
	prerelease []string
}

func parseVersion(value string) (semanticVersion, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if strings.Count(value, "+") > 1 {
		return semanticVersion{}, fmt.Errorf("%q is not a semantic version", value)
	}
	withoutBuild := value
	if plus := strings.IndexByte(value, '+'); plus >= 0 {
		if !validIdentifiers(value[plus+1:], false) {
			return semanticVersion{}, fmt.Errorf("%q is not a semantic version", value)
		}
		withoutBuild = value[:plus]
	}
	parts := strings.SplitN(withoutBuild, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return semanticVersion{}, fmt.Errorf("%q is not a semantic version", value)
	}
	var result semanticVersion
	for index, number := range numbers {
		if len(number) > 1 && number[0] == '0' {
			return semanticVersion{}, fmt.Errorf("%q is not a semantic version", value)
		}
		parsed, err := strconv.Atoi(number)
		if err != nil || parsed < 0 {
			return semanticVersion{}, fmt.Errorf("%q is not a semantic version", value)
		}
		result.numbers[index] = parsed
	}
	if len(parts) == 2 {
		if !validIdentifiers(parts[1], true) {
			return semanticVersion{}, fmt.Errorf("%q is not a semantic version", value)
		}
		result.prerelease = strings.Split(parts[1], ".")
	}
	return result, nil
}

func validIdentifiers(value string, rejectLeadingZeroNumbers bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, character := range identifier {
			if character < '0' || character > '9' {
				numeric = false
			}
			if (character < '0' || character > '9') && (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && character != '-' {
				return false
			}
		}
		if rejectLeadingZeroNumbers && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func comparePrerelease(left, right []string) int {
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	if len(left) == 0 {
		return 1
	}
	if len(right) == 0 {
		return -1
	}
	limit := min(len(left), len(right))
	for index := range limit {
		leftNumber, leftErr := strconv.Atoi(left[index])
		rightNumber, rightErr := strconv.Atoi(right[index])
		switch {
		case leftErr == nil && rightErr == nil && leftNumber != rightNumber:
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		case leftErr == nil && rightErr != nil:
			return -1
		case leftErr != nil && rightErr == nil:
			return 1
		case left[index] < right[index]:
			return -1
		case left[index] > right[index]:
			return 1
		}
	}
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return 0
}

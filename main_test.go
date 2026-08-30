package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	originalVersion := version
	originalDistribution := distribution
	t.Cleanup(func() {
		version = originalVersion
		distribution = originalDistribution
	})
	version = "1.2.3"
	distribution = "standalone"

	var output bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &output, &output); err != nil {
		t.Fatalf("run version: %v", err)
	}
	if got, want := output.String(), "inline v1.2.3 (standalone)\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}

	output.Reset()
	if err := run(context.Background(), []string{"version", "--short"}, &output, &output); err != nil {
		t.Fatalf("run short version: %v", err)
	}
	if got, want := output.String(), "1.2.3\n"; got != want {
		t.Fatalf("short version output = %q, want %q", got, want)
	}
}

func TestUpdateUsesInstallationChannel(t *testing.T) {
	originalDistribution := distribution
	t.Cleanup(func() { distribution = originalDistribution })

	tests := []struct {
		name         string
		distribution string
		want         string
	}{
		{name: "homebrew", distribution: "homebrew", want: "brew upgrade inline"},
		{name: "source", distribution: "source", want: "go install github.com/maful/inline@latest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			distribution = test.distribution
			var output bytes.Buffer
			if err := runUpdate(context.Background(), nil, &output, &output); err != nil {
				t.Fatalf("run update: %v", err)
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("update output = %q, want it to contain %q", output.String(), test.want)
			}
		})
	}
}

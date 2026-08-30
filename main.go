package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/maful/inline/internal/process"
	"github.com/maful/inline/internal/procfile"
	"github.com/maful/inline/internal/ui"
	"github.com/maful/inline/internal/updater"
)

var (
	version      = "dev"
	distribution = "source"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fatal("%v", err)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			return runVersion(args[1:], stdout, stderr)
		case "update", "upgrade":
			return runUpdate(ctx, args[1:], stdout, stderr)
		case "--version":
			if len(args) != 1 {
				return fmt.Errorf("--version does not accept arguments")
			}
			printVersion(stdout, false)
			return nil
		}
	}

	flags := flag.NewFlagSet("inline", flag.ContinueOnError)
	flags.SetOutput(stderr)
	procfilePath := flags.String("f", "Procfile", "path to a Procfile")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	file, err := os.Open(*procfilePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", *procfilePath, err)
	}
	defer file.Close()

	processes, err := procfile.Parse(file)
	if err != nil {
		return fmt.Errorf("parse %s: %w", *procfilePath, err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	branch := currentGitBranch(ctx, workingDirectory)

	supervisor := process.NewSupervisor(processes)
	model := ui.New(processes, supervisor, *procfilePath, workingDirectory, branch, displayVersion(currentVersion()))
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())

	if _, err := program.Run(); err != nil {
		supervisor.StopAll()
		return fmt.Errorf("run inline: %w", err)
	}
	supervisor.StopAll()
	return nil
}

func currentGitBranch(ctx context.Context, workingDirectory string) string {
	output, err := exec.CommandContext(
		ctx,
		"git", "-C", workingDirectory, "symbolic-ref", "--quiet", "--short", "HEAD",
	).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func runVersion(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("inline version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	short := flags.Bool("short", false, "print only the version number")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("version: unexpected argument %q", flags.Arg(0))
	}
	printVersion(stdout, *short)
	return nil
}

func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("inline update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("update: unexpected argument %q", flags.Arg(0))
	}

	switch distribution {
	case "standalone":
		return updater.Update(ctx, updater.Options{
			CurrentVersion: currentVersion(),
			Stdout:         stdout,
		})
	case "homebrew":
		fmt.Fprintln(stdout, "Inline is managed by Homebrew. Run: brew upgrade inline")
		return nil
	case "source":
		fmt.Fprintln(stdout, "Inline was installed from source. Run: go install github.com/maful/inline@latest")
		return nil
	default:
		return fmt.Errorf("updates are not supported for distribution %q", distribution)
	}
}

func printVersion(output io.Writer, short bool) {
	value := currentVersion()
	if short {
		fmt.Fprintln(output, strings.TrimPrefix(value, "v"))
		return
	}
	fmt.Fprintf(output, "inline %s (%s)\n", displayVersion(value), distribution)
}

func displayVersion(value string) string {
	if value != "dev" && !strings.HasPrefix(value, "v") {
		value = "v" + value
	}
	return value
}

func currentVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "inline: "+format+"\n", args...)
	os.Exit(1)
}

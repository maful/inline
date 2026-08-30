# Inline

Inline is a terminal workspace for running and monitoring multiple development processes. Every command gets a separate log screen with fast navigation, automatic following, and scrollback.

Procfile is supported as a first-class, drop-in configuration format—not the boundary of what Inline is designed to become.

## Why Inline

- Keep servers, workers, asset builders, tunnels, and log streams in one terminal without mixing their output.
- Give every process an isolated, ANSI-aware log view with line wrapping and up to 20,000 lines of scrollback.
- Move between processes with the keyboard or mouse while active logs automatically follow the latest output.
- Stop the complete process tree cleanly when leaving Inline.

## Usage

### Install a release

Install the latest release on macOS or Linux:

```sh
curl -fsSL https://github.com/maful/inline/releases/latest/download/install.sh | sh
```

The installer supports Intel/AMD and ARM64 systems. It installs into `~/.local/share/inline` and links the active version at `~/.local/bin/inline`; it never requires `sudo`.

To inspect the installer before running it:

```sh
curl -fsSLO https://github.com/maful/inline/releases/latest/download/install.sh
less install.sh
sh install.sh
```

Update a standalone installation at any time:

```sh
inline update
```

`inline upgrade` is an alias. Updates are downloaded into a new versioned directory, checked against the release SHA-256 checksum, and activated only after verification succeeds.

### Install from source

Install the latest tagged version into your Go binary directory:

```sh
go install github.com/maful/inline@latest
```

Re-run that command to update a source installation. When working from a clone, install the current checkout with:

```sh
go install .
```

Alternatively, build a local binary:

```sh
go build -o inline .
```

Print the installed version and distribution channel with:

```sh
inline version
```

### Define your processes

Add a `Procfile` to the project whose processes you want to run:

```procfile
web: bin/rails server
js: yarn build --watch
css: yarn watch:css
worker: bundle exec sidekiq
```

Each non-comment line uses the format `name: command`. Blank lines and lines beginning with `#` are ignored. Process names must be unique.

### Start Inline

From a project containing a `Procfile`, run:

```sh
inline
```

Use `-f` to load a differently named file:

```sh
inline -f Procfile.dev
```

Commands inherit the directory where Inline was launched. Change into the target project before starting Inline when its commands use relative paths.

To run Inline directly from this repository without installing it:

```sh
go run . -f Procfile.dev
```

### Navigate the app

| Key | Action |
| --- | --- |
| `↑` / `↓`, `j` / `k` | Select a process |
| `Tab`, `Shift+Tab`, `←` / `→` | Select the next or previous process |
| `1`–`9` | Jump directly to a process |
| `PgUp` / `PgDn`, `Ctrl+U` / `Ctrl+D`, mouse wheel | Scroll logs |
| `g` / `Home` | Jump to the top and pause following |
| `G` / `End` | Jump to the bottom and resume following |
| `f` | Toggle automatic bottom-follow |
| `q`, `Ctrl+C` | Stop every process and quit |

Each pane shows the process state, PID, and number of captured lines. Long output wraps to the pane width and reflows when the terminal is resized.

### Shell commands and output

Commands run through the interactive shell named by `$SHELL`, so shell operators, environment assignments, aliases, and functions from your normal shell setup work normally. Inline falls back to `/bin/sh` when `$SHELL` is unavailable.

Some commands suppress output when stdout is not a terminal. For example, ngrok defaults to logging nothing; add `--log=stdout` to see its connection logs in Inline.

### Process shutdown

Quitting sends `SIGTERM` to every managed process group, including child processes. Inline waits up to two seconds before sending `SIGKILL` to anything still running.

## Development

Inline requires Go 1.26.5 or newer. Install the dependencies and build a development binary from the repository root:

```sh
go mod download
go build -o /tmp/inline .
```

Launch that binary from a project containing the Procfile you want to exercise:

```sh
cd /path/to/project
/tmp/inline
```

Run the complete verification suite before submitting a change:

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Test the release configuration locally without publishing:

```sh
goreleaser release --snapshot --clean
```

Publishing a SemVer tag such as `v0.1.0` triggers the release workflow. It tests the repository, creates macOS and Linux archives for AMD64 and ARM64, publishes their checksums and `install.sh` to GitHub Releases, and attests the archives.

# completionctl

Manage generated Zsh completion definitions the way you manage packages:
install them from a tool's own completion command, generate them from `--help`
when there isn't one, and update everything later with a single command.

Every managed file records exactly how it was produced, so updates are
deterministic. Every write is validated and atomic, so a broken generator can
never destroy a working completion.

## Why

Tools installed outside Homebrew (from `go install`, `cargo install`, `npm`,
release tarballs, …) don't ship completions into your fpath. The usual fix is
a one-off `tool completions zsh > ~/.config/zsh/completions/_tool` that you
forget about: nobody remembers the exact flags six months later, stale files
drift as tools grow options, and one bad regeneration wipes a working file.

`completionctl` replaces that with a small lifecycle: discover, validate,
install atomically, record provenance, update on demand, and refresh the
running shell.

## Install

```sh
brew install malh/tap/completionctl
```

or with Go:

```sh
go install github.com/malh/completionctl@latest
```

## Quick start

```sh
# a tool with a native completion command (fnm, starship, kubectl, …)
completionctl install fnm

# a tool without one — parse its --help output instead
completionctl generate amp

# see what's managed and where it came from
completionctl list

# later: regenerate everything exactly the way it was created
completionctl update
```

Definitions land in `$ZDOTDIR/completions` (falling back to
`~/.config/zsh/completions`); override with `--dir`. The directory must be in
your `fpath` before `compinit` runs.

## Commands

| Command | What it does |
|---|---|
| `install TOOL` | Try a curated list of native generator invocations (`completion zsh`, `completions --shell zsh`, …) and install the first valid result. `--generator-arg` supplies a nonstandard invocation. |
| `generate TOOL` | Starting at `TOOL --help` (or `--help-arg`), discover recognized subcommand sections recursively and build a context-aware completion. Never invoked implicitly. |
| `update [TOOL]` | Re-run each definition's recorded generation — native command, help invocation, or import source — and replace it only if the result validates. |
| `list` | Show managed definitions, their source kind, and anything they shadow. |
| `inspect TOOL` | Full provenance as JSON. |
| `import TOOL FILE` | Adopt an existing definition file under management. |
| `remove --yes TOOL` | Delete a managed definition. |
| `completion zsh` | completionctl's own completion. |
| `init zsh` | Print the shell integration (see below). |

### install — native generators first

```
❱ completionctl install fnm
installed fnm
```

When nothing works, each attempt is shown and the fallback is one paste away:

```
❱ completionctl install amp
error: amp has no native completion command
  tried 'amp completion zsh': Unknown command "completion". Run 'amp --help' for the full list.
  tried 'amp completions zsh': Unknown command "completions". Run 'amp --help' for the full list.
  tried 'amp completions --shell zsh': unknown option '--shell'
  tried 'amp completion --shell zsh': unknown option '--shell'

you can generate one from its help output instead:
  completionctl generate amp
```

### generate — help parsing, explicitly

`generate` parses GNU/BSD option tables and the two-line layouts used by
clap and Commander, including `[possible values: …]` annotations. When it
finds a recognized `Commands:`, `Available Commands:`, or `Subcommands:`
section, it runs each listed command with the same help argument and repeats
the process. Thus `tool run --help` supplies the options offered after
`tool run`, independently of the root options:

```
❱ completionctl generate fnm --force   # fnm has a native command; forced for illustration
generated fnm

❱ grep log-level ~/.config/zsh/completions/_fnm
  '--log-level[The log level of fnm commands]:LOG_LEVEL:(quiet error info)' \
```

Discovery is bounded and configurable:

| Flag | Default | Limit |
|---|---:|---|
| `--max-depth` | `3` | deepest subcommand help level inspected (`0` runs only the root help) |
| `--max-commands` | `128` | total help commands run, including the root |
| `--timeout` | `5s` | wall-clock limit for each help command |
| `--max-output` | `4194304` | cumulative help bytes accepted across the tree |

These settings are recorded in managed metadata, so `update` uses the same
depth, command-count, and output policy. `--timeout` remains a global runtime
override for all subprocess-based operations.

It is deliberately conservative: only the three exact headings above and
indented `name  description` rows are recognized as subcommands. In sections
that display a whole hierarchy, only the shallowest indentation is treated as
the current command's direct children. Aliases,
multi-word command columns, missing descriptions, unrecognized headings, and
unusual layouts are omitted rather than guessed. Positionals are never assumed
to be files, and descriptions are rendered inert — help text can't inject code
into your shell. Reaching the depth limit leaves deeper commands listed but
does not inspect their options; exceeding the command-count, output-size, or
timeout limit fails generation without replacing the installed definition. If
the tool has a working native command, `generate` refuses and points you at
`install` unless you pass `--force`.

### Provenance

Each managed file keeps `#compdef` as its first line and adds one comment of
versioned, non-evaluable metadata (base64 JSON — never shell-evaluated):

```
#compdef fnm
# completionctl-metadata-v1: eyJ2ZXJzaW9uIjoxLCJUb29sIjoiZm5tIiwiU291cmNlIjoibmF0aXZlIiwi…
```

```
❱ completionctl inspect fnm
{
  "version": 1,
  "Tool": "fnm",
  "Source": "native",
  "Executable": "/opt/homebrew/bin/fnm",
  "Args": ["completions", "--shell", "zsh"],
  "output": "…/completions/_fnm"
}
```

Files without valid metadata are treated as hand-maintained: `update`,
`remove`, and overwrites refuse to touch them.

### Shadow detection

If another fpath directory already provides a completion (Homebrew's
site-functions, for example), creating a managed copy would silently take
precedence — so it's refused unless forced, and reported by `list`:

```
❱ completionctl install fnm
error: a completion for fnm already exists at /opt/homebrew/share/zsh/site-functions/_fnm; a managed copy would take precedence over it:
  completionctl install --force fnm

❱ completionctl list
fnm       native  shadows /opt/homebrew/share/zsh/site-functions/_fnm
starship  native
zsh       help
```

Search directories come from `COMPLETIONCTL_SEARCH_DIRS` (colon-separated)
when set — the shell integration exports your real fpath dirs — otherwise
from the active Homebrew prefix. Set it empty to disable the check.

## Shell integration

Completion functions load once per shell, so editing a file on disk doesn't
change a running session. `init zsh` prints a thin wrapper that forwards
every call to the real binary and, after any successful mutation, unloads the
changed functions and re-runs `compinit` — new completions work immediately,
no restart.

```zsh
# .zshrc, after compinit
if (( $+commands[completionctl] )); then
  eval "$(command completionctl init zsh)"
fi
```

By default the wrapper manages the dump at
`${XDG_CACHE_HOME:-$HOME/.cache}/zsh/zcompdump-${ZSH_VERSION}`. If your setup
uses different paths, pass them:

```zsh
eval "$(command completionctl init zsh --dump "$my_dump" --stamp "$my_stamp")"
```

Cobra's `__complete` protocol passes through untouched, so completionctl's own
tab completion works through the wrapper.

## Safety model

- `install` and `generate` **execute the target tool** with your privileges.
  Execution is explicit (never a silent fallback), stdin is closed, output is
  size-capped (cumulatively during recursive discovery), and runs are killed
  after `--timeout` (default 5s) — but this
  is not a sandbox. Don't point it at binaries you don't trust.
- Candidates must be non-empty, declare the intended `#compdef`, contain no
  control characters, and pass `zsh -n` before an atomic rename replaces the
  destination. Every failure path leaves the previous definition in place.
- Generation arguments are stored as encoded vectors, not evaluable strings.

## Environment

| Variable | Effect |
|---|---|
| `COMPLETIONCTL_SEARCH_DIRS` | Colon-separated dirs checked for shadowed definitions; empty disables. |
| `NO_COLOR`, `TERM=dumb` | Disable colored output (also auto-disabled when piped). |

## Rollback

Managed files are ordinary Zsh completion definitions. Remove the binary and
the `init zsh` line and everything keeps working from the last generated
files; the metadata line is just a comment.

## Development

```sh
make test    # go tests + zsh integration tests (needs zsh)
make build   # bin/completionctl
```

Releases are tagged `vX.Y.Z`; GitHub Actions runs GoReleaser, publishes
binaries for macOS/Linux (arm64/amd64), and updates the Homebrew tap.

## License

[MIT](LICENSE)

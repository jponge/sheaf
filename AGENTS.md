# AGENTS.md

Guidance for coding agents (and humans) working on sheaf. User documentation
is in [README.md](README.md); design decisions, history and the backlog are in
[PLAN.md](PLAN.md) (its "Takeaways" section explains where most rules below
come from).

## Project map

- `cmd/sheaf/`: `main.go` only.
- `internal/bundle/`: the `sheaf.yaml` model (load, save, validation, layout and
  name rules), bundle discovery (walk up from the working directory).
- `internal/gitx/`: thin, context-aware wrapper over the git CLI (clone, fetch,
  status, worktrees, fast-forward, deletion risks) and the per-command cache of
  read-only git queries.
- `internal/hosting/`: the `Hosting` interface and its gh CLI adapter;
  `internal/hosting/hostingtest/` holds the in-memory fake used by tests.
- `internal/ops/`: UI-agnostic use cases (create, init, add, adopt, remove,
  sync, update, fetch, prune, worktrees, doctor, browse). They report progress
  through events and never print.
- `internal/tui/`: bubbletea v2 models (overview, multi-select, prompt,
  confirm, progress view, report view) and shared styles.
- `internal/cli/`: cobra commands, TUI or plain rendering, the home screen
  (`cmd_home.go`), shell integration, completion.
- `internal/testutil/`: git fixtures and test isolation.
- `demo/`: the vhs recording of the README demo (`setup.sh`, `check.sh`,
  `sheaf.tape`).

Worktrees are never recorded in `sheaf.yaml`: `git worktree list` is the
source of truth.

## Commands

Go version: see `go.mod`. Only the Go toolchain and git are needed; gh is
optional at runtime and never needed for tests.

| Command | What it does |
|---|---|
| `make build` / `make install` | build `bin/sheaf` / install into `$GOBIN` (`VERSION=` stamps the version) |
| `make test` | `go test -race ./...` |
| `make vet` | `go vet` and staticcheck (pinned version, run with `go run`, not a module dependency) |
| `make vet-windows` | the same for `GOOS=windows` (Windows is refused at runtime but must compile) |
| `make test-symlinked-tmp` | the suite with `TMPDIR` behind a symbolic link (the macOS `/var` → `/private/var` class of bugs) |
| `make test-shells` | the cli tests with fish and zsh required (`SHELLS_ROOT=` for unpacked shells) |
| `make demo-check` | checks the demo bundle and the vhs tape without vhs; `make demo` records it (needs vhs, ttyd, ffmpeg) |

Before considering a change done: `gofmt -l internal` prints nothing, `make
vet`, `make vet-windows`, `go test -race -count=1 ./...` and `make
test-symlinked-tmp` pass.

## Testing rules

- Test first: write the test, run it and see it fail on the assertion that
  encodes the behaviour (not on a compile or setup error), then change the code
  until it passes. A test that passes before the change does not reproduce
  anything.
- Never weaken, skip or delete a failing test, change an expected value, loosen
  an assertion, or add retries to make a test pass. When a behaviour change
  requires a different expectation, say so explicitly.
- Check new tests by mutation: break the code they protect and make sure they
  fail.
- Real git only against repositories created under `t.TempDir()`, with local
  bare remotes reached through `file://`. No network, no push to anything that
  isn't a temporary bare repository.
- Never run a real `gh`: use `hostingtest`'s fake, or a stub `gh` script put
  first on `PATH` in a temporary directory; the gh adapter is tested against
  recorded fixtures (`internal/*/testdata/`).
- Packages that run git call `testutil.Main` from `TestMain` (process-wide
  isolation: no user git config, no inherited `GIT_*` variables). Use
  `t.Parallel()` unless the test touches process-global state (environment,
  `PATH` stubs, package hooks, signals, pty, cwd). Tests that write the global
  git config use `testutil.IsolateGitPerTest`. `testutil.NewOrigin` and
  `testutil.CachedTree` give cached fixtures.
- TUI models are tested by sending messages to `Update` and checking `View()`.
  End-to-end TUI tests drive the binary on a pty (`*_pty_linux_test.go`, Linux
  only) and render the output with the terminal emulator in
  `internal/cli/vt_test.go`; in-process TUI tests feed scripted input segments.
- `TestGitCallsPerCommand` holds git-call budgets per command: changing a count
  is a deliberate decision, not a test fix.
- Assertions must not depend on the length of `TMPDIR` (macOS CI uses long
  `/var/folders/...` paths, and views wrap long lines) or on the resolution of
  symbolic links in temporary paths.

## Data safety

Most defects found in this project were data-loss paths in destructive
commands (remove, sync `--remove-gone`, prune, `wt rm`, adopt). When touching
them:

- Refuse rather than guess. Absence from a remote listing is not proof of
  deletion; a remote that can't be fetched makes its state unknown, which is a
  risk, not a pass.
- Plan, confirm, then re-check under the bundle lock with fresh git reads (not
  the query cache) and compare with what was confirmed before deleting
  anything: worktrees can be moved, locked, switched or replaced while a dialog
  is open.
- Never act on another repository's worktrees, on worktrees outside their
  repository directory, or on a directory that lost its `.git` (git discovery
  would walk up into an enclosing repository).
- Sanitize child environments (`GIT_*`, locale, user git config) and kill
  process groups on cancellation.
- An error that combines an interruption with a failed cleanup or rollback is
  a failure (`ops.CleanupError`), never just "interrupted".

## Output and UI conventions

- Plain output and `--json` output are interfaces: list every user-visible text
  change, keep `--json` schemas stable.
- Hints that tell the user what to run print the complete command
  (`ops.CommandLine`, shell-quoted, `--bundle` when it was given), and the
  command must do exactly what the message says.
- Colors and terminal control sequences (window title, progress indicator,
  clipboard, mouse) only on a terminal that supports them: respect `NO_COLOR`,
  `CLICOLOR=0`, `TERM=dumb`, `--no-tui` and `--json`.
- The home screen runs its actions as in-process commands; their output goes
  live to the normal screen and into the report view.
- List views share the layout rules in `internal/tui` (blank-line gaps that
  give way only when the list would otherwise scroll, compact key help with
  `?`, status parts shown whole or dropped, `…` on truncation).
- Ask before adding a module dependency.

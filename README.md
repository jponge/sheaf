# sheaf

sheaf manages a group of git repositories checked out side by side in one
directory, the *bundle*. Each repository gets a fixed layout: a primary
checkout in a directory named after the default branch, and linked worktrees
(`git worktree`) as its siblings, one directory per branch, tag or pull
request. sheaf clones repositories from GitHub (one by one or by
organization), adopts existing clones, and runs bundle-wide operations
(status, fetch, fast-forward update, running a command everywhere, pruning
merged worktrees, syncing with organizations). It runs as an interactive TUI
on a terminal and as a plain CLI otherwise.

Worktrees are not recorded in `sheaf.yaml`: git (`git worktree list`) is the
source of truth, so worktrees created or removed with plain git are picked up.

## Contents

- [Install](#install)
- [Concepts](#concepts)
- [Quick start](#quick-start)
- [The home screen](#the-home-screen)
- [Command reference](#command-reference)
- [Shell integration](#shell-integration)
- [Safety model](#safety-model)
- [TUI and plain mode](#tui-and-plain-mode)
- [JSON output](#json-output)
- [Platform support](#platform-support)
- [Development](#development)
- [License](#license)

## Install

Requirements:

- Go 1.27 or later to build (`go.mod`).
- git 2.36 or later (`sheaf doctor` checks this; other commands do not).
  Tested with git 2.47 and 2.56.
- [`gh`](https://cli.github.com/), authenticated with `gh auth login`, for
  everything that talks to GitHub: `add` (it resolves `owner/repo` through
  gh, including for single repositories), `add --org`, fork detection, `sync`
  and `wt pr`. Commands working on local state (`adopt`, `init`, `status`,
  `fetch`, `update`, `run`, `wt add/list/rm`, `path`, `prune`, `remove`) do
  not need gh.

With Go installed:

```sh
go install github.com/jponge/sheaf/cmd/sheaf@latest    # or @v0.1.0 for a given release
```

This installs into `$(go env GOBIN)`, or `$(go env GOPATH)/bin` by default,
and `sheaf version` prints the release.

From a clone of this repository:

```sh
make build VERSION=v0.1.0      # writes bin/sheaf, version stamped
make install VERSION=v0.1.0    # installs into $(go env GOBIN) or $(go env GOPATH)/bin
go install ./cmd/sheaf         # same, without stamping
```

`sheaf version` prints the version given through `VERSION=` (that is,
`-ldflags "-X github.com/jponge/sheaf/internal/cli.Version=..."`). Without
one, it prints the version Go recorded in the binary: the module version for
`go install <module>/cmd/sheaf@<version>`, a pseudo-version (commit, `+dirty`
for uncommitted changes) for a build in a git checkout, and otherwise `dev`,
followed by the commit when known.

Then set up the [shell integration](#shell-integration) and run
`sheaf doctor` inside a bundle to check git, gh and the bundle.

## Concepts

### Bundle and `sheaf.yaml`

A bundle is a directory containing a `sheaf.yaml` file. Commands find the
bundle by walking up from the working directory (symbolic links resolved) to
the first directory containing a regular file named `sheaf.yaml`;
`--bundle <dir>` overrides the search.

`sheaf.yaml` (schema version 1) is written by sheaf:

```yaml
version: 1
sources:                       # organizations tracked by `sheaf sync`
  - org: smallrye
    exclude: [legacy-thing]    # declined with `sync --exclude`; no longer proposed
repositories:
  - name: boiler-vroom         # directory name under the bundle
    slug: jponge/boiler-vroom  # the reference repository (owner/name)
    url: git@github.com:jponge/boiler-vroom.git
    default_branch: main
    origin: explicit           # explicit | org:<name> | adopted
    host: gitlab.com           # optional; omitted means github.com, "local" for file:// remotes
    fork:                      # optional; present when origin is your fork
      slug: you/boiler-vroom
      url: git@github.com:you/boiler-vroom.git
```

GitHub features (`sync`, `wt pr`, organization listings) skip repositories
whose host is not `github.com`. For SSH configuration aliases (e.g.
`git@github-work:...`) the alias is recorded as the host unless
`adopt --host github.com` is given.

`.sheaf.lock` next to `sheaf.yaml` is the lock file (flock(2)) serializing
bundle updates between concurrent sheaf processes. It stays on disk by
design; a second process waits for the first.

### Repository layout

```
~/work/                      bundle
├── sheaf.yaml
├── .sheaf.lock
└── boiler-vroom/            repository directory
    ├── main/                primary checkout (named after the default branch)
    ├── feat-cli-rework/     worktree of branch feat/cli/rework
    ├── v1.2.0/              worktree pinned at tag v1.2.0 (detached HEAD)
    └── pr-123/              pull request from a fork (branch pr/123)
```

- **Repository directory** `<bundle>/<repo>/`. Its name is the repository
  name (letters, digits, `.`, `_`, `-`; not starting with `.`). Two
  repositories with the same name from different owners clash: use
  `add --as <dir>`.
- **Primary checkout** `<bundle>/<repo>/<default-branch>/`. This is a
  regular clone (with its own `.git` directory), and the *directory* is what
  makes it primary: it may have any branch checked out, temporary switches
  are fine. `wt add <default-branch>` is refused, with a hint to switch the
  primary back when it is on another branch.
- **Worktrees** `<bundle>/<repo>/<normalized-branch>/`, created with
  `git worktree add` from the primary. Branch names are normalized to
  directory names: every character outside `[A-Za-z0-9._-]` (including `/`)
  becomes `-`, runs of `-` collapse, leading and trailing `-` and `.` are
  trimmed (`feat/cli/rework` → `feat-cli-rework`). Two branches mapping to
  the same directory (compared case-insensitively) are refused.
- **Tag worktrees** (`wt add --tag v1.2.0`) are detached at the tag's
  commit, in the same directory namespace. `update` never moves them,
  `prune` never removes them.
- **Pull request worktrees** (`wt pr <n>`): a pull request from a branch of
  the repository is checked out like that branch; one from a fork (or whose
  head branch was deleted) goes to `<repo>/pr-<n>/` on the local branch
  `pr/<n>`, tracking `refs/pull/<n>/head`.

### Forks

When you have a fork of a repository you add, sheaf clones it with
`origin` = your fork and `upstream` = the reference repository; the default
branch tracks `upstream/<default>`, new branches start from
`upstream/<default>`, and `sheaf.yaml` keeps the reference slug plus a
`fork` entry. Forks are detected on `add` with one gh listing of your forks
(`--no-fork` disables it) and on `adopt` when a clone has an `upstream`
remote naming another repository whose default branch shares history with
origin's. `adopt --recheck <repo>` records a fork that became visible after
the repository was registered (e.g. after `git fetch upstream`). sheaf never
creates forks. Fetch, update, merged/gone and unpushed checks consider both
remotes.

## Quick start

```sh
sheaf create ~/work && cd ~/work         # new, empty bundle
sheaf add jponge/boiler-vroom            # clone into boiler-vroom/main/
sheaf add --org smallrye                 # TUI: pick repositories from a list
sheaf add --org smallrye --repo smallrye-config --repo smallrye-common  # plain mode
sheaf add --org smallrye --all           # every non-archived repository
```

An existing directory of clones:

```sh
cd ~/src
sheaf init                 # writes sheaf.yaml, lists the clones found (TUI: pick them)
sheaf adopt foo bar        # foo/ (a plain clone) is moved to foo/<default-branch>/
sheaf init --adopt-all     # or adopt everything found in one go
```

`adopt` never checks out or switches anything; linked worktrees of a moved
clone are repaired.

Daily use:

```sh
cd ~/work/boiler-vroom/main
cd "$(sheaf wt add feat/cli/rework)"   # creates ../feat-cli-rework/, prints its path
sheaf status                           # every checkout of the bundle, no network
sheaf update                           # fetch once per repository, then fast-forward
sheaf run --dirty-only git diff --stat
scd boiler-vroom feat-cli-rework       # cd via the shell integration
sheaf wt rm -d feat/cli/rework --yes   # remove the worktree and its merged branch
sheaf prune                            # list worktrees whose branch is gone or merged
```

## The home screen

`sheaf` without a command, inside a bundle, with standard input and output
on a terminal, opens an overview of every checkout of the bundle (columns
REPO, DIR, BRANCH, STATE). It reads local state only; nothing is fetched
when it opens. Checkouts that cannot be inspected are shown as problem rows.

| Key | Action |
|---|---|
| up/down, k/j, pgup/pgdown, home/end | move |
| space | toggle the selection of the current row |
| a, n | select / deselect every visible row (rows hidden by the filter keep their selection) |
| / | filter (case-insensitive substring over the cells; enter keeps it, esc clears it) |
| enter | change directory to the current checkout and exit (see below) |
| u | update the selected rows (or the current row) |
| f | fetch the repositories of the selected rows (or the current row) |
| w | add a worktree to the current row's repository: a prompt asks for a branch (an existing local or remote one, else a new branch from the default branch, as `wt add`), tab switches to a tag |
| r | remove the selected worktrees (or the current one); not the primary |
| p, d, s | `prune`, `doctor`, `sync` on the whole bundle |
| q, esc | quit with status 0 (esc first clears an applied filter) |
| ctrl+c | exit with status 130 |

Actions run with the TUI of the corresponding command (progress view,
confirmation dialogs, menus) and return to the overview, which shows the
outcome (e.g. `prune: nothing to prune` when nothing changed). An action
that fails prints its error and is reported in the overview; quitting
afterwards still exits with 0. The selection is kept, except after an
update, fetch or removal of the selected rows that succeeded.

Enter writes the checkout's path to the file named by `SHEAF_CD_FILE`, which
the `sheaf` shell function defined by `sheaf shell-init` creates and then
`cd`s into. Without the shell function the path is printed on standard
output. sheaf removes `SHEAF_CD_FILE` from its environment at startup, so
the processes it starts (git, hooks, `sheaf run` commands) never see it.

Outside a bundle, bare `sheaf` prints the help and a hint to run
`sheaf create` or `sheaf init`. With `--no-tui` or without a terminal it
prints the help.

## Command reference

Global flags:

- `--bundle <dir>`: bundle directory (default: nearest parent containing
  `sheaf.yaml`).
- `--no-tui`: plain CLI mode.
- `-j, --jobs <n>`: repositories processed in parallel (checkouts for
  `status` and `run`); default: the number of CPUs.

Each entry lists the main flags only; `sheaf <command> --help` documents
every flag and the exact semantics.

Arguments naming a repository are exact, case-sensitive repository names
(directory names); a name that only differs by case gets a "did you mean"
hint. `--repo` filters of `status`, `fetch`, `update`, `run` and `prune`
also accept glob patterns (`--repo 'smallrye-*'`, repeatable).

### Bundle

**`sheaf create [dir]`**: create a new, empty bundle (default: the current
directory).

**`sheaf init [dir]`**: create `sheaf.yaml` in an existing directory and list
the checkouts found in its subdirectories: plain clones, and repositories
with a single checkout `<dir>/<child>/`. `--adopt-all` adopts them; in TUI
mode a menu lets you pick, and the checkouts left out are listed with the
`sheaf adopt` command to adopt them later.

**`sheaf doctor`**: check git (2.36 or later), gh installation and
authentication for github.com, `sheaf.yaml`, and for each repository its
directory, primary checkout, remote URLs (and fork setup), missing or
outside worktrees, and lock files left in its git directory. Directories of
the bundle that are not registered are listed. Nothing is fetched or
changed. One line per check (OK, INFO, WARN, FAIL); exit status 1 when a
check fails. `--json`.

**`sheaf version`**: print the version.

### Repositories

**`sheaf add <owner/repo>...`** / **`sheaf add --org <org> (--all | --repo <name>...)`**:
clone into `<bundle>/<repo>/<default-branch>/` and record in `sheaf.yaml`.
Repositories already in the bundle are skipped. With `--org`, the
organization (or user) becomes a tracked source for `sync` (sheaf says so
when it starts tracking it, even when every chosen repository was already
present); archived repositories need `--include-archived`; without
`--all`/`--repo`, TUI mode shows a selection menu (repositories already in
the bundle are shown, not selectable, and counted apart) and plain mode
lists the candidates and fails.
`--as <dir>` names the directory (single repository). `--no-fork` disables
fork detection.

**`sheaf adopt <dir>...`**: register existing checkouts located directly in
the bundle. A plain clone `<dir>/` is moved to `<dir>/<default-branch>/`; a
single checkout `<dir>/<child>/` is renamed to it if needed. Owner, name and
host come from the `origin` URL. `--default-branch` when it cannot be
determined, `--host` to record a host instead of an SSH alias, `--recheck`
to re-examine the remotes of already registered repositories and record a
fork (nothing is moved; a recorded fork is never dropped or changed).

**`sheaf remove <repo>`**: fetch, check every checkout and worktree for data
that would be lost, then delete the worktrees, the repository directory and
its `sheaf.yaml` entry. Requires `--yes` in plain mode; TUI mode shows the
plan in a confirmation dialog (skipped with `--yes`). `--force`,
`--offline`. See [Safety model](#safety-model).

**`sheaf sync`**: list the repositories of each tracked organization
(`--org` restricts) and compare with the bundle: `new`, `gone`, `moved`
(informational) and `archived` (informational). Only github.com repositories
are compared. Without action flags nothing changes. Actions, applied in this
order: `--exclude [org/]name` records declined repositories in `sheaf.yaml`,
`--add-new` clones the remaining new ones, `--remove-gone --yes` removes the
gone ones with the checks of `sheaf remove` (`--force`,
`--allow-mass-removal`). In TUI mode without action flags, a menu offers the
new and gone repositories (nothing preselected; gone ones refused as a mass
removal are shown but not selectable), then asks to confirm removals; the
TUI never applies `--force` or `--allow-mass-removal`. New
repositories left unselected are not excluded: the next sync proposes them
again (decline them for good with `--exclude`). `--json` prints the
proposals without changing anything. Exit status 1 when gone candidates could not be looked up.

### Checkouts and worktrees

`worktree` has the alias `wt`. For `wt add`, `wt rm` and `path`: with two
arguments the first is the repository; with one argument, inside a checkout
of the bundle it names a branch or directory of that checkout's repository,
outside any checkout it is a repository name.

**`sheaf status`**: branch, upstream, ahead/behind, staged, unstaged,
untracked, conflicted counts and stashes of every checkout, primary first.
No network access: ahead/behind are relative to the last fetch, unless
`--fetch`. `--repo`, `--primary-only`, `--dirty-only`, `--json`. Checkouts
that cannot be inspected are listed as problems after the table and make
the exit status 1.

**`sheaf fetch`**: `git fetch --prune origin` once per repository (for a fork:
`upstream`, then `origin`), repositories in parallel. `--repo`.

**`sheaf update`**: fetch once per repository, then fast-forward each checkout
(primary first) to its upstream with `git merge --ff-only`. Only checked-out
branches move. A checkout is skipped, with the reason, when it has staged or
unstaged changes (untracked files do not count), is detached, has no
upstream or a deleted one, or has diverged. Tag worktrees are reported as
pinned. Exit status 1 when a checkout failed or could not be inspected;
`--strict` also counts skips. `--repo`, `--primary-only`. Interruption: see
[Ctrl-C](#interruption-ctrl-c).

**`sheaf run [flags] [--] <command> [args...]`**: run a command in each
checkout directory, several at a time (`-j`). The output of each is printed
as one block headed `==> repo/dir <==`, in checkout order; with `-j 1` it is
streamed live. The command runs without a shell unless `--shell` (the first
argument is then a script for `sh -c`, the others its `$1`, `$2`, ...).
Flags after the command name belong to the command. The command gets no
standard input and no controlling terminal; `SHEAF_REPO`, `SHEAF_DIR` and
`SHEAF_BRANCH` describe the checkout. Exit status 1 if any command failed.
`--repo`, `--primary-only`, `--dirty-only`.

**`sheaf wt add [<repo>] <branch>`**: create `<repo>/<normalized-branch>/` for
the local branch if it exists, else a local branch tracking
`origin/<branch>`, else a new branch from `--base` (default
`origin/<default>`, `upstream/<default>` for a fork). The remotes are fetched
first unless `--no-fetch` (alias `--offline`). A name that is only a tag is
refused: use `--tag`. The path is printed on standard output, progress on
standard error:

```sh
cd "$(sheaf wt add feat/x)"
```

**`sheaf wt add [<repo>] --tag <tag>`**: detached worktree at the tag's
commit, pinned to the tag. Local tags are never deleted or overwritten.

**`sheaf wt list [<repo>]`** (alias `ls`): every checkout with its directory,
branch, flags (primary, off-default, detached, tag, locked, prunable,
outside) and path. `--porcelain` prints paths only (`-z`: NUL-terminated),
`--json`. Unusable worktrees (another repository's, or without `.git`) are
reported and make the exit status 1.

**`sheaf wt rm [<repo>] [<branch-or-dir>]`** (alias `remove`): remove a linked
worktree, named by branch, tag or directory (`--branch`/`--dir` when a name
is both). `-d` also deletes the local branch if merged, `-D` even if
unmerged; the plan says whether the branch is deleted or kept. Requires
`--yes` in plain mode; TUI mode asks in a dialog.
`--force`, `--offline`. The primary checkout cannot be removed (use
`sheaf remove`).

**`sheaf wt pr [<repo>] <number>`**: check out a GitHub pull request (`#123`
also accepted) in a worktree and print its path. Running it again for a
pull request from a fork fast-forwards its worktree to the current head,
refusing uncommitted changes and force-pushed heads. `sheaf update` skips
these worktrees; `git pull` works in them.

**`sheaf path [<repo>] [<branch-or-dir>]`**: print the bundle root, a
repository's primary checkout, or a worktree (also by tag). Inside a
checkout, one argument names a worktree of that repository if there is one,
else a repository; `--repo`, `--dir`, `--branch` disambiguate.

### Maintenance

**`sheaf prune`**: fetch (unless `--offline`), then find the linked worktrees
whose branch is gone (upstream deleted on the remote) or merged into
`origin/<default>` (`upstream/<default>` for a fork) with a merge commit.
Squash- and rebase-merged branches are found only once their remote branch
is deleted. Without `--yes` it only lists the candidates (dry run); with
`--yes` each is removed like `sheaf wt rm`. `--delete-branches` also deletes
the branches of removed worktrees when merged (like `-d`, never `-D`);
`--include-branches` also deletes gone or merged local branches without a
worktree. `--repo`, `--json`. In TUI mode the candidates are shown in a
confirmation dialog, which states what will be removed and whether each
worktree's branch is kept or, with `--delete-branches`, deleted (a branch
is deleted only if merged, as `git branch -d`).

### Shell

**`sheaf shell-init bash|zsh|fish`**: print the `sheaf` and `scd` shell
functions. **`sheaf completion bash|zsh|fish`**: print the completion script.
See [Shell integration](#shell-integration).

## Shell integration

### `sheaf` and `scd` functions

```sh
# ~/.bashrc or ~/.zshrc
eval "$(sheaf shell-init bash)"    # or zsh
```

```fish
# ~/.config/fish/config.fish
sheaf shell-init fish | source
```

This defines:

- `scd [<args>]`: `cd` to the output of `sheaf path <args>`. `scd` alone goes
  to the bundle root, `scd <repo>` to the primary checkout,
  `scd <repo> <branch-or-dir>` to a worktree. When `sheaf path` fails, its
  status is returned and the directory is unchanged. `scd` completes like
  `sheaf path` and does not need `sheaf completion`.
- `sheaf`: a wrapper that runs the sheaf binary and, after enter on a
  checkout in the [home screen](#the-home-screen), changes the directory to
  it. Every other use behaves as without the function (same arguments,
  streams and exit status).

The fish functions need fish 3.1 or later. zsh completion of `scd` needs
`compinit` to have run before.

Known limits of bash `scd` completion: completing inside an open single
quote a candidate that itself contains a single quote, words quoted as
`$'…'`, and `!` inside double quotes (history expansion) are not handled.
Complete such names without quotes, or type them in full.

### Completion

```sh
source <(sheaf completion bash)       # needs the bash-completion package
source <(sheaf completion zsh)        # after compinit
sheaf completion fish | source
```

`sheaf completion <shell> --help` shows how to install the script
permanently. Besides commands and flags, completion offers repository
names, worktree branches and directories, branches for `wt add`, and
organizations, read from the bundle (`--bundle` on the command line is
honoured). It never uses the network.

## Safety model

sheaf deletes data in `remove`, `wt rm`, `prune` and `sync --remove-gone`.
These commands share one set of rules.

**Confirmation.** In plain mode, nothing is deleted without `--yes`
(`remove`, `wt rm`, `sync --remove-gone`); `prune` without `--yes` is a dry
run. sheaf never prompts in plain mode: missing input is an error. In TUI
mode, a dialog shows what will be deleted and asks for confirmation unless
`--yes` is given.

**Fetch first.** `remove`, `wt rm` and `prune` fetch before deciding, so
that commits whose remote branch was deleted count as unpushed. `--offline`
skips the fetch and decides on the remote-tracking refs of the last fetch
(possibly stale). For a fork, when your fork is gone or with `--offline`,
its remote-tracking refs are not trusted (not pushed, not a merge target).
`sync` does not accept `--offline`.

**Data at risk** (refused unless `--force`):

- `remove`: uncommitted changes, untracked files, stashes, unpushed commits
  in any checkout or worktree; entries of the repository directory that
  belong to no checkout (OS files such as `.DS_Store` excepted); nested
  repositories (also inside ignored directories) that are dirty, have
  stashes, unpushed commits or no remote; a remote repository that no
  longer exists. Commits reachable from a pull request head recorded by
  `wt pr` count as pushed (with `--offline`, as of its last fetch).
- `wt rm`: uncommitted changes, untracked files, commits only on a detached
  HEAD in that worktree.
- `sync --remove-gone`: as `remove`; in addition, a gone repository whose
  remote still answers (a likely gh token scope issue) is refused unless
  `--force`. When more than half of an organization's tracked repositories
  (more than one) look gone, removal also needs `--allow-mass-removal`.

**Never overridden by `--force`:**

- the working directory is inside the repository or worktree to delete;
- locked worktrees (`git worktree unlock` first);
- worktrees outside the repository directory, and worktrees of another
  repository inside it (`remove`);
- a missing primary checkout or a repository directory that is a symbolic
  link (`remove`); a worktree whose directory is missing (`wt rm`: run
  `git worktree prune`);
- a failed fetch other than a gone remote;
- the primary checkout in `wt rm` (use `sheaf remove`).

`remove` only deletes inside `<bundle>/<repo>`, a real directory directly
under the bundle root; symbolic links inside it are removed, not followed.
It checks the plan again under `.sheaf.lock` after confirmation.

**Branch deletion** mirrors `git branch -d/-D`. `wt rm -d` deletes the
branch only if it is merged into its own upstream or into `origin/<default>`
(`upstream/<default>` for a fork), or contained in another ref; `-D`
deletes it even if unmerged (its commits are lost). `--force` only concerns
the worktree's own data, never the branch. Tags of tag worktrees are always
kept.

**`prune`** has no `--force`: a worktree with uncommitted changes or
untracked files is kept (ignored files do not block it). It never touches
the primary checkout, worktrees outside the repository directory, locked
worktrees, tag worktrees, or `wt pr` branches. A branch without commits of
its own is never considered merged; a branch being rebased or bisected
counts as in use.

## TUI and plain mode

A command runs its TUI when both standard input and standard output are
terminals and `--no-tui` is not given. Otherwise it runs in plain mode:
every TUI choice has a flag equivalent, progress goes to standard error,
results to standard output. Commands with TUI variants: bare `sheaf`,
`init`, `add`, `adopt`, `remove`, `fetch`, `update`, `wt rm`, `prune`,
`sync`. The others are always plain (`status`, `path`, `wt list`, `wt add`,
`wt pr`, `run`, `doctor`, `create`, `version`, `completion`, `shell-init`).

Menus (`add --org`, `init`, `sync`): space toggles, `a` selects all
visible, `n` none, `/` filters, enter confirms, esc or q cancels.

**Colour.** Plain output uses colour only on a terminal and not when
`NO_COLOR` is set to a non-empty value. The TUI drops colours when
`NO_COLOR` is non-empty or `CLICOLOR=0` (without `CLICOLOR_FORCE`); bold
and faint are kept.

**Terminal access for git.** git normally runs in its own session without
the terminal, so it cannot prompt: use an SSH agent or a credential helper.
In plain mode with standard input on a terminal, git keeps the terminal
(so ssh can ask for a passphrase or a host key confirmation) for the
single-repository commands `remove`, `wt add`, `wt rm` and `wt pr`, and for
the other commands with `-j 1`. In TUI mode git never gets the terminal.

### Interruption (Ctrl-C)

- The first Ctrl-C (or SIGINT, SIGTERM, SIGHUP) cancels the command: git
  processes are stopped, sheaf prints `interrupted` and exits with 130.
  Ctrl-C in a TUI menu or dialog also exits with 130.
- `update` lets a running fast-forward merge finish after the first Ctrl-C
  (interrupting it would leave a half-updated work tree) and starts no new
  one.
- A second Ctrl-C is an emergency stop: the process groups of running git
  commands and `run` commands are killed and sheaf exits with 130 at once,
  without cleanup. A partial clone directory (`add`, `sync`), an
  `.sheaf-adopt-*` folder from an interrupted adopt, or a `.git/index.lock`
  and half-updated work tree from a killed fast-forward (`update`) may
  remain; the message says what to check for the operations that were
  running.
- `run`: Ctrl-C sends SIGTERM to the running commands and their children,
  then SIGKILL after 5 seconds.

When the reader of standard output goes away (e.g. `sheaf run ... | head`),
sheaf stops quietly with exit status 141.

## JSON output

These commands accept `--json`; each document has a `"version": 1` field.

| Command | Content |
|---|---|
| `status --json` | repositories, their checkouts and problems (schema in `sheaf status --help`) |
| `wt list --json` | `{"version": 1, "worktrees": [...], "errors": [...]}` |
| `prune --json` | `{"version": 1, "dry_run", "candidates", "skipped", "results", "errors"}`; always plain mode, removes only with `--yes` |
| `doctor --json` | `{"version": 1, "failed": bool, "checks": [{level, check, repo, message, hint}]}` |
| `sync --json` | organizations and proposals; never changes anything (keys in `sheaf sync --help`) |

`status --help` states that new fields may be added within version 1.
`wt list --porcelain [-z]` prints one path per line for scripts.

## Platform support

- Linux and macOS: tested.
- Windows: not supported. The code builds, but `sheaf` refuses to run there (exit 1): it relies on Unix process groups, signals and file locks.

## Development

```sh
make build                           # bin/sheaf (VERSION=... to stamp)
make test                            # go test -race ./...
make vet                             # go vet ./...
make test-symlinked-tmp              # suite with TMPDIR behind a symbolic link (macOS /var case)
make test-shells SHELLS_ROOT=DIR     # cli tests with fish and zsh required
```

`make test` skips the fish and zsh tests when those shells are missing.
`make test-shells` requires them: from `PATH`, or unpacked without root
under `SHELLS_ROOT` (e.g. `dpkg-deb -x fish_*.deb DIR`, same for zsh, giving
`DIR/usr/bin/fish` and `DIR/usr/bin/zsh`).

Tests use real git only against temporary local repositories (bare
"origin" repositories reached through `file://` URLs) with an isolated git
configuration; they never use the network, never push anywhere else, and
never run `gh` (the hosting layer is tested with a fake and recorded JSON
fixtures).

## License

Copyright 2026 The sheaf Authors.

Licensed under the Apache License, Version 2.0; see [LICENSE](LICENSE).

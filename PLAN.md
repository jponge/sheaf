# sheaf — implementation plan

Status: approved 2026-10-14. Source of requirements: `goals.md`.

## Decisions taken

| Topic | Decision |
|---|---|
| Dependencies | `spf13/cobra`, `charmbracelet/bubbletea`, `bubbles`, `lipgloss`, `go.yaml.in/yaml/v3` (maintained successor of gopkg.in/yaml.v3, approved 2026-10-14). Tests: stdlib `testing` only. Nothing else without asking. |
| Module / Go | `github.com/jponge/sheaf`, Go 1.27 |
| Repo directory | `<bundle>/<repo>/`; a name clash across owners is an error unless `--as <dir>` is given |
| Primary checkout | `<bundle>/<repo>/<default-branch-normalized>/` |
| Worktrees | `<bundle>/<repo>/<branch-normalized>/` as siblings of the primary; created with `git worktree add` from the primary |
| Normalization | `/` and other path-unsafe characters → `-`, consecutive `-` collapsed; collisions (e.g. `feat/a-b` vs `feat-a/b`) are detected and refused |
| update | `git pull --ff-only`; skip and report when the tree is dirty (staged or unstaged), there is no upstream, or the branch has diverged |
| remove | refuses when any checkout/worktree has uncommitted changes, stashes, or unpushed commits; `--force` overrides |
| UI mode | TUI when stdin+stdout are TTYs; plain CLI otherwise or with `--no-tui`. In CLI mode every TUI choice has a flag equivalent; missing required input is an error, never a prompt |
| Bundle discovery | walk up from CWD to the first directory containing `sheaf.yaml`; `--bundle <dir>` overrides |
| adopt | a plain clone `foo/` is moved to `foo/<branch-normalized>/`; existing linked worktrees fixed with `git worktree repair`; refuse if the target exists |
| Org listing | archived repos hidden unless `--include-archived`; forks listed and marked |
| Commits | orchestrator commits locally after each reviewed step (conventional commits, no attribution trailers); never pushes |

## Architecture

```
cmd/sheaf/            main.go only
internal/bundle/      sheaf.yaml model, Load/Save, Find (walk up), layout & name normalization
internal/gitx/        thin exec wrapper over git: clone, default branch, status (porcelain v2), ahead/behind,
                      stash count, worktree add/list/remove/repair, fetch, pull --ff-only
internal/hosting/     interface Hosting { ListRepos(owner), Repo(owner, name) }; gh CLI impl (gh repo list/view --json); in-memory fake
internal/ops/         use cases, UI-agnostic; emit progress events (repo, worktree, phase, message, error) on a channel;
                      bounded-parallel executor (-j N, default NumCPU)
internal/tui/         bubbletea models: multi-select (filter, select all/none), progress view over ops events, confirm dialog
internal/cli/         cobra commands; chooses TUI or plain renderer
internal/testutil/    fixtures: local bare "origin" repos with commits/branches in t.TempDir(), file:// clone URLs,
                      isolated HOME / GIT_CONFIG_GLOBAL so tests never read the user's git config
```

The source of truth for worktrees is git (`git worktree list --porcelain`), not `sheaf.yaml`, which avoids the two drifting apart.

### `sheaf.yaml` (v1)

```yaml
version: 1
sources:                     # tracked by `sync`
  - org: smallrye
    exclude: [legacy-thing]  # deliberately declined; sync stops proposing them
repositories:
  - name: boiler-vroom       # directory name
    slug: jponge/boiler-vroom
    url: git@github.com:jponge/boiler-vroom.git
    default_branch: main
    origin: explicit         # explicit | org:<name> | adopted
```

## Testing rules (non-negotiable, applies to every step)

- Red → green per behaviour; the implementer reports the red and green runs with their output.
- Real `git` only against ephemeral repos under `t.TempDir()`; the remotes are local bare repos reached through `file://`. No network, no push to anything that isn't a temp bare repo.
- `gh` is never invoked in tests: `hosting` is covered by a fake, and the gh adapter by parsing recorded JSON fixtures (plus a stub `gh` script placed on `PATH` in a temp dir).
- TUI models are tested by feeding `tea.Msg`s to `Update` and asserting on state and `View()` output (no extra dependency).
- `go vet ./...` and `go test -race ./...` must be green at the end of every step.

## Steps

Model per step: **O** = Opus, **S** = Sonnet. Every step goes through implement (TDD) → adversarial review (Opus, separate agent) → fixes → re-review if there were blocking findings → orchestrator verifies tests itself → commit.

| # | Step | Model |
|---|---|---|
| 0 | Scaffold: go.mod, cobra root, `--bundle`/`--no-tui`/`-j` global flags, `version`, testutil fixtures (bare origin, isolated git env), Makefile with `test`/`vet`/`build` | S |
| 1 | `bundle`: yaml model + validation, Load/Save (atomic write), Find upward, normalization + collision detection, layout path helpers | S |
| 2 | `gitx`: runner, clone, default-branch detection, status parsing (branch, upstream, ahead/behind, staged/unstaged/untracked, stashes), worktree list/add/remove, fetch, pull --ff-only with classified outcomes | O |
| 3 | `hosting`: interface, gh adapter (`gh repo list <owner> --json …`, `gh repo view`), archived/fork flags, fake | S |
| 4 | `ops` + CLI: `create`, `init`, `add owner/repo`, `add --org <org> [--all \| --repo …]`, `adopt <dir>`, `remove`; executor + events; plain renderer | O |
| 5 | `ops` + CLI: interactive git mode (git keeps the terminal, no Setsid, when CLI mode && -j 1 && stdin is a TTY; lets ssh ask for host keys/passphrases), `status`, `update`, `run -- <cmd>` (with `--repo`, `--dirty-only` filters), `fetch` | O |
| 6 | `worktree add/list/remove` (`wt` alias), `path <repo> [branch]` | O |
| 7 | `sync`: diff org listings against the bundle (new / gone / archived), honour `exclude`, apply chosen changes | O |
| 8 | TUI: multi-select (org add, sync), progress view for long operations, confirm for destructive ones; TTY detection wiring | O |
| 9 | Completion: cobra bash/zsh/fish plus dynamic completion of repo and branch/worktree names; `sheaf shell-init <shell>` emitting an `scd` function on top of `path` | S |
| 10 | Extras: `prune` (merged/gone branches), `wt pr <n>` (via gh), `doctor` | O |

## Orchestration log

The orchestrator appends one line per completed step: commit, review rounds, notable findings.

- Step 0 — scaffold. Sonnet impl, Opus review: 1 blocking (inherited GIT_* env leaked into fixtures, proven by writing into a victim repo), fixed with regression test; -j validation, locale pinning, AdvanceOrigin added. Note for step 2: gitx must also strip GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE etc. from child env.
- Step 1 — bundle. Sonnet impl, Opus review: 0 blocking, 6 should-fix fixed (case-insensitive collisions, trailing YAML docs rejected, default_branch/url required, strict repo-name charset + reserved sheaf.yaml, slug-before-name duplicate check, O_EXCL Create). Switched to go.yaml.in/yaml/v3. Open: RemoveSource still case-sensitive (revisit in step 7).
- Decision 2026-10-14: no gh authentication in the sandbox; real gh JSON shapes and GraphQL not-found messages remain unverified (hand-written fixtures).
- Step 3 — hosting. Sonnet impl, Opus review x2: round 1 blocking (git_protocol read without -h host → SSH users got HTTPS URLs) + 6 should-fix (cancel caching, ErrNotFound mapping, flag injection via owner, ctx deadline, truncation off-by-one, Fake/adapter divergence); round 2 blocking regression (validation rejected smallrye/.github), fixed with user-approved test expectation change. Follow-up: real gh output (user-recorded) added as fixtures; unknown-owner stderr mapped to ErrNotFound.
- Env 2026-10-14: sandbox hit pids.max (2048) from unreaped zombie git processes (PID 1 = claude, no init). Added podman --init; testutil disables maintenance.auto/gc.auto. Step 8a (tui) agent was killed mid-work: internal/tui/ is partial, restart it. Step 2 (gitx) reviewed: B1 cancel kills only top process (use Setsid+SIGTERM to group), B2 parallel fetch ref-lock races (fetch once per repo, then merge --ff-only per worktree), S1 ssh tty, S2 status flags, S3 remove-safety check, S4 min git 2.36, S5 WorktreeAdd leftover branch — fixes pending.
- Decisions 2026-10-14 (user ok): two-step esc in filtered MultiSelect; EOF on non-TTY input = abort; x/term + termenv direct imports covered by charm approval; git runs in its own session (no tty prompts; ssh-agent works; possible future opt-in for CLI -j1). CLI must translate SIGINT into context cancellation.
- Decision 2026-10-14 (user): add interactive git mode for CLI with -j 1 (scheduled in step 5; gitx gets a per-call option to keep the controlling terminal).
- Step 8a — tui widgets (MultiSelect, Confirm, Run). Opus impl (restarted after PID exhaustion), Opus review: 0 blocking; fixed non-TTY *os.File stdin swallowing (now ErrInputNotTerminal), NO_COLOR disabled-row marker, control-char sanitization (GitHub descriptions), per-rune key handling, tiny heights. Progress view pending (step 8b, after ops events exist).
- Step 2 — gitx. Opus impl, Opus review x2. Round 1: 2 blocking (cancel left child processes / wrong PullResult; parallel fetch ref-lock race) + 5 should-fix, all fixed test-first (Setsid + group SIGTERM, Fetch-once + FastForward, status flags, DeletionRisks, git>=2.36, WorktreeAdd cleanup). Round 2: good to commit; follow-ups: WaitDelay false failure with backgrounded hooks, fetch origin explicitly, safe branch cleanup, tag-only risk informational, skip-worktree files, risk scope API, cancel-mid-checkout policy (user decision).
- Step 4 requirement: CLI installs signal.NotifyContext(os.Interrupt, SIGTERM, SIGHUP) so cancellation reaches git process groups (git no longer receives terminal SIGINT); tested. ops must Fetch before remove risk checks.
- Decision 2026-10-14 (user): option (a) — a started local fast-forward merge runs to completion (uncancellable, own long timeout); cancellation applies before/between repos.
- gitx follow-ups (review 2 items 1-8 + decision a) implemented test-first; 4 cancellation tests adapted to merge-timeout semantics as authorised. Light review pending.

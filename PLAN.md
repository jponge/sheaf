# sheaf — implementation plan

Status: approved 2026-10-14. Source of requirements: `goals.md`.

## Decisions taken

| Topic | Decision |
|---|---|
| Dependencies | `spf13/cobra`, `charmbracelet/bubbletea`, `bubbles`, `lipgloss`, `go.yaml.in/yaml/v3` (maintained successor of gopkg.in/yaml.v3, approved 2026-10-14). Tests: stdlib `testing` only. Nothing else without asking. |
| Module / Go | `github.com/jponge/sheaf`, Go 1.27 |
| Repo directory | `<bundle>/<repo>/`; a name clash across owners is an error unless `--as <dir>` is given |
| Primary checkout | the directory `<bundle>/<repo>/<default-branch-normalized>/`; it may have any branch checked out (temporary switches in the primary are a normal workflow) |
| Worktrees | `<bundle>/<repo>/<branch-normalized>/` as siblings of the primary; created with `git worktree add` from the primary |
| Normalization | `/` and other path-unsafe characters → `-`, consecutive `-` collapsed; collisions (e.g. `feat/a-b` vs `feat-a/b`) are detected and refused |
| update | one fetch per repository, then `git merge --ff-only @{upstream}` in each checkout (only checked-out branches move); skip and report when the tree is dirty (staged or unstaged), there is no upstream, or the branch has diverged |
| remove | refuses when any checkout/worktree has uncommitted changes, stashes, unpushed commits (commits of a recorded `wt pr` head count as pushed), unknown entries, or the remote is gone; `--force` overrides only these. Never overridden: working directory inside, repository directory a symlink, no or several primaries, locked worktrees, worktrees outside the directory or of other repositories inside it, unreadable directories in unknown entries, other fetch failures |
| UI mode | TUI when stdin+stdout are TTYs; plain CLI otherwise or with `--no-tui`. In CLI mode every TUI choice has a flag equivalent; missing required input is an error, never a prompt |
| Bundle discovery | walk up from CWD to the first directory containing `sheaf.yaml`; `--bundle <dir>` overrides |
| adopt | a plain clone `foo/` is moved to `foo/<default-branch-normalized>/` whatever branch it has checked out (never switched, see the design decision of 2026-10-14); existing linked worktrees fixed with `git worktree repair`; refuse if the target exists |
| Org listing | archived repos hidden unless `--include-archived`; forks listed and marked |
| Commits | orchestrator commits locally after each reviewed step (conventional commits, no attribution trailers); never pushes |

## Architecture

```
cmd/sheaf/            main.go only
internal/bundle/      sheaf.yaml model, Load/Save, Find (walk up), layout & name normalization
internal/gitx/        thin exec wrapper over git: clone, default branch, status (porcelain v2), ahead/behind,
                      stash count, worktree add/list/remove/repair, fetch (one remote, all remotes, explicit
                      refspecs), fast-forward of a checkout to its upstream after the fetch (merge --ff-only),
                      deletion risks, per-command query cache
internal/hosting/     interface Hosting { ListRepos, Repo, PR, CurrentUser, ListForks }; gh CLI impl (gh repo
                      list/view --json, gh api user); in-memory Fake for tests in the subpackage
                      hosting/hostingtest (like net/http/httptest)
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
    host: gitlab.com         # optional; omitted = github.com, "local" for file:// origins; gh features skip non-GitHub repos
    fork:                    # optional: your fork, cloned as origin (the repository above is then upstream)
      slug: jponge-fork/boiler-vroom
      url: git@github.com:jponge-fork/boiler-vroom.git
```

## Testing rules (non-negotiable, applies to every step)

- Red → green per behaviour; the implementer reports the red and green runs with their output.
- Real `git` only against ephemeral repos under `t.TempDir()`; the remotes are local bare repos reached through `file://`. No network, no push to anything that isn't a temp bare repo.
- `gh` is never invoked in tests: `hosting` is covered by a fake, and the gh adapter by parsing recorded JSON fixtures (plus a stub `gh` script placed on `PATH` in a temp dir).
- TUI models are tested by feeding `tea.Msg`s to `Update` and asserting on state and `View()` output (no extra dependency).
- `go vet ./...` and `go test -race ./...` must be green at the end of every step.
- Packages that run git call `testutil.Main` from TestMain (process-wide isolation, read-only global git config, fail fast without it). Use `t.Parallel()` unless the test touches process-global state (env, PATH stubs, package seams/hooks, signals, KillAll, pty, timing assertions, cwd, executable scripts). Tests that write the global git config call `testutil.IsolateGitPerTest`.
- `make test-symlinked-tmp` must stay green (macOS /var → /private/var class of bugs).

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
- Decision 2026-10-14 (user): keep 10-min merge bound; first Ctrl-C prints 'waiting for in-progress fast-forward in <repo> to finish (Ctrl-C again to force)', second Ctrl-C force-kills the merge (gitx needs a force-cancel channel/context). Scheduled in step 5.
- Decision 2026-10-14 (user): adopt of a clone on a non-default branch → if clean, move to <repo>/<default>/, switch to default, re-create the feature branch as a sibling worktree; if dirty or detached, refuse. Invariant: primary is always <repo>/<normalized default branch>/.
- Decision 2026-10-14 (user): laid-out repo whose primary is not on the default branch (e.g. foo/dev/ primary, default main) → treated like a plain clone: if clean, rename to foo/<default>/, switch, re-create the branch as sibling worktree; if dirty, refuse. To implement together with step 4 review fixes.
- gitx light-review fixes: CRLF/symlink hidden-mod false positives, --stdin-paths hashing (129→98 ms @20k files, measured), RiskUnverifiable for failing required filters, FastForwardWithOptions{MergeTimeout, Force} + ErrForceKilled (for step 5 two-stage Ctrl-C), pinned start point in WorktreeAdd. Not re-reviewed (small, test-first).
- Step 4 review: 3 blocking (adopt checkout overwrote ignored files; remove deleted nested repos in ignored dirs; laid-out detection broke invariant) + 7 should-fix. Decisions 2026-10-14 (user): remove gets OS-junk ignore list + --offline, --force only for data risks; sheaf.yaml repositories get optional host (default github.com), gh features skip non-GitHub repos.
- Step 4 review round 2: originals fixed; remaining R1 (gitlinks/old submodules/nested repos in tracked dirs invisible to remove) + lock wait ignoring cancel + host alias parsing. Orchestrator decision: nested repos discovered everywhere, checked recursively; blocking if unpushed/stashed/dirty/no remote, informational if clean and fully pushed. adopt --host override for SSH aliases.
- Step 4 review round 3: round-2 fixed; B1 nested repos assessed on stale refs → decision: remove fetches each nested repo with a remote (not under --offline), failed fetch blocks. User decision 2026-10-14: bare repos inside a checkout count as nested repos and block (no remote); remotes inside the deleted tree count as no remote.
- Step 4 — ops + CLI (create/init/add/adopt/remove). Opus impl, Opus review x3 (+ orchestrator verification of round 3). Committed.
- Decision 2026-10-14 (user): wt remove branch deletion mirrors git: -d/--delete-branch (merged only), -D/--force-delete-branch (unmerged, states commits lost), --force only for worktree-local data risks.
- Step 6 review round 2: originals fixed. User decision 2026-10-14: `sheaf path <x>` inside a checkout falls back to repository x when no worktree x exists in the current repo; ambiguity error when both exist; wt add/rm keep the strict rule.
- DESIGN DECISION 2026-10-14 (user): the primary checkout is a *directory* (<repo>/<normalized default>/) that may have any branch checked out. Supersedes earlier adopt 'switch if clean' decisions: adopt/init move or rename into <repo>/<default-dir>/ and never switch branches or create worktrees. Laid-out detection keys on the default branch, not the checked-out one. `update` fast-forwards only checked-out branches (local default branch ref left alone when the primary is elsewhere). `wt add <default>` while the primary is off-default is refused with a switch-back hint.
- Step 6 — worktree add/list/remove + path. Opus impl, Opus review x2: round 1 blocking (dir/branch name ambiguity removed the wrong worktree; merged check accepted self-upstream), fixed; round 2 good to commit after path fallback (user decision) and -d containment fixes. Verified in isolation on top of HEAD.
- Adopt rework per primary-checkout model: move/rename only, never switch; laid-out detection keys on default branch (plus plausibility guard). Verified in isolation.
- Step 5 — status/fetch/update/run, two-stage Ctrl-C, interactive git mode. Opus impl, Opus review x2: round 1 blocking (missing primary .git inside an outer repo → update fast-forwarded the outer repo) + 4 should-fix; round 2 blocking (same via linked worktrees) + SIGPIPE, spill leaks, flaky test; fixed; orchestrator verified B1' end to end with the binary.
- Step 7 review: B1 data loss (deleted remote → --offline hint → last copy deleted). Orchestrator decision: remove classifies 'remote repository gone' fetch failures as a data risk (only --force overrides, no --offline hint); sync rejects --offline with --remove-gone; gone candidates confirmed via hosting.Repo (moved/incomplete listing detection), mass-removal guard (>50%) needs --allow-mass-removal.
- Step 10 review: B1 prune/wt rm -d deleted a branch being rebased (detached HEAD) — branches in use now include rebase/bisect state across worktrees. Orchestrator decision: prune lists but does not block on ignored files.
- Decision 2026-10-14 (user): prune removes worktrees with ignored files (listed, not blocking) — 'when we prune, we prune'.
- Step 7 — sync. Opus impl, Opus review x2: round 1 blocking (deleted remote + --offline hint → last copy deleted), mass-gone, alias-host duplicates; round 2: token-scope contradiction refusal, alias-only host matching. Verified in isolation.
- Step 9 — completion + shell-init/scd. Sonnet impl, Opus review: no blocking; fixed bash escaping (pty-tested), --bundle forms, directives. zsh/fish not executable in sandbox (unverified). Verified in isolation.
- Decision 2026-10-14 (user): sync --remove-gone requires --force for any repo with a checkout whose remote is gone or contradicts the API (history may exist nowhere else).
- Step 10 — prune, wt pr, doctor. Opus impl, Opus review x2: round 1 blocking (branch mid-rebase deleted), round 2 should-fix (reftable HEAD placeholder hid checked-out branches; PR head read from shared FETCH_HEAD). Fixed test-first.
- Decision 2026-10-14 (user): latest stable versions of approved libraries are fine (e.g. bubbletea v2: charm.land/bubbletea/v2 v2.1.0, released 2026-10-08; v2.0.0 stable since 2026-02-24).
- Step 8b review: wiring sound; bubbletea v1 init queries terminal background colour (OSC 11, 5 s timeout on silent ptys, eats typeahead) — verified from source and measured. Orchestrator decision: fix S2/S3 on v1, commit 8b, then migrate to bubbletea v2.1.0 / bubbles v2.2.1 / lipgloss v2.0.6 (no init query) as a separate reviewed commit with a pty regression test.
- Step 8b — TUI wiring (menus, confirm dialogs, progress view, unified interrupts). Opus impl, Opus review: no blocking; fixed wrapping, warning replay, Ctrl-C=130 in menus. Known regression pending v2 migration: bubbletea v1 init terminal query.
- Decisions 2026-10-14 (user): ultraviolet + colorprofile direct imports covered by the Charm approval; NO_COLOR/CLICOLOR=0 disable colour only, bold/faint kept (no-color.org, v2 default).
- v2 migration review: B1 v2 startup capability-query replies leak into the shell after fast exits (fix: avoid queries or DA1 drain before quit, pty regression); S3 test helper hid sanitize mutant; pre-existing ops spill-file test flake root-caused (test race).
- Decisions 2026-10-14 (user): accept the exit drain (DA1, ~35 ms, keys typed in the window swallowed); paste-freeze (ultraviolet, ESC P/X/^/_ inside bracketed paste) left as a documented upstream limitation, no mitigation.
- bubbletea v2 migration: Opus impl, Opus review (round 1 blocking: v2 capability-query replies leaking into the shell; fixed with a DA1 drain on quit). Focused round-2 review was stopped by the user; orchestrator verified instead (full suite, pty tests x10, drain code read: bounded 300 ms total, finished models ignore input, force-exit independent of the program).

## Takeaways (2026-10-14)

What worked
- Test-first plus a separate adversarial reviewer per step paid off: reviews found ~20 blocking defects that green test suites had missed, almost all data-loss paths in destructive commands (remove, sync --remove-gone, prune, wt rm -d, adopt). Reviewers that built the binary and ran experiments (pty drivers, fake ssh/upload-pack, mutation checks) found far more than code reading.
- Mutation checks ("break the code, does a test fail?") repeatedly exposed vacuous or weak tests; ask implementers and reviewers for them explicitly.
- Isolation builds before committing (`git archive HEAD` + only the step's files) allowed parallel steps without committing half-integrated trees.
- Registration hooks in separate files (cmd_*.go) let parallel agents work without editing the same file.

Recurring defect classes (check these first next time)
- Inherited environment: GIT_* variables from hooks, user git config (status.showUntrackedFiles, pull.ff, autocrlf), locale. Isolate tests and sanitize child envs.
- "Remote state" assumptions: stale remote-tracking refs, remotes that vanished, listings that are incomplete because of auth scope. Never treat absence in a listing as proof of deletion; confirm, and treat "remote gone" as a data risk.
- Git discovery walking up into an enclosing repository when a checkout lost its .git.
- Processes outliving cancellation: kill process groups (Setsid + SIGTERM, then SIGKILL), WaitDelay false failures, SIGPIPE, temp files on emergency exit.
- Git state the porcelain hides: branches being rebased/bisected (detached HEAD), reftable HEAD placeholders, skip-worktree/assume-unchanged files, nested and bare repositories, FETCH_HEAD shared between processes.
- Terminal I/O: library init-time terminal queries (bubbletea v1), query replies leaking after exit (v2), escaping in completion scripts.

What to do differently
- Settle the core domain model with the user before building on it: the "primary checkout is a directory on any branch" correction came after adopt had been built around "primary is on the default branch" and cost a rework.
- Run the sandbox with an init process from the start (PID exhaustion stopped work once).
- Give every agent an explicit rule against touching real external tools in tests (one test run reached the real `gh auth status`).
- Paste-friendly commands for the user: short lines, no multi-line blocks (terminal wrapping broke fish input twice).
- Prefer targeted re-reviews over full ones after a blocking fix; the last focused re-review ran long and was stopped.
- Test speed (user: do what's best): ops 20.5 s Linux / 72.9 s macOS, 210 serial tests (~0.1 s each, git process spawns), 0 t.Parallel because IsolateGit uses t.Setenv. Plan after the macOS path fix: package-level isolation in TestMain + t.Parallel where no process-global state, cached fixture repos copied per test, before/after measurements. Target: <5 s per package on Linux, full suite <30 s.
- macOS: 15 failures were test expectations using unresolved temp paths (no product bug); force-kill delay attributed to a fork racing the group SIGKILL (macOS reasoning unverified), fixed with straggler re-kill; added make test-symlinked-tmp. Takeaway: a step-0 portability finding (N5) was dropped and no non-Linux check existed.
- Test speed-up: package-level isolation (testutil.Main), t.Parallel where safe (ops 186/210, gitx 113/148, cli 96/142), cached NewOrigin fixtures, sheaf binary built once per cli test process. Full suite ~27 s → ~11 s plain, ~28 s → ~14 s -race (Linux, 5 CPUs). Floors: gitx cancel/deadline tests (~8 s), cli pty completion driver with fixed windows (~6 s).
- macOS (user, git 2.56.0): full suite green plain and -race at 4b00fa1. Timings plain: ops 31.4 s (was 72.9), gitx 27.3 s, cli 11.7 s; -race: ops 35.5, gitx 32.3, cli 15.7. macOS ~3x slower than the Linux sandbox.

## Follow-up features (decided 2026-10-15)

| # | Feature | Decisions |
|---|---|---|
| 11 | Tag worktrees | `sheaf wt add [<repo>] --tag <tag>`: fetch tags, detached worktree at the tag in `<repo>/<normalized tag>/` (same namespace as branch dirs, collision-checked). update reports it as pinned and never moves it; prune never removes it; status/list/path/completion know the tag. `wt add <name>` where `<name>` is a tag but not a branch is refused with a hint to use `--tag` (fixes today's footgun of silently creating a branch named like the tag). |
| 12 | Fork-aware repositories | origin = your fork, upstream = reference repository. sheaf.yaml keeps the reference slug (sync/org/wt pr use it) and records the fork. update fetches both remotes; the default branch tracks upstream/<default>; new worktrees start from upstream/<default>; merged/gone/unpushed/remote-gone checks consider both remotes. Auto-detected on `add` (single and --org, one gh listing of your forks; `--no-fork` opts out) and recognised on `adopt` (existing origin+upstream remotes). sheaf never creates forks. Repos without a fork keep today's single-origin behaviour. |
- gh verified 2026-10-15 (user): `gh api user --jq .login` → login; `gh repo list <me> --fork --json name,owner,parent,url,sshUrl,isFork` gives parent {name, owner.login} (no URL); non-fork parent=null. Fixtures saved for step 12.
- Decision 2026-10-15 (user): rename the bundle lock file sheaf.yaml.lock → .sheaf.lock (kept on disk by design: flock; deleting on unlock is racy). To do right after step 11 (avoids editing ops concurrently with the step 11 agent).
- Step 11 — tag worktrees + clearer --base errors. Opus impl (restarted after an accidental stop; inherited tests mutation-checked), Opus review: no blocking; fixed moved-tag display, fetch-output tolerance, ref-like --tag, footgun for unfetched tags, local-branch suggestions.
- Lock file renamed to .sheaf.lock (test-first).
- Decision 2026-10-16 (user): forked repos keep git's default push behaviour (no remote.pushDefault set by sheaf).
- Step 12 review: B1 remove with gone fork ignored commits only on stale fork refs (fixed: fork refs count as fork-only tips). Decisions 2026-10-16 (user): update does partial fast-forwards when only the fork fetch fails; wt rm/prune classify a gone fork like remove (stale fork refs untrusted; --offline also untrusts fork refs for merged decisions). adopt requires shared history to recognise a fork.
- Step 12 — fork-aware repositories. Opus impl, Opus review x2: round 1 blocking (gone fork: commits only on stale fork refs deleted by remove) + adopt trusted any upstream + gone-fork handling in wt rm/prune/update (user decisions); round 2 ready. Open NITs: adopt's 'fetch upstream' hint uses the pre-move path; no way to convert an already-registered single-remote entry into a fork entry.
- macOS (user) at ba7d7b3: Tag|Fork gitx tests and full -race suite green (git 2.56 → tag clobber parser OK there). -race timings: ops 44.5 s, gitx 34.6 s, cli 21.0 s. Decision 2026-10-16 (user): add `sheaf adopt --recheck <repo>` to re-examine remotes of a registered repo and record a fork; fix the stale path in adopt's fetch hint.
- `sheaf adopt --recheck <repo>...` implemented (test-first): re-examines a registered repo's primary checkout with adopt's recognition rules, records the fork (slug/url from upstream, fork from origin), keeps directory/branches/tracking, never drops or changes a recorded fork (warns), refuses a reference-slug collision, uses the entry's default branch (no network). adopt's fetch hint now names the final primary path and points to `--recheck`.
- Test speed round 2: step 11/12 tests already conform (96/103 parallel); fixture git time ops −31%, CPU −7%, wall unchanged (Linux). Fixed: cached origin copies' remote pointed into the shared cache (isolation hole from round 1); fixture-cache race (entries could share a directory). Next lever: repeated product git calls (rev-parse --show-toplevel ×2211, worktree list ×1189, show-ref ×1076 in ops tests).
- Decision 2026-10-16 (user): step 13 — reduce repeated git calls in production code with a per-command cache of read-only git queries; safety re-checks before deletion and re-reads under the lock stay fresh.
- Step 13 review: no blocking; S1 missing test for tag-add fresh read under lock (shown to matter), repoKey lexical vs physical relative gitdir; proceeding with default (keep cache) pending user's keep/drop answer.
- Step 13 — per-command query cache + DeletionRisks top-level refactor + git-call budgets. Opus impl, Opus review: no blocking; fixed tag-add lock re-check test, physical relative gitdir resolution, no caching of fatal failures. git calls −12% suite-wide (doctor −19%, fetch −33%, prune −22%, remove −26%); wall unchanged.
- macOS (user) at ef33315, -race: ops 46.1 s (was 44.5), gitx 31.3 s (was 34.6), cli 17.5 s (was 21.0). Step 13 did not help ops on macOS; further work needs per-test macOS timings.
- Cached whole-tree fixtures (testutil.CachedTree) for pruneScenario and forkGoneTopic: ops processes −14%, CPU −15%/−18% (-race); the 15 prune-scenario tests −57% processes. Flaky TestFastForwardWithOptionsForceAlreadyClosed fixed (empty pidfile race; not reproduced).
- macOS (user) at 040cb46, -race slowest ops tests: TestR10BranchMovedAfterPlan 4.02 s (builds the cached scenario), other prune-scenario tests 1.8–2.4 s (were 3.1–3.9 s); TestGitCallsPerCommand 3.35 s (was 5.1).
- Decision 2026-10-16 (user): test execution time is acceptable; speed work closed.
- fish 4.0.2 / zsh 5.9 verified in the container (Debian debs unpacked without root): scd + completion work; fixed quoting/escaping in zsh and fish scd completion, and patched cobra's generated fish script (it evaluated quoted (cmd) words on TAB). make test-shells added. Open: same quoting bug in bash scd completion; cobra zsh open-quote issue (upstream).
- Decisions 2026-10-16 (user): keep the cobra fish-script patch (orchestrator drafts an upstream report for the user to file); fix bash scd completion quoting with a dequote routine.
- bash scd completion: quote-aware dequote and open-quote completion (test-first, 9/10 red → green). Cobra fish issue draft left untracked at ./cobra-fish-issue.md for the user to file.

## Backlog (recorded 2026-10-16, user: "All items will need to be addressed")

| # | Item | Status |
|---|---|---|
| B1 | User documentation: README (install, concepts, layout, `sheaf.yaml` schema, commands, shell integration, safety model) | done (README.md) |
| B2 | Bare `sheaf` (no subcommand) opens a TUI home screen (goals.md: "runs as a bubbletea TUI and displays menus") instead of printing help | done (af66acc) |
| B3 | Merge the duplicated listings `ops.ListWorktrees` (step 6) and `ops.ListCheckouts` (step 5) | done (810b2fd, 975e844) |
| B4 | CI (GitHub Actions): `make test`, `make test-symlinked-tmp`, `make test-shells` on Linux and macOS | workflow added (never run: needs push; macOS runner, ubuntu fish 3.7 and timeout unverified) |
| B5 | Windows: declare unsupported (docs + clear startup error) rather than half-support | done (a4a8745: refused at startup) |
| B6 | Cross-step end-to-end review: inconsistencies, dead code, leftovers from design changes (e.g. adopt's old switching logic) | done (b315805) |
| B7 | Dogfooding on the user's Mac (real bundles, GitHub error texts, ssh prompts, terminal emulator, macOS fish/zsh) | later, user-driven |
| B8 | Release packaging (GoReleaser/Homebrew: new tool, needs approval) | decided 2026-10-16 (user): `go install ...@<tag>` only (README); no release tooling |
| B9 | Housekeeping: goals.md, cobra-fish-issue.md (file upstream, then delete) | user |
| B10 | Remaining bash scd completion gaps (single quote inside open single quote, `$'…'`, `!` in double quotes); cobra zsh open-quote (upstream); fish < 3.7 `-o` fallback untested | decided 2026-10-16 (user): documented as known limits in README |
| B11 | `sheaf version` falls back to Go build info (go install builds print `dev` today); Makefile `install` target | done: build-info fallback + make install |
| B12 | Warn on git older than 2.36 in every command, not only doctor | decided 2026-10-16 (user): leave as is (doctor only) |
| B13 | `wt pr` on an existing, not checked-out pr/<n> fetches under .sheaf.lock with interactive git: fetch before locking | done (cf624f3) |
| B14 | Root `--help` does not mention the home screen (kept byte-identical by decision) | done (a4a8745) |
| B15 | Check that all Go dependencies are on their latest stable versions; upgrade (user: latest stable OK) | done: yaml v3.0.5, x/sys v0.49.0, x/sync v0.24.0, pflag v1.0.10, terminfo v1.2.0, runewidth v0.0.31; Charm, cobra, x/ansi, x/term, colorprofile already latest; ultraviolet at main head (no tags) |
| B16 | License | done: Apache-2.0, "Copyright 2026 The sheaf Authors" (user, 2026-10-16) |
| B17 | Idiomatic Go review (2026-10-16, golangci-lint 2.14 + AST scripts; gofmt/vet clean). HIGH: `co` prefix as namespace (93 identifiers, 43 files; leaked into the error text "coCommands killed on request", checkout_run.go:306); behaviour switches in context values (WithInteractive, WithFreshReads, interrupt controller). MEDIUM: ~17 package-level test hooks in production; over-wide exports in internal packages (~15-20 funcs/consts); hosting.Fake in production package (→ hostingtest); review-round names in test files (23) and comments; %v for secondary errors (14); triplicated resolves-to-itself guard. LOW: coJson* initialisms, cache.lookup error-in-middle, const doc comments, ctx in structs (2), shadowed max, gitx: prefix inconsistency, modernize (~15 production sites), long functions, 2 unparam | done: 43e197b, 0e0cc29, 8e3e50b, b3ff003 (WithFreshReads, killGroup, process registries, cli.Version kept with reasons) |
| B18 | Select all / deselect all in every TUI list (user report: the home screen overview has none) | done (`a`/`n` in the overview, add worktree on `w`; scripted-input race in the TUI test harness fixed: each program claims its input segment at creation) |
| B19 | TUI sync: drop the "Exclude the unselected new repositories from future syncs" step (user, 2026-10-16: confusing; unselected repositories are simply not added); audit the other TUI flows for similar confusing steps and report | done: exclusion step removed (sync --exclude remains the way to decline for good); audit recorded as B21 |
| B20 | gh default repository (user report: `gh pr list` fails in fork clones: no default) | implemented uncommitted (set-default in add/adopt/recheck, `adopt --recheck --all`, warn once, serialized gh calls); decision 2026-10-16 (user): keep an existing default and report it (set only when none) — blocked on real gh output to read the current default; doctor check pending the same |
| B21 | TUI flow audit (2026-10-16): (1) add --org: enter with nothing new selected silently tracks the org, counter counts present repos; (2) deletion flows title "Removing … · 1 running" before confirmation, "skipped" after declining; (3) sync lets refused (mass removal) gone repos be selected; (4) home says "prune: done"/"sync: done" when nothing happened; (5) wt rm dialog does not say the branch is kept; (6) home multi-remove clears selection after cancel; (7) help wording differs home vs menus; (8) prune confirmation says "would"; (9) sync shows nothing while listing orgs; (10) init/adopt: deselected checkouts not mentioned afterwards; (11) second Ctrl-C message generic (mentions adopt folders during update); (12) stale overview copies may pile up in scrollback (unverified); (13) add-worktree prompt terse, empty enter silently ignored | done (12: not reproduced; terminal-emulator regression test); review fixes: prune dialog states each worktree branch fate, emergency-note wiring tested |
| B22 | Flaky TestTUIRemoveDialogCtrlCExits130 (exit 1 "cancelled" instead of 130; 2/300 at b3ff003^ and 1/300 after: pre-existing, single segment, not the segment race); also: execute(cmd, nil, ...) binds a controller the Deps never see (require non-nil) | closed 2026-10-16 (user): not worth investigating |
| B23 | Repository names starting with `-` are valid (bundle.ValidateRepoName), so hints like `sheaf adopt -x` would be read as a flag: refuse leading `-` or print `--` in hints | done: names starting with `-` refused (user, 2026-10-16) |

Order for the high-priority items: B3 → B6 (review sees the merged code) → B2 → B1 (docs describe the final behaviour).
- Decisions 2026-10-16 (user) for B2: bare `sheaf` in a bundle with a TTY opens an overview (checkouts table from local status, no network at open; multi-select, filter) with actions update/fetch/wt add/wt rm/prune/doctor/sync reusing the per-command TUIs and returning to the overview; enter on a row cds there through the shell-init wrapper (prints the path without it). Outside a bundle: help plus a create/init hint. --no-tui or non-TTY: help, unchanged.
- B3 (810b2fd): shared enumeration (enumerateRepo) with the listings' 12 divergences kept as enumPolicy options. Decisions 2026-10-16 (user): (a) a linked worktree of another repository or without .git is a bug in the wt commands: verify harm by experiment, then report/refuse it like status does (test-first); (b) one strict repo-dir + primary check for all commands (real repo dir, .git directory, no symlinked primary, git-dir == common-dir, first worktree-list entry is the primary), one error text, no double check in doctor/prune; (c) outside rule and order unified on wt's (literal and resolved parent both the repo dir; outside last); (d) missing-dir/prunable display, Dir/Path/OffDefault fields and ListWorktrees sequencing stay view differences, documented.
- B6 cross-step review (810b2fd): no blocking (deletion paths held in experiments). SHOULD: S1 clean `wt pr` branch counted as unpushed → remove/sync --remove-gone refuse (needs --force) and blame the primary's branch; S2 locked worktree: hard in wt rm/prune, --force-overridable in remove (then RemoveAll fallback); S3 a worktree of repo A inside B's dir: hard for `remove A`, --force-deletable (with its uncommitted changes, unlisted) for `remove B`; S4 repo-name matching case-insensitive in path/wt list, case-sensitive elsewhere, raw lstat error in adopt --recheck; S5 wt add/rm don't use checkoutContext (no interactive git), single-repo commands still require -j 1 for it; S6 remove re-checks and deletes outside .sheaf.lock. Docs: wt add --no-fetch vs --offline elsewhere; PLAN adopt row and architecture stale; wt rm no-arg hint; -j help. NIT: 11 dead functions (mostly origin-literal pre-fork API), unparam, message/pluralization/help-wrap/JSON-envelope inconsistencies.
- Decisions 2026-10-16 (user) on B6: S1 commits reachable from a pr/<n> branch's PR head (refs/pull/<n>/head) count as published; messages name the branches carrying unpushed commits. S2/S3 remove refuses locked worktrees and another repository's worktree inside the directory, whatever --force. S4 exact (case-sensitive) repo names everywhere, "did you mean" hint on a case-only miss. Also: S5 (wt add/rm interactive git; single-repo commands ignore -j for it), S6 (remove under .sheaf.lock), delete the dead code, message/help/docs fixes (wt add --offline alias of --no-fetch). JSON envelopes unchanged.
- Decisions 2026-10-16 (user) on B3 follow-up: rewrite TestFetchSerializedPerCommonDir to assert the symlinked repo dir is refused and alpha fetched; one shared error text per failure kind; accept the per-linked-worktree rev-parse cost (path budget 4+2; file-based check possible later, measured); doctor warns about unusable linked worktrees.
- B6 fixes committed (b315805) after an adversarial review (no blocking; S3 holes under embedded repos/inside checkouts closed, lock-order and no-fallback regression tests). Defaults pending user objection: offline remove trusts recorded PR heads; a failed PR-head fetch trusts none; all risk counts pluralized. Open: `wt pr` on an existing unchecked-out pr/<n> fetches under .sheaf.lock with interactive git (a prompt can hold the lock; fix: fetch before locking).
- B2 home screen (381d439 + da5f495 in an agent worktree, applied as a patch onto b315805): overview from local status, actions via in-process root commands, enter → SHEAF_CD_FILE written for the shell-init `sheaf` function (variable removed from sheaf's environment at startup so children can't use it), CheckoutFilter.Paths for per-worktree update. Review: no blocking; fixed the SHEAF_CD_FILE leak to children (reproduced: a `sheaf run` child moved the shell to /etc), errexit temp-file leak, mktemp noise, untested guards. Hidden selected rows are targets, as in MultiSelect.

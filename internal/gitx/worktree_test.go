package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// resolved returns p with symlinks resolved (git reports resolved paths).
func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustList(t *testing.T, repoDir string) []Worktree {
	t.Helper()
	wts, err := WorktreeList(context.Background(), repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return wts
}

func findWorktree(t *testing.T, wts []Worktree, path string) Worktree {
	t.Helper()
	for _, w := range wts {
		if w.Path == path {
			return w
		}
	}
	t.Fatalf("worktree %q not in %+v", path, wts)
	return Worktree{}
}

func mustAdd(t *testing.T, repoDir, path, branch string, opts WorktreeAddOpts, want WorktreeAddMode) WorktreeAddResult {
	t.Helper()
	res, err := WorktreeAdd(context.Background(), repoDir, path, branch, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != want {
		t.Fatalf("WorktreeAdd(%q) mode = %v, want %v", branch, res.Mode, want)
	}
	return res
}

// layout returns a primary clone at <root>/repo/main and the <root>/repo dir.
func layout(t *testing.T, opts testutil.OriginOpts) (o testutil.Origin, repo, primary string) {
	t.Helper()
	o = testutil.NewOrigin(t, opts)
	repo = filepath.Join(t.TempDir(), "repo")
	primary = filepath.Join(repo, "main")
	if err := Clone(context.Background(), o.URL, primary, ""); err != nil {
		t.Fatal(err)
	}
	return o, resolved(t, repo), resolved(t, primary)
}

func TestWorktreeListPrimaryOnly(t *testing.T) {
	_, _, primary := layout(t, testutil.OriginOpts{})
	wts := mustList(t, primary)
	if len(wts) != 1 {
		t.Fatalf("got %d worktrees: %+v", len(wts), wts)
	}
	w := wts[0]
	if w.Path != primary || w.Branch != "main" || w.Detached || w.Bare || w.Locked || w.Prunable {
		t.Fatalf("primary = %+v", w)
	}
	if want := testutil.Git(t, primary, "rev-parse", "HEAD"); w.Head != want {
		t.Fatalf("Head = %q, want %q", w.Head, want)
	}
}

func TestWorktreeListBare(t *testing.T) {
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	wts := mustList(t, o.Path)
	if len(wts) != 1 || !wts[0].Bare || wts[0].Path != resolved(t, o.Path) {
		t.Fatalf("bare list = %+v", wts)
	}
}

func TestWorktreeAddExistingLocalBranch(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	testutil.Git(t, primary, "branch", "local-work")
	path := filepath.Join(repo, "local work dir")
	mustAdd(t, primary, path, "local-work", WorktreeAddOpts{}, WorktreeLocalBranch)
	w := findWorktree(t, mustList(t, primary), path)
	if w.Branch != "local-work" || w.Detached {
		t.Fatalf("worktree = %+v", w)
	}
}

func TestWorktreeAddRemoteBranchCreatesTrackingBranch(t *testing.T) {
	const b = "feat/cli/rework-ergonomics"
	_, repo, primary := layout(t, testutil.OriginOpts{Branches: []string{b}})
	path := filepath.Join(repo, "feat-cli-rework-ergonomics")
	mustAdd(t, primary, path, b, WorktreeAddOpts{}, WorktreeRemoteBranch)
	w := findWorktree(t, mustList(t, primary), path)
	if w.Branch != b {
		t.Fatalf("worktree = %+v", w)
	}
	if got := testutil.Git(t, path, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/"+b {
		t.Fatalf("upstream = %q", got)
	}
	if got, want := testutil.Git(t, path, "rev-parse", "HEAD"), testutil.Git(t, primary, "rev-parse", "origin/"+b); got != want {
		t.Fatalf("HEAD = %s, want %s", got, want)
	}
}

func TestWorktreeAddNewBranchFromOriginDefault(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{DefaultBranch: "trunk", Branches: []string{"other"}})
	// Move the primary away from origin/trunk so HEAD is not a valid base.
	testutil.Git(t, primary, "checkout", "-q", "other")
	testutil.CommitFile(t, primary, "x.txt", "x\n", "local only")

	path := filepath.Join(repo, "feat-new")
	mustAdd(t, primary, path, "feat/new", WorktreeAddOpts{}, WorktreeNewBranch)
	w := findWorktree(t, mustList(t, primary), path)
	if w.Branch != "feat/new" {
		t.Fatalf("worktree = %+v", w)
	}
	if want := testutil.Git(t, primary, "rev-parse", "origin/trunk"); w.Head != want {
		t.Fatalf("new branch tip = %s, want origin/trunk %s", w.Head, want)
	}
	st := mustStatus(t, path)
	if st.Upstream != "" {
		t.Fatalf("new branch must not track its base, upstream = %q", st.Upstream)
	}
}

func TestWorktreeAddNewBranchFromExplicitBase(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{Branches: []string{"other"}})
	path := filepath.Join(repo, "from-other")
	mustAdd(t, primary, path, "from-other", WorktreeAddOpts{Base: "origin/other"}, WorktreeNewBranch)
	if got, want := testutil.Git(t, path, "rev-parse", "HEAD"), testutil.Git(t, primary, "rev-parse", "origin/other"); got != want {
		t.Fatalf("HEAD = %s, want %s", got, want)
	}
}

func TestWorktreeAddErrors(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	ctx := context.Background()
	if _, err := WorktreeAdd(ctx, primary, filepath.Join(repo, "x"), "", WorktreeAddOpts{}); err == nil {
		t.Error("empty branch: expected error")
	}
	// main is already checked out in the primary.
	if _, err := WorktreeAdd(ctx, primary, filepath.Join(repo, "main2"), "main", WorktreeAddOpts{}); err == nil {
		t.Error("branch checked out elsewhere: expected error")
	}
	if _, err := WorktreeAdd(ctx, primary, filepath.Join(repo, "bad"), "-bad", WorktreeAddOpts{}); err == nil {
		t.Error("invalid branch name: expected error")
	}
	if len(mustList(t, primary)) != 1 {
		t.Error("failed adds left worktrees behind")
	}
}

func TestWorktreeRemove(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	ctx := context.Background()
	path := filepath.Join(repo, "tmp")
	mustAdd(t, primary, path, "tmp", WorktreeAddOpts{}, WorktreeNewBranch)

	writeFile(t, path, "file-1.txt", "dirty\n")
	if err := WorktreeRemove(ctx, primary, path, false); err == nil {
		t.Fatal("removing a dirty worktree without force: expected error")
	}
	if err := WorktreeRemove(ctx, primary, path, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present: %v", err)
	}
	if n := len(mustList(t, primary)); n != 1 {
		t.Fatalf("%d worktrees after remove", n)
	}
}

func TestWorktreeListDetachedLockedPrunableAndRepair(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	ctx := context.Background()

	det := filepath.Join(repo, "detached")
	testutil.Git(t, primary, "worktree", "add", "-q", "--detach", det)
	if w := findWorktree(t, mustList(t, primary), det); !w.Detached || w.Branch != "" || w.Head == "" {
		t.Fatalf("detached = %+v", w)
	}

	locked := filepath.Join(repo, "locked")
	testutil.Git(t, primary, "worktree", "add", "-q", "-b", "locked", locked)
	testutil.Git(t, primary, "worktree", "lock", "--reason", "on usb stick", locked)
	if w := findWorktree(t, mustList(t, primary), locked); !w.Locked || w.LockReason != "on usb stick" {
		t.Fatalf("locked = %+v", w)
	}

	moved := filepath.Join(repo, "moved")
	if err := os.Rename(det, moved); err != nil {
		t.Fatal(err)
	}
	if w := findWorktree(t, mustList(t, primary), det); !w.Prunable || w.PrunableReason == "" {
		t.Fatalf("moved-away worktree should be prunable: %+v", w)
	}
	if err := WorktreeRepair(ctx, primary, moved); err != nil {
		t.Fatal(err)
	}
	if w := findWorktree(t, mustList(t, primary), moved); w.Prunable || !w.Detached {
		t.Fatalf("repaired = %+v", w)
	}
	if !IsRepo(ctx, moved) {
		t.Fatal("repaired worktree is not a work tree")
	}
}

func TestWorktreeAddModeString(t *testing.T) {
	for m, want := range map[WorktreeAddMode]string{
		WorktreeLocalBranch:  "local-branch",
		WorktreeRemoteBranch: "remote-branch",
		WorktreeNewBranch:    "new-branch",
		WorktreeAddMode(0):   "WorktreeAddMode(0)",
	} {
		if got := m.String(); got != want {
			t.Errorf("String(%d) = %q, want %q", int(m), got, want)
		}
	}
}

func wantNoBranch(t *testing.T, dir, branch string) {
	t.Helper()
	if refs := testutil.Git(t, dir, "for-each-ref", "refs/heads/"+branch); refs != "" {
		t.Errorf("branch %s left behind: %q", branch, refs)
	}
}

// S5: a non-empty or non-directory target path is refused before anything is
// created, whatever the mode.
func TestWorktreeAddRefusesOccupiedPath(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{Branches: []string{"remote-b"}})
	ctx := context.Background()
	occupied := filepath.Join(repo, "occupied")
	writeFile(t, occupied, "keep.txt", "precious\n")
	file := filepath.Join(repo, "a-file")
	writeFile(t, repo, "a-file", "x\n")
	for _, path := range []string{occupied, file} {
		for _, b := range []string{"brand-new", "remote-b"} {
			if _, err := WorktreeAdd(ctx, primary, path, b, WorktreeAddOpts{}); err == nil {
				t.Errorf("WorktreeAdd(%s, %s): expected an error", path, b)
			}
			wantNoBranch(t, primary, b)
		}
	}
	if b, err := os.ReadFile(filepath.Join(occupied, "keep.txt")); err != nil || string(b) != "precious\n" {
		t.Fatalf("occupied dir content = %q, %v", b, err)
	}
	if n := len(mustList(t, primary)); n != 1 {
		t.Fatalf("%d worktrees", n)
	}
}

func TestWorktreeAddAcceptsEmptyDirectory(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	path := filepath.Join(repo, "empty")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, primary, path, "in-empty", WorktreeAddOpts{}, WorktreeNewBranch)
}

// S5: when git fails after creating the branch, the branch is deleted.
func TestWorktreeAddDeletesBranchCreatedByFailedAdd(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("SKIP: running as root, a read-only directory does not make git fail")
	}
	_, repo, primary := layout(t, testutil.OriginOpts{Branches: []string{"remote-b"}})
	ro := filepath.Join(repo, "read-only")
	if err := os.Mkdir(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	for _, b := range []string{"brand-new", "remote-b"} {
		if _, err := WorktreeAdd(context.Background(), primary, filepath.Join(ro, "wt"), b, WorktreeAddOpts{}); err == nil {
			t.Fatalf("WorktreeAdd(%s) under a read-only dir: expected an error", b)
		}
		wantNoBranch(t, primary, b)
	}
	// A pre-existing local branch is never deleted.
	testutil.Git(t, primary, "branch", "mine")
	if _, err := WorktreeAdd(context.Background(), primary, filepath.Join(ro, "wt"), "mine", WorktreeAddOpts{}); err == nil {
		t.Fatal("expected an error")
	}
	if refs := testutil.Git(t, primary, "for-each-ref", "refs/heads/mine"); refs == "" {
		t.Fatal("pre-existing branch deleted")
	}
}

// WorktreeAdd never reaches the network: without Base and without a local
// refs/remotes/origin/HEAD it fails instead of asking the remote.
func TestWorktreeAddWithoutBaseNeedsLocalOriginHEAD(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	testutil.Git(t, primary, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	_, err := WorktreeAdd(context.Background(), primary, filepath.Join(repo, "x"), "x", WorktreeAddOpts{})
	if !errors.Is(err, ErrNoDefaultBranch) {
		t.Fatalf("err = %v, want ErrNoDefaultBranch", err)
	}
	wantNoBranch(t, primary, "x")
}

// The result exposes the upstream so callers can warn when an existing local
// branch tracks something other than origin/<branch>.
func TestWorktreeAddReportsUpstream(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{Branches: []string{"feat", "other"}})
	if res := mustAdd(t, primary, filepath.Join(repo, "feat"), "feat", WorktreeAddOpts{}, WorktreeRemoteBranch); res.Upstream != "origin/feat" {
		t.Errorf("remote mode upstream = %q", res.Upstream)
	}
	testutil.Git(t, primary, "branch", "--track", "mismatch", "origin/other")
	if res := mustAdd(t, primary, filepath.Join(repo, "mismatch"), "mismatch", WorktreeAddOpts{}, WorktreeLocalBranch); res.Upstream != "origin/other" {
		t.Errorf("local mode upstream = %q, want origin/other", res.Upstream)
	}
	testutil.Git(t, primary, "branch", "plain")
	if res := mustAdd(t, primary, filepath.Join(repo, "plain"), "plain", WorktreeAddOpts{}, WorktreeLocalBranch); res.Upstream != "" {
		t.Errorf("untracked local branch upstream = %q", res.Upstream)
	}
	if res := mustAdd(t, primary, filepath.Join(repo, "fresh"), "fresh", WorktreeAddOpts{}, WorktreeNewBranch); res.Upstream != "" {
		t.Errorf("new branch upstream = %q", res.Upstream)
	}
}

// Review 2, item 4: the cleanup after a failed `worktree add -b` deletes the
// branch only if this call created it and it still points at the start point.
func TestCleanupCreatedBranchSparesBranchesItDidNotCreate(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	ctx := context.Background()
	start := testutil.Git(t, primary, "rev-parse", "origin/main")
	testutil.CommitFile(t, primary, "w.txt", "w\n", "precious work")
	other := head(t, primary)
	testutil.Git(t, primary, "reset", "-q", "--hard", start)
	refOf := func(b string) string {
		return testutil.Git(t, primary, "for-each-ref", "--format=%(objectname)", "refs/heads/"+b)
	}

	// The branch already existed (created by someone else between WorktreeAdd's
	// check and the add): git refuses, the branch must survive, wherever it points.
	for _, at := range []string{other, start} {
		testutil.Git(t, primary, "branch", "-f", "foreign", at)
		_, addErr := Run(ctx, primary, "worktree", "add", "--quiet", "--no-track", "-b", "foreign", "--", filepath.Join(repo, "f"), start)
		if addErr == nil {
			t.Fatal("precondition: worktree add -b on an existing branch succeeded")
		}
		cleanupCreatedBranch(ctx, primary, "foreign", start, addErr)
		if got := refOf("foreign"); got != at {
			t.Errorf("existing branch at %s: after cleanup it is %q", at[:7], got)
		}
	}

	genericErr := &GitError{Args: []string{"worktree", "add"}, Dir: primary, ExitCode: 128,
		Stderr: "fatal: could not create directory of '/x': Permission denied", Err: errors.New("exit status 128")}
	// Created by this call but moved since: not ours to delete any more.
	testutil.Git(t, primary, "branch", "moved", other)
	cleanupCreatedBranch(ctx, primary, "moved", start, genericErr)
	if got := refOf("moved"); got != other {
		t.Errorf("moved branch: after cleanup it is %q", got)
	}
	// Created by this call, untouched: deleted.
	testutil.Git(t, primary, "branch", "ours", start)
	cleanupCreatedBranch(ctx, primary, "ours", start, genericErr)
	wantNoBranch(t, primary, "ours")
	// At the start point but checked out in a worktree: never deleted.
	mustAdd(t, primary, filepath.Join(repo, "busy"), "busy", WorktreeAddOpts{Base: start}, WorktreeNewBranch)
	cleanupCreatedBranch(ctx, primary, "busy", start, genericErr)
	if got := refOf("busy"); got != start {
		t.Errorf("checked-out branch: after cleanup it is %q", got)
	}
}

// Review 2c, item 6: the start point of a new branch is the commit resolved
// before the add (pinned by id), so a base that moves in between cannot leak
// into the branch. The branch reflog records the start point git was given.
func TestWorktreeAddNewBranchIsCreatedFromResolvedStartPoint(t *testing.T) {
	_, repo, primary := layout(t, testutil.OriginOpts{})
	want := testutil.Git(t, primary, "rev-parse", "origin/main")
	mustAdd(t, primary, filepath.Join(repo, "pinned"), "pinned", WorktreeAddOpts{}, WorktreeNewBranch)
	mustAdd(t, primary, filepath.Join(repo, "pinned2"), "pinned2", WorktreeAddOpts{Base: "origin/main"}, WorktreeNewBranch)
	for _, b := range []string{"pinned", "pinned2"} {
		if got := testutil.Git(t, primary, "reflog", "show", "--format=%gs", "refs/heads/"+b); !strings.Contains(got, "Created from "+want) {
			t.Errorf("reflog of %s = %q, want a start point pinned to %s", b, got, want)
		}
	}
}

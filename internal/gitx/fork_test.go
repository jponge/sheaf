package gitx

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// forkedClone clones a forked fixture like sheaf add does for a fork.
func forkedClone(t *testing.T, opts testutil.OriginOpts) (testutil.ForkedOrigin, string) {
	t.Helper()
	f := testutil.NewForkedOrigin(t, opts)
	dest := filepath.Join(t.TempDir(), "repo", "main")
	if err := (Net{}).CloneFork(context.Background(), f.Upstream.URL, f.Fork.URL, dest, "main"); err != nil {
		t.Fatal(err)
	}
	return f, dest
}

func TestRemotes(t *testing.T) {
	t.Parallel()
	if r := OriginRemotes; r.Reference != "origin" || r.Fork != "" || r.Forked() ||
		!reflect.DeepEqual(r.All(), []string{"origin"}) || !reflect.DeepEqual(r.BranchSearch(), []string{"origin"}) {
		t.Fatalf("OriginRemotes = %+v", r)
	}
	if r := ForkRemotes; r.Reference != "upstream" || r.Fork != "origin" || !r.Forked() ||
		!reflect.DeepEqual(r.All(), []string{"upstream", "origin"}) || !reflect.DeepEqual(r.BranchSearch(), []string{"origin", "upstream"}) {
		t.Fatalf("ForkRemotes = %+v", r)
	}
}

func TestCloneForkLayout(t *testing.T) {
	t.Parallel()
	f, dir := forkedClone(t, testutil.OriginOpts{Branches: []string{"feat"}})
	ctx := context.Background()
	for remote, want := range map[string]string{"origin": f.Fork.URL, "upstream": f.Upstream.URL} {
		if got, err := RemoteURL(ctx, dir, remote); err != nil || got != want {
			t.Fatalf("remote %s = %q, %v; want %q", remote, got, err, want)
		}
	}
	if up := testutil.Git(t, dir, "rev-parse", "--abbrev-ref", "main@{upstream}"); up != "upstream/main" {
		t.Fatalf("main tracks %q, want upstream/main", up)
	}
	if b, err := CurrentBranch(ctx, dir); err != nil || b != "main" {
		t.Fatalf("branch = %q, %v", b, err)
	}
	for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/feat", "refs/remotes/upstream/feat"} {
		if ok, err := refExists(ctx, dir, ref); err != nil || !ok {
			t.Fatalf("%s missing (%v)", ref, err)
		}
	}
	if def, err := (Net{}).DefaultBranchOf(ctx, dir, "upstream"); err != nil || def != "main" {
		t.Fatalf("Net{}.DefaultBranchOf(upstream) = %q, %v", def, err)
	}
}

func TestCloneForkFailsOnUnreachableFork(t *testing.T) {
	t.Parallel()
	f := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dest := filepath.Join(t.TempDir(), "repo", "main")
	if err := (Net{}).CloneFork(context.Background(), f.Upstream.URL, "file:///nonexistent/fork.git", dest, "main"); err == nil {
		t.Fatal("clone with an unreachable fork succeeded")
	}
}

func TestFetchRemoteFetchesOnlyThatRemote(t *testing.T) {
	t.Parallel()
	f, dir := forkedClone(t, testutil.OriginOpts{})
	ctx := context.Background()
	testutil.AdvanceOrigin(t, f.Upstream, "main", "u.txt", "u\n", "upstream commit")
	testutil.AdvanceOrigin(t, f.Fork, "main", "f.txt", "f\n", "fork commit")
	if err := (Net{}).FetchRemote(ctx, dir, "upstream"); err != nil {
		t.Fatal(err)
	}
	if got, want := testutil.Git(t, dir, "rev-parse", "refs/remotes/upstream/main"), testutil.Git(t, f.Upstream.Path, "rev-parse", "main"); got != want {
		t.Fatalf("upstream/main = %s, want %s", got, want)
	}
	if testutil.Git(t, dir, "rev-parse", "refs/remotes/origin/main") == testutil.Git(t, f.Fork.Path, "rev-parse", "main") {
		t.Fatal("origin was fetched too")
	}
	// Prunes: a branch deleted on the remote disappears.
	testutil.Git(t, f.Upstream.Path, "branch", "x", "main")
	if err := (Net{}).FetchRemote(ctx, dir, "upstream"); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, f.Upstream.Path, "branch", "-D", "x")
	if err := (Net{}).FetchRemote(ctx, dir, "upstream"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := refExists(ctx, dir, "refs/remotes/upstream/x"); ok {
		t.Fatal("upstream/x not pruned")
	}
}

func TestDefaultBranchOfAsksTheRemote(t *testing.T) {
	t.Parallel()
	_, dir := forkedClone(t, testutil.OriginOpts{DefaultBranch: "main"})
	testutil.Git(t, dir, "symbolic-ref", "--delete", "refs/remotes/upstream/HEAD")
	if def, err := (Net{}).DefaultBranchOf(context.Background(), dir, "upstream"); err != nil || def != "main" {
		t.Fatalf("DefaultBranchOf = %q, %v", def, err)
	}
	if _, err := (Net{}).DefaultBranchOf(context.Background(), dir, "nosuch"); !errors.Is(err, ErrNoDefaultBranch) {
		t.Fatalf("unknown remote: err = %v", err)
	}
}

func TestWorktreeAddSearchesRemotesInOrder(t *testing.T) {
	t.Parallel()
	f, dir := forkedClone(t, testutil.OriginOpts{Branches: []string{"both"}})
	ctx := context.Background()
	testutil.AdvanceOrigin(t, f.Fork, "main", "x.txt", "x\n", "x")
	testutil.Git(t, f.Fork.Path, "branch", "on-fork", "main")
	testutil.Git(t, f.Upstream.Path, "branch", "on-upstream", "main")
	for _, r := range []string{"origin", "upstream"} {
		if err := (Net{}).FetchRemote(ctx, dir, r); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Dir(dir)
	opts := WorktreeAddOpts{Base: "upstream/main", Remotes: ForkRemotes.BranchSearch()}
	for branch, want := range map[string]string{"on-fork": "origin/on-fork", "on-upstream": "upstream/on-upstream", "both": "origin/both"} {
		res := mustAdd(t, dir, filepath.Join(repo, branch), branch, opts, WorktreeRemoteBranch)
		if res.Upstream != want {
			t.Fatalf("%s: upstream %q, want %q", branch, res.Upstream, want)
		}
	}
	res := mustAdd(t, dir, filepath.Join(repo, "new"), "new", opts, WorktreeNewBranch)
	if res.Upstream != "" {
		t.Fatalf("new branch has upstream %q", res.Upstream)
	}
	if got, want := testutil.Git(t, dir, "rev-parse", "new"), testutil.Git(t, dir, "rev-parse", "upstream/main"); got != want {
		t.Fatalf("new branch at %s, want upstream/main %s", got, want)
	}
}

func TestFetchTagsFrom(t *testing.T) {
	t.Parallel()
	f, dir := forkedClone(t, testutil.OriginOpts{})
	oid := testutil.TagOriginDangling(t, f.Upstream, "v1", "main")
	kept, err := Net{}.FetchTagsFrom(context.Background(), dir, "upstream")
	if err != nil || kept != nil {
		t.Fatalf("FetchTagsFrom = %v, %v", kept, err)
	}
	if got, err := TagCommit(context.Background(), dir, "v1"); err != nil || got != oid {
		t.Fatalf("v1 = %s, %v; want %s", got, err, oid)
	}
}

func TestDeletionRisksUntrustedRemotes(t *testing.T) {
	t.Parallel()
	_, dir := forkedClone(t, testutil.OriginOpts{})
	ctx := context.Background()
	testutil.Git(t, dir, "switch", "-q", "-c", "topic")
	testutil.CommitFile(t, dir, "t.txt", "t\n", "on the fork only")
	testutil.Git(t, dir, "push", "-q", "origin", "topic")
	testutil.Git(t, dir, "switch", "-q", "--detach", "topic")
	testutil.Git(t, dir, "branch", "-q", "-D", "topic")
	count := func(rs []Risk, k RiskKind) int {
		for _, r := range rs {
			if r.Kind == k {
				return r.Count
			}
		}
		return 0
	}
	trusted, err := DeletionRisksWithOptions(ctx, dir, DeletionRisksOptions{})
	if err != nil || count(trusted, RiskDetachedCommits) != 0 {
		t.Fatalf("trusted: %v, %v", trusted, err)
	}
	untrusted, err := DeletionRisksWithOptions(ctx, dir, DeletionRisksOptions{UntrustedRemotes: []string{"origin"}})
	if err != nil || count(untrusted, RiskDetachedCommits) != 1 {
		t.Fatalf("untrusted: %v, %v", untrusted, err)
	}
	testutil.Git(t, dir, "switch", "-q", "-c", "again")
	untrusted, err = DeletionRisksWithOptions(ctx, dir, DeletionRisksOptions{UntrustedRemotes: []string{"origin"}})
	if err != nil || count(untrusted, RiskUnpushedBranchCommits) != 1 {
		t.Fatalf("untrusted branch: %v, %v", untrusted, err)
	}
}

func TestDeletionRisksTrustedRemotes(t *testing.T) {
	t.Parallel()
	_, dir := forkedClone(t, testutil.OriginOpts{})
	ctx := context.Background()
	testutil.Git(t, dir, "switch", "-q", "-c", "topic")
	testutil.CommitFile(t, dir, "t.txt", "t\n", "on the fork only")
	testutil.Git(t, dir, "push", "-q", "origin", "topic")
	unpushed := func(opts DeletionRisksOptions) (int, []string) {
		t.Helper()
		rs, err := DeletionRisksWithOptions(ctx, dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		names, err := UnpushedBranches(ctx, dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.Kind == RiskUnpushedBranchCommits {
				return r.Count, names
			}
		}
		return 0, names
	}
	for _, c := range []struct {
		opts DeletionRisksOptions
		want int
	}{
		{DeletionRisksOptions{TrustedRemotes: []string{"upstream"}}, 1},
		{DeletionRisksOptions{TrustedRemotes: []string{"upstream", "origin"}}, 0},
		{DeletionRisksOptions{TrustedRemotes: []string{"origin"}}, 0},
		{DeletionRisksOptions{TrustedRemotes: []string{"upstream", "origin"}, UntrustedRemotes: []string{"origin"}}, 1},
		// A remote whose name is a prefix of another's trusts only its own refs.
		{DeletionRisksOptions{TrustedRemotes: []string{"upstream", "orig"}}, 1},
	} {
		n, names := unpushed(c.opts)
		if n != c.want || (n > 0) != slices.Equal(names, []string{"topic"}) {
			t.Errorf("%+v: %d unpushed commits on %v, want %d", c.opts, n, names, c.want)
		}
	}
}

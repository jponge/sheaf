package gitx

import (
	"context"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// Refusals name the branches that carry unpushed commits (not only
// the checked-out one).
func TestUnpushedBranches(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{Branches: []string{"pushed"}})
	testutil.Git(t, dir, "branch", "--track", "pushed", "origin/pushed")
	testutil.Git(t, dir, "switch", "-q", "-c", "topic")
	testutil.CommitFile(t, dir, "t.txt", "t\n", "topic work")
	testutil.Git(t, dir, "branch", "also", "topic")
	testutil.Git(t, dir, "branch", "behind", "topic~1") // the pushed parent
	testutil.Git(t, dir, "switch", "-q", "-c", "pr/7", "main")
	testutil.CommitFile(t, dir, "p.txt", "p\n", "pr head")
	testutil.Git(t, dir, "update-ref", "refs/sheaf/pull/7", "HEAD")
	testutil.Git(t, dir, "switch", "-q", "main")
	testutil.Git(t, dir, "tag", "local-tag", "topic") // tags do not carry
	got := func(opts DeletionRisksOptions) []string {
		t.Helper()
		opts.TrustFetchAllRemotes = true // every remote is fetched in this test
		names, err := UnpushedBranches(context.Background(), dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	if g := got(DeletionRisksOptions{}); !slices.Equal(g, []string{"also", "pr/7", "topic"}) {
		t.Errorf("UnpushedBranches = %q", g)
	}
	if g := got(DeletionRisksOptions{PublishedRefs: []string{"refs/sheaf/pull/7", "refs/sheaf/pull/99"}}); !slices.Equal(g, []string{"also", "topic"}) {
		t.Errorf("with the published pr head: %q", g)
	}
	// Commits only on an untrusted remote's refs are unpushed.
	testutil.Git(t, dir, "push", "-q", "origin", "topic:topic")
	testutil.Git(t, dir, "fetch", "-q", "origin")
	if g := got(DeletionRisksOptions{PublishedRefs: []string{"refs/sheaf/pull/7"}}); g != nil {
		t.Errorf("all pushed: %q", g)
	}
	if g := got(DeletionRisksOptions{UntrustedRemotes: []string{"origin"}}); len(g) != 6 {
		t.Errorf("untrusted origin: %q, want every branch", g)
	}
}

func TestRiskStringPluralization(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		r    Risk
		want string
	}{
		{Risk{Kind: RiskStaged, Count: 1}, "1 staged change"},
		{Risk{Kind: RiskUnstaged, Count: 1}, "1 unstaged change"},
		{Risk{Kind: RiskConflicted, Count: 1}, "1 conflict"},
		{Risk{Kind: RiskUntracked, Count: 1}, "1 untracked file"},
		{Risk{Kind: RiskUntracked, Count: 2}, "2 untracked files"},
		{Risk{Kind: RiskStash, Count: 1}, "1 stash"},
		{Risk{Kind: RiskUnpushedBranchCommits, Count: 2, Detail: "a, b"}, "2 unpushed commits on local branches: a, b"},
		{Risk{Kind: RiskDetachedCommits, Count: 1}, "1 commit only on detached HEAD"},
		{Risk{Kind: RiskTagOnlyCommits, Count: 1}, "1 commit only reachable from local tags"},
		{Risk{Kind: RiskIgnoredFiles, Count: 1}, "1 ignored file"},
		{Risk{Kind: RiskHiddenModifications, Count: 1}, "1 hidden modification (skip-worktree or assume-unchanged file)"},
		{Risk{Kind: RiskUnverifiable, Count: 1, Detail: "x"}, "1 file that could not be verified: x"},
		{Risk{Kind: RiskNestedRepository, Count: 1, Detail: "v (no remote)"}, "1 nested repository: v (no remote)"},
		{Risk{Kind: RiskNestedRepositoryClean, Count: 1, Detail: "v"}, "1 clean, fully pushed nested repository: v"},
	} {
		if got := tc.r.String(); got != tc.want {
			t.Errorf("%+v: String = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// PublishedRefs count as pushed (e.g. a recorded pull request head): only
// the commits beyond them are at risk; a missing ref is ignored, and
// without the option they do not count.
func TestDeletionRisksPublishedRefs(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "switch", "-q", "-c", "pr/7")
	testutil.CommitFile(t, dir, "p1.txt", "1\n", "pr head")
	testutil.Git(t, dir, "update-ref", "refs/sheaf/pull/7", "HEAD")
	testutil.CommitFile(t, dir, "p2.txt", "2\n", "local work on the pr")
	testutil.Git(t, dir, "switch", "-q", "main")
	risks := func(refs ...string) []Risk {
		t.Helper()
		r, err := DeletionRisksWithOptions(context.Background(), dir, DeletionRisksOptions{TrustFetchAllRemotes: true, PublishedRefs: refs})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	wantRisks(t, risks("refs/sheaf/pull/7", "refs/sheaf/pull/99"), map[RiskKind]int{RiskUnpushedBranchCommits: 1})
	wantRisks(t, risks(), map[RiskKind]int{RiskUnpushedBranchCommits: 2})
	testutil.Git(t, dir, "update-ref", "refs/sheaf/pull/7", "pr/7")
	wantRisks(t, risks("refs/sheaf/pull/7"), nil)
	// A detached HEAD on the published head is not at risk either.
	testutil.Git(t, dir, "switch", "-q", "--detach", "refs/sheaf/pull/7")
	testutil.Git(t, dir, "branch", "-q", "-D", "pr/7")
	wantRisks(t, risks("refs/sheaf/pull/7"), nil)
}

// Only branches tracking refs/pull/<n>/head (not /merge) from the given
// remote are pull request branches; each number once.
func TestPullRequestBranches(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	for _, b := range []struct{ name, remote, merge string }{
		{"pr/5", "origin", "refs/pull/5/head"},
		{"pr5-again", "origin", "refs/pull/5/head"},
		{"pr/9", "origin", "refs/pull/9/head"},
		{"other/6", "elsewhere", "refs/pull/6/head"},
		{"merge/7", "origin", "refs/pull/7/merge"},
		{"plain", "origin", "refs/heads/main"},
	} {
		testutil.Git(t, dir, "branch", b.name, "main")
		testutil.Git(t, dir, "config", "branch."+b.name+".remote", b.remote)
		testutil.Git(t, dir, "config", "branch."+b.name+".merge", b.merge)
	}
	ns, err := PullRequestBranches(context.Background(), dir, "origin")
	if err != nil || !slices.Equal(ns, []int{5, 9}) {
		t.Fatalf("PullRequestBranches = %v, %v; want [5 9]", ns, err)
	}
}

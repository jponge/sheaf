package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// addForked adds up/<name> with the user's fork me/<name> through Add and
// returns the fixture repositories and the primary checkout.
func (f *fixture) addForked(t *testing.T, name string, opts testutil.OriginOpts) (testutil.ForkedOrigin, string) {
	t.Helper()
	fo := testutil.NewForkedOrigin(t, opts)
	def := opts.DefaultBranch
	if def == "" {
		def = "main"
	}
	f.fake.User = forkUser
	f.fake.AddRepo(hostingRepoBranch("up", name, fo.Upstream.URL, def))
	f.fake.AddFork(hostingFork(name, fo.Fork.URL))
	s, err := f.add(t, AddRequest{Slugs: []string{"up/" + name}})
	if err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	return fo, filepath.Join(f.w.Root, name, def)
}

func head(t *testing.T, dir, rev string) string {
	t.Helper()
	return testutil.Git(t, dir, "rev-parse", rev)
}

func TestUpdateForkFetchesBothRemotesAndFollowsUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{Branches: []string{"feat"}})
	if _, err := AddWorktree(context.Background(), f.w, "x", "feat", WorktreeAddOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	testutil.AdvanceOrigin(t, fo.Upstream, "main", "u.txt", "u\n", "upstream main")
	testutil.AdvanceOrigin(t, fo.Fork, "main", "fm.txt", "fm\n", "fork main (must not be followed)")
	testutil.AdvanceOrigin(t, fo.Fork, "feat", "ff.txt", "ff\n", "fork feat")
	rep, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 2}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	got := coUpdatesByName(rep)
	if u := got["x/main"]; u.Outcome != gitx.PullFastForwarded {
		t.Fatalf("primary: %+v", u)
	}
	if h, want := head(t, primary, "HEAD"), head(t, fo.Upstream.Path, "main"); h != want {
		t.Fatalf("primary at %s, want upstream main %s", h, want)
	}
	if u := got["x/feat"]; u.Outcome != gitx.PullFastForwarded {
		t.Fatalf("feat: %+v", u)
	}
	if h, want := head(t, filepath.Join(f.w.Root, "x", "feat"), "HEAD"), head(t, fo.Fork.Path, "feat"); h != want {
		t.Fatalf("feat at %s, want fork feat %s", h, want)
	}
}

func TestFetchReposAndStatusFetchForkFetchBothRemotes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	plain := f.repo(t, "up", "y", "main", false)
	if s, err := f.add(t, AddRequest{Slugs: []string{"up/y"}}); err != nil || s.Err() != nil {
		t.Fatal(err)
	}
	_ = plain
	testutil.AdvanceOrigin(t, fo.Upstream, "main", "u.txt", "u\n", "u")
	testutil.AdvanceOrigin(t, fo.Fork, "main", "f.txt", "f\n", "f")
	s, err := FetchRepos(context.Background(), f.w, FetchOptions{Jobs: 2}, Discard)
	if err != nil || s.Err() != nil {
		t.Fatalf("%+v, %v", s, err)
	}
	msgs := map[string]string{}
	for _, r := range s.Results {
		msgs[r.Repo] = r.Message
	}
	if msgs["x"] != "fetched upstream and origin" || msgs["y"] != "fetched origin" {
		t.Fatalf("messages = %v", msgs)
	}
	for remote, o := range map[string]testutil.Origin{"upstream": fo.Upstream, "origin": fo.Fork} {
		if head(t, primary, "refs/remotes/"+remote+"/main") != head(t, o.Path, "main") {
			t.Fatalf("%s/main not fetched", remote)
		}
	}
	testutil.AdvanceOrigin(t, fo.Upstream, "main", "u2.txt", "u\n", "u2")
	st, err := CollectStatus(context.Background(), f.w, StatusOptions{Fetch: true, Jobs: 1}, Discard)
	if err != nil || len(st.Problems) != 0 {
		t.Fatalf("%+v, %v", st.Problems, err)
	}
	for _, c := range st.Checkouts {
		if c.Repo == "x" && (c.Status.Upstream != "upstream/main" || c.Status.Behind != 2) {
			t.Fatalf("x status: upstream %q behind %d", c.Status.Upstream, c.Status.Behind)
		}
	}
}

// User decision: when only the fork fetch fails, the checkouts whose
// upstream remote was fetched are still updated; those tracking the fork
// fail with the fetch error.
func TestUpdateForkFailedForkFetchUpdatesWhatWasFetched(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{Branches: []string{"feat"}})
	if _, err := AddWorktree(context.Background(), f.w, "x", "feat", WorktreeAddOptions{}, noEmit); err != nil {
		t.Fatal(err)
	}
	testutil.AdvanceOrigin(t, fo.Upstream, "main", "u.txt", "u\n", "upstream main")
	testutil.Git(t, primary, "remote", "set-url", "origin", "file:///nonexistent/fork.git")
	var fetchFailed bool
	rep, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 1}, ReporterFunc(func(e Event) {
		fetchFailed = fetchFailed || e.Phase == PhaseFetch && e.Kind == KindFailed
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := coUpdatesByName(rep)
	if u := got["x/main"]; u.Outcome != gitx.PullFastForwarded || head(t, primary, "HEAD") != head(t, fo.Upstream.Path, "main") {
		t.Fatalf("main (tracks upstream) must be fast-forwarded: %+v", u)
	}
	if u := got["x/feat"]; u.Outcome != gitx.PullFailed || u.Err == nil || !strings.Contains(u.Err.Error(), "origin") {
		t.Fatalf("feat (tracks the fork) must fail with the fetch error: %+v", u)
	}
	if !fetchFailed {
		t.Fatal("no failed fetch event")
	}
	if len(rep.Problems) != 1 || rep.Problems[0].Repo != "x" || !strings.Contains(rep.Problems[0].Error(), "origin") {
		t.Fatalf("the failed fork fetch must be a problem (non-zero exit): %+v", rep.Problems)
	}
}

func TestAddWorktreeForkNewBranchStartsFromUpstreamDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	testutil.AdvanceOrigin(t, fo.Upstream, "main", "u.txt", "u\n", "upstream main")
	testutil.AdvanceOrigin(t, fo.Fork, "main", "f.txt", "f\n", "stale fork main")
	res, evs, err := f.addWT(t, "x", "topic", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != gitx.WorktreeNewBranch || res.Upstream != "" || res.Base != "upstream/main" {
		t.Fatalf("result = %+v", res)
	}
	if h, want := head(t, primary, "refs/heads/topic"), head(t, fo.Upstream.Path, "main"); h != want {
		t.Fatalf("topic at %s, want upstream main %s", h, want)
	}
	if !strings.Contains(messages(evs), "fetching upstream and origin") {
		t.Fatalf("events: %s", messages(evs))
	}
}

func TestAddWorktreeForkFindsRemoteBranchesForkFirst(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{Branches: []string{"both"}})
	// Created after the clone: AddWorktree must fetch both remotes.
	testutil.Git(t, fo.Fork.Path, "branch", "on-fork", "main")
	testutil.Git(t, fo.Upstream.Path, "branch", "on-upstream", "main")
	for branch, want := range map[string]string{"on-fork": "origin/on-fork", "on-upstream": "upstream/on-upstream", "both": "origin/both"} {
		res, _, err := f.addWT(t, "x", branch, WorktreeAddOptions{})
		if err != nil {
			t.Fatalf("%s: %v", branch, err)
		}
		if res.Mode != gitx.WorktreeRemoteBranch || res.Upstream != want {
			t.Fatalf("%s: %+v, want upstream %s", branch, res, want)
		}
	}
}

func TestAddWorktreeForkLocalBranchTrackingEitherRemoteIsNotWarned(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{Branches: []string{"feat"}})
	_ = fo
	testutil.Git(t, primary, "branch", "--track", "feat", "upstream/feat")
	res, _, err := f.addWT(t, "x", "feat", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != gitx.WorktreeLocalBranch || len(res.Warnings) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestAddWorktreeForkRefusesTagOnlyNameFromUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	testutil.TagOriginDangling(t, fo.Upstream, "v9", "main")
	if _, _, err := f.addWT(t, "x", "v9", WorktreeAddOptions{}); err == nil || !strings.Contains(err.Error(), "--tag") {
		t.Fatalf("err = %v", err)
	}
}

func TestAddTagWorktreeForkFetchesUpstreamTags(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	oid := testutil.TagOriginDangling(t, fo.Upstream, "v1.0", "main")
	res, _, err := f.addTag(t, "x", "v1.0", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Head != oid {
		t.Fatalf("head %s, want %s", res.Head, oid)
	}
}

// forkTopic creates, in a forked repository added with addForked, a
// worktree of the new branch topic with one commit pushed to the fork
// (tracking origin/topic), then merges it into the upstream's main with a
// merge commit, as a pull request from the fork would be. The fork's main
// stays behind.
func (f *fixture) forkTopic(t *testing.T, fo testutil.ForkedOrigin) string {
	t.Helper()
	res, _, err := f.addWT(t, "x", "topic", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	testutil.CommitFile(t, res.Path, "topic.txt", "topic\n", "work on topic")
	testutil.Git(t, res.Path, "push", "-q", "-u", "origin", "topic")
	work := originWork(t, fo.Upstream)
	testutil.Git(t, work, "fetch", "-q", fo.Fork.Path, "topic")
	testutil.Git(t, work, "merge", "-q", "--no-ff", "-m", "merge topic from the fork", "FETCH_HEAD")
	testutil.Git(t, work, "push", "-q", "origin", "main")
	return res.Path
}

func TestPruneForkMergedIntoUpstreamDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkTopic(t, fo)
	plan := f.prunePlan(t, PruneOptions{})
	if got := candidateNames(plan); len(got) != 1 || got[0] != "x/topic:wt:merged" {
		t.Fatalf("candidates = %v (skipped %+v, problems %v)", got, plan.Skipped, plan.Problems)
	}
	if !strings.Contains(plan.Candidates[0].Reason, "merged into upstream/main") {
		t.Fatalf("reason = %q", plan.Candidates[0].Reason)
	}
	// The branch deleted on the fork after the merge: gone (per its own
	// upstream, origin/topic) and merged.
	testutil.Git(t, fo.Fork.Path, "branch", "-q", "-D", "topic")
	plan = f.prunePlan(t, PruneOptions{})
	if got := candidateNames(plan); len(got) != 1 || got[0] != "x/topic:wt:gone+merged" {
		t.Fatalf("candidates = %v", got)
	}
}

func TestRemoveWorktreeDeleteBranchForkMergedIntoUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	f.forkTopic(t, fo)
	testutil.Git(t, fo.Fork.Path, "branch", "-q", "-D", "topic")
	msg, err := f.rmWT(t, "x", "topic", WorktreeRemoveOptions{DeleteBranch: true, WorkingDir: f.w.Root, Confirm: func(WorktreeRemovePlan) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "merged into upstream/main") {
		t.Fatalf("msg = %q", msg)
	}
	if branchExists(t, primary, "topic") {
		t.Fatal("topic not deleted")
	}
}

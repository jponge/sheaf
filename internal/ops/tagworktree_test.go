package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

func (f *fixture) addTag(t *testing.T, repo, tag string, opts WorktreeAddOptions) (WorktreeAddResult, []Event, error) {
	t.Helper()
	var evs []Event
	res, err := AddTagWorktree(context.Background(), f.w, repo, tag, opts, func(e Event) { evs = append(evs, e) })
	return res, evs, err
}

func (f *fixture) mustAddTag(t *testing.T, repo, tag string) WorktreeAddResult {
	t.Helper()
	res, _, err := f.addTag(t, repo, tag, WorktreeAddOptions{NoFetch: true})
	if err != nil {
		t.Fatalf("add tag worktree %s %s: %v", repo, tag, err)
	}
	return res
}

func infoByDir(t *testing.T, infos []WorktreeInfo, dir string) WorktreeInfo {
	t.Helper()
	for _, in := range infos {
		if in.Dir == dir {
			return in
		}
	}
	t.Fatalf("no checkout with directory %q in %+v", dir, infos)
	return WorktreeInfo{}
}

func TestAddTagWorktreeFetchesTagsAndPins(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	root := f.w.Root
	// Created after the clone: only a fetch of the tags brings it.
	testutil.TagOrigin(t, o, "release/1.0", "feat", true)
	featOID := testutil.Git(t, o.Path, "rev-parse", "feat")

	if _, _, err := f.addTag(t, "a", "release/1.0", WorktreeAddOptions{NoFetch: true}); err == nil || !strings.Contains(err.Error(), "no tag") || !strings.Contains(err.Error(), "--no-fetch") {
		t.Fatalf("--no-fetch with a tag only on origin: err = %v, want a not-found error mentioning --no-fetch", err)
	}
	res, evs, err := f.addTag(t, "a", "release/1.0", WorktreeAddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "a", "release-1.0")
	if res.Path != want || res.Dir != "release-1.0" || res.Tag != "release/1.0" || res.Head != featOID || res.Branch != "" {
		t.Fatalf("result = %+v, want %s at %s", res, want, featOID)
	}
	if !strings.Contains(messages(evs), "fetching origin") {
		t.Errorf("no fetch reported:\n%s", messages(evs))
	}
	if head := testutil.Git(t, want, "rev-parse", "HEAD"); head != featOID {
		t.Errorf("HEAD = %s, want %s", head, featOID)
	}
	if b := testutil.Git(t, want, "branch", "--show-current"); b != "" {
		t.Errorf("worktree is on branch %q, want detached", b)
	}
	infos, _, err := ListWorktrees(context.Background(), f.w, "a")
	if err != nil {
		t.Fatal(err)
	}
	in := infoByDir(t, infos, "release-1.0")
	if in.Tag != "release/1.0" || !in.Detached || in.Branch != "" {
		t.Errorf("listed as %+v", in)
	}
	if p := infoByDir(t, infos, "main"); p.Tag != "" {
		t.Errorf("primary listed with tag %q", p.Tag)
	}
	_ = primary
}

func TestAddTagWorktreeLightweightLocalTag(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v0", "origin/feat") // local only, lightweight
	res := f.mustAddTag(t, "a", "v0")
	if want := testutil.Git(t, primary, "rev-parse", "origin/feat"); res.Head != want {
		t.Fatalf("Head = %s, want %s", res.Head, want)
	}
}

func TestAddTagWorktreeRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	root := f.w.Root
	// No tags differing only by case: they would collide on case-insensitive
	// file systems (loose refs).
	for _, tag := range []string{"feat", "Main", "junk", "v1"} {
		testutil.Git(t, primary, "tag", tag)
	}
	f.mustAddWT(t, "a", "feat")
	f.mustAddTag(t, "a", "v1")
	if err := os.Mkdir(filepath.Join(root, "a", "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ tag, want string }{
		{"feat", "directory"},
		{"FEAT", "directory"},
		{"Main", "primary"},
		{"junk", "not a worktree"},
		{"v1", "already"},
		{"V1", "directory"},
		{"a..b", "not a valid tag name"},
		{"-x", "not a valid tag name"},
		{"x.lock", "not a valid tag name"},
		{"has space", "not a valid tag name"},
		{"", "not a valid tag name"},
		{"nope", "no tag"},
	} {
		_, _, err := f.addTag(t, "a", tc.tag, WorktreeAddOptions{NoFetch: true})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("tag %q: err = %v, want it to contain %q", tc.tag, err, tc.want)
		}
	}
	// A branch whose directory is taken by a tag worktree is refused too.
	if _, _, err := f.addWT(t, "a", "V1", WorktreeAddOptions{NoFetch: true}); err == nil || !strings.Contains(err.Error(), "pinned at tag v1") {
		t.Errorf("branch V1 (dir collides with the tag worktree v1): err = %v", err)
	}
	if n := len(mustListInfos(t, f, "a")); n != 3 {
		t.Errorf("refusals left %d checkouts, want 3 (main, feat, v1)", n)
	}
}

func mustListInfos(t *testing.T, f *fixture, repo string) []WorktreeInfo {
	t.Helper()
	infos, fails, err := ListWorktrees(context.Background(), f.w, repo)
	if err != nil || len(fails) > 0 {
		t.Fatalf("ListWorktrees: %v %v", err, fails)
	}
	return infos
}

// `wt add <name>` must not silently create a branch named like an existing
// tag.
func TestAddWorktreeRefusesATagName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	testutil.TagOrigin(t, o, "v2", "main", true)
	testutil.Git(t, primary, "tag", "v1")
	testutil.Git(t, primary, "tag", "feat", "origin/main") // also a remote branch
	for _, tc := range []struct {
		name string
		opts WorktreeAddOptions
	}{
		{"v1", WorktreeAddOptions{NoFetch: true}},
		{"v1", WorktreeAddOptions{NoFetch: true, Base: "origin/main"}},
		{"v2", WorktreeAddOptions{}}, // known only after the fetch
	} {
		_, _, err := f.addWT(t, "a", tc.name, tc.opts)
		want := tc.name + " is a tag; use --tag " + tc.name + " for a detached worktree at that tag"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("add %s %+v: err = %v, want %q", tc.name, tc.opts, err, want)
		}
		if branchExists(t, primary, tc.name) {
			t.Errorf("branch %s was created", tc.name)
		}
	}
	// A tag on origin that git's automatic tag following does not bring (no
	// branch contains its commit) is found too: tags are fetched for the check.
	testutil.TagOriginDangling(t, o, "hotfix-1", "main")
	if _, _, err := f.addWT(t, "a", "hotfix-1", WorktreeAddOptions{}); err == nil || !strings.Contains(err.Error(), "hotfix-1 is a tag; use --tag hotfix-1") {
		t.Errorf("add hotfix-1 (tag only on origin, outside branches): err = %v", err)
	}
	if branchExists(t, primary, "hotfix-1") {
		t.Error("branch hotfix-1 was created")
	}
	// A tag that is also a branch (here on origin) does not prevent the
	// branch worktree.
	if res := f.mustAddWT(t, "a", "feat"); res.Mode != gitx.WorktreeRemoteBranch {
		t.Errorf("feat: mode %v", res.Mode)
	}
	// Nor does a tag that is a local branch.
	testutil.Git(t, primary, "branch", "both")
	testutil.Git(t, primary, "tag", "both")
	if res := f.mustAddWT(t, "a", "both"); res.Mode != gitx.WorktreeLocalBranch {
		t.Errorf("both: mode %v", res.Mode)
	}
}

func TestTagWorktreeStatusUpdatePrune(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v1", "origin/main")
	tw := f.mustAddTag(t, "a", "v1")
	before := testutil.Git(t, tw.Path, "rev-parse", "HEAD")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote main")
	// The tag moves on origin: the worktree stays where it is.
	testutil.Git(t, o.Path, "tag", "-f", "v1", "main")

	st, err := CollectStatus(context.Background(), f.w, StatusOptions{Jobs: 2}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range st.Checkouts {
		if c.Dir == "v1" {
			found = true
			if c.Tag != "v1" || !c.Detached {
				t.Errorf("status checkout = %+v, want tag v1", c.Checkout)
			}
		} else if c.Tag != "" {
			t.Errorf("%s has tag %q", c.Name(), c.Tag)
		}
	}
	if !found {
		t.Fatalf("v1 not in status: %+v", st.Checkouts)
	}

	r, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 2}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	u, ok := coUpdatesByName(r)["a/v1"]
	if !ok {
		t.Fatalf("a/v1 not updated: %+v", r.Checkouts)
	}
	if !u.Pinned || u.Skipped() || u.Outcome == gitx.PullFailed || u.Reason != "pinned at tag v1" {
		t.Errorf("update a/v1 = %+v, want pinned, not skipped", u)
	}
	if m := coUpdatesByName(r)["a/main"]; m.Pinned || m.Outcome != gitx.PullFastForwarded {
		t.Errorf("update a/main = %+v", m)
	}
	if after := testutil.Git(t, tw.Path, "rev-parse", "HEAD"); after != before {
		t.Errorf("tag worktree moved from %s to %s", before, after)
	}

	plan := f.prunePlan(t, PruneOptions{IncludeBranches: true})
	for _, c := range plan.Candidates {
		if c.Path == tw.Path {
			t.Errorf("tag worktree proposed: %+v", c)
		}
	}
	var skipped bool
	for _, s := range plan.Skipped {
		if s.Tag == "v1" && s.Path == tw.Path && strings.Contains(s.Reason, "pinned at tag v1") && s.Branch == "" {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("tag worktree not listed as skipped: %+v", plan.Skipped)
	}
}

func TestRemoveTagWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	for _, tag := range []string{"release/1.0", "v2", "v3"} {
		testutil.Git(t, primary, "tag", tag)
	}
	rel := f.mustAddTag(t, "a", "release/1.0")
	v2 := f.mustAddTag(t, "a", "v2")
	v3 := f.mustAddTag(t, "a", "v3")

	// -d / -D make no sense for a tag worktree.
	for _, o := range []WorktreeRemoveOptions{{DeleteBranch: true}, {ForceDeleteBranch: true}} {
		if _, err := f.rmWT(t, "a", "v2", o); err == nil || !strings.Contains(err.Error(), "pinned at tag v2") || !strings.Contains(err.Error(), "-d") {
			t.Errorf("rm -d/-D of a tag worktree: err = %v", err)
		}
	}
	if !exists(v2.Path) {
		t.Fatal("refused removal removed the worktree")
	}
	// By tag name (the directory differs).
	msg, err := f.rmWT(t, "a", "release/1.0", WorktreeRemoveOptions{Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if exists(rel.Path) || !strings.Contains(msg, "tag release/1.0") {
		t.Errorf("rm by tag: %q, exists=%v", msg, exists(rel.Path))
	}
	// The tag survives.
	testutil.Git(t, primary, "rev-parse", "--verify", "refs/tags/release/1.0")

	// Commits on the detached HEAD are at risk.
	testutil.CommitFile(t, v2.Path, "x.txt", "x\n", "detached work")
	_, err = f.rmWT(t, "a", "v2", WorktreeRemoveOptions{Offline: true})
	var ref *WorktreeRefusedError
	if !errors.As(err, &ref) || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("rm with detached commits: err = %v", err)
	}
	// Untracked files are at risk; by directory name.
	writeFile(t, filepath.Join(v3.Path, "u.txt"), "u\n")
	if _, err := f.rmWTT(t, "a", WorktreeTarget{Name: "v3", Kind: TargetDir}, WorktreeRemoveOptions{Offline: true}); !errors.As(err, &ref) {
		t.Fatalf("rm with untracked file: err = %v", err)
	}
	if _, err := f.rmWT(t, "a", "v3", WorktreeRemoveOptions{Offline: true, Force: true}); err != nil || exists(v3.Path) {
		t.Fatalf("rm --force: %v", err)
	}
	// --branch does not select tags.
	if _, err := f.rmWTT(t, "a", WorktreeTarget{Name: "v2", Kind: TargetBranch}, WorktreeRemoveOptions{Offline: true}); err == nil {
		t.Error("--branch v2 selected the tag worktree")
	}
}

func TestResolvePathOfTagWorktree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "release/2.0")
	tw := f.mustAddTag(t, "a", "release/2.0")
	for _, q := range []PathQuery{
		{Args: []string{"a", "release/2.0"}, Cwd: f.w.Root},
		{Args: []string{"a", "release-2.0"}, Cwd: f.w.Root},
		{Args: []string{"release/2.0"}, Cwd: primary},
		{Args: []string{"release/2.0"}, Repo: "a", Cwd: f.w.Root},
	} {
		p, err := ResolvePath(context.Background(), f.w, q)
		if err != nil || p != tw.Path {
			t.Errorf("ResolvePath(%+v) = %q, %v; want %s", q, p, err, tw.Path)
		}
	}
}

// Local tags differing from origin's are kept by the fetch: the worktree is
// still created (from the local tag when it is the requested one), with
// warnings.
func TestAddTagWorktreeKeptLocalTagsWarn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	testutil.TagOrigin(t, o, "v1", "main", true)
	testutil.TagOrigin(t, o, "v2", "main", true)
	testutil.Git(t, primary, "fetch", "-q", "--tags", "origin")
	localV1 := testutil.Git(t, primary, "rev-parse", "v1^{commit}")
	testutil.Git(t, o.Path, "tag", "-f", "-a", "-m", "moved", "v1", "feat")
	testutil.Git(t, o.Path, "tag", "-f", "-a", "-m", "moved", "v2", "feat")
	testutil.TagOrigin(t, o, "v3", "feat", true)

	res, evs, err := f.addTag(t, "a", "v3", WorktreeAddOptions{})
	if err != nil {
		t.Fatalf("add v3: %v", err)
	}
	want := "local tags v1, v2 differ from origin's and were kept"
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], want) || !strings.Contains(messages(evs), "warning: "+want) {
		t.Errorf("add v3: warnings %q, events:\n%s\nwant %q", res.Warnings, messages(evs), want)
	}

	res, _, err = f.addTag(t, "a", "v1", WorktreeAddOptions{})
	if err != nil {
		t.Fatalf("add v1: %v", err)
	}
	if res.Head != localV1 {
		t.Errorf("add v1: Head = %s, want the local tag's commit %s", res.Head, localV1)
	}
	want = "local tag v1 differs from origin's and was kept: the worktree is at the local tag (git fetch origin +refs/tags/v1:refs/tags/v1 takes origin's)"
	if !strings.Contains(strings.Join(res.Warnings, "\n"), want) {
		t.Errorf("add v1: warnings %q, want %q", res.Warnings, want)
	}
}

// A --base that does not resolve is reported in sheaf's words, with whether
// origin was fetched and the closest remote branches and tags (up to 5).
func TestAddWorktreeBaseNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"2.9", "2.X", "2.9.x-old", "feat"}})
	f.fake.AddRepo(hostingRepo("o", "a", o.URL))
	if s, err := f.add(t, AddRequest{Slugs: []string{"o/a"}}); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	primary := filepath.Join(f.w.Root, "a", "main")
	testutil.TagOrigin(t, o, "2.9.1", "main", true)
	testutil.TagOrigin(t, o, "v3", "main", true)

	_, _, err := f.addWT(t, "a", "fix", WorktreeAddOptions{Base: "origin/2.9.x"})
	want := "base origin/2.9.x not found in a (after fetching origin); closest: origin/2.9.x-old, origin/2.9, 2.9.1 (tag)"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	// Matching ignores case (an exact name in another case is not used here:
	// it would resolve on case-insensitive file systems with loose refs).
	_, _, err = f.addWT(t, "a", "fix", WorktreeAddOptions{Base: "origin/FEA", NoFetch: true})
	want = "base origin/FEA not found in a (not fetched: --no-fetch); closest: origin/feat"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	_, _, err = f.addWT(t, "a", "fix", WorktreeAddOptions{Base: "nothing-like-it", NoFetch: true})
	want = "base nothing-like-it not found in a (not fetched: --no-fetch)"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	if branchExists(t, primary, "fix") {
		t.Error("branch fix was created")
	}
	// Local branches are suggested too.
	testutil.Git(t, primary, "branch", "topic-x", "origin/main")
	_, _, err = f.addWT(t, "a", "fix", WorktreeAddOptions{Base: "topic", NoFetch: true})
	want = "base topic not found in a (not fetched: --no-fetch); closest: topic-x (local branch)"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	// At most 5 suggestions, closest first.
	for _, b := range []string{"rel-1", "rel-2", "rel-3", "rel-4", "rel-5", "rel-6"} {
		testutil.Git(t, primary, "update-ref", "refs/remotes/origin/"+b, "origin/main")
	}
	_, _, err = f.addWT(t, "a", "fix", WorktreeAddOptions{Base: "REL", NoFetch: true})
	want = "base REL not found in a (not fetched: --no-fetch); closest: origin/rel-1, origin/rel-2, origin/rel-3, origin/rel-4, origin/rel-5"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
}

// A tag worktree needs no fetch: it is reported as pinned even when the
// fetch of its repository fails.
func TestUpdateTagWorktreePinnedWithoutFetch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v1")
	f.mustAddTag(t, "a", "v1")
	testutil.Git(t, primary, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "missing.git"))
	r, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 1}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	m := coUpdatesByName(r)
	if u := m["a/v1"]; !u.Pinned || u.Outcome == gitx.PullFailed || u.Reason != "pinned at tag v1" {
		t.Errorf("fetch failed: a/v1 = %+v, want pinned", u)
	}
	if u := m["a/main"]; u.Outcome != gitx.PullFailed {
		t.Errorf("fetch failed: a/main = %+v, want failed", u)
	}
}

// A tag worktree whose HEAD moved is shown as such and updated like a
// detached worktree (skipped, never moved by sheaf).
func TestTagWorktreeMoved(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v1")
	tw := f.mustAddTag(t, "a", "v1")
	testutil.CommitFile(t, tw.Path, "x.txt", "x\n", "on detached HEAD")
	head := testutil.Git(t, tw.Path, "rev-parse", "HEAD")

	in := infoByDir(t, mustListInfos(t, f, "a"), "v1")
	if in.Tag != "v1" || !in.TagMoved || in.Head != head {
		t.Errorf("listed as %+v, want tag v1 moved to %s", in, head)
	}
	st, err := CollectStatus(context.Background(), f.w, StatusOptions{Jobs: 1}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range st.Checkouts {
		if c.Dir == "v1" && (c.Tag != "v1" || c.TagMovedTo != head) {
			t.Errorf("status checkout = %+v, want TagMovedTo %s", c.Checkout, head)
		}
	}
	r, err := Update(context.Background(), f.w, UpdateOptions{Jobs: 1}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	u := coUpdatesByName(r)["a/v1"]
	want := "detached (was pinned at tag v1; moved to " + head[:10] + ")"
	if u.Pinned || !u.Skipped() || u.Outcome != gitx.PullSkippedDetached || u.Reason != want {
		t.Errorf("update a/v1 = %+v, want skipped with reason %q", u, want)
	}
	if got := testutil.Git(t, tw.Path, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved by update: %s", got)
	}
}

// Ref-like prefixes on --tag are refused with a hint (they would create
// refs/tags/refs/tags/...-style lookups and directories).
func TestAddTagWorktreeRefusesRefPrefixes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	testutil.Git(t, primary, "tag", "v1")
	for _, tc := range []struct{ arg, want string }{
		{"refs/tags/v1", `tag "refs/tags/v1": drop the "refs/tags/" prefix (--tag v1)`},
		{"tags/v1", `tag "tags/v1": drop the "tags/" prefix (--tag v1)`},
		{"refs/heads/x", `tag "refs/heads/x": drop the "refs/heads/" prefix (--tag x)`},
	} {
		if _, _, err := f.addTag(t, "a", tc.arg, WorktreeAddOptions{NoFetch: true}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("--tag %s: err = %v, want %q", tc.arg, err, tc.want)
		}
	}
}

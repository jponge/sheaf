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

func TestWorktreeAddTagDetachedAndListedWithItsTag(t *testing.T) {
	t.Parallel()
	o, repo, primary := layout(t, testutil.OriginOpts{Branches: []string{"feat"}})
	ctx := context.Background()
	// Tags created after the clone exist locally only once fetched.
	testutil.TagOrigin(t, o, "v1", "main", true)       // annotated
	testutil.TagOrigin(t, o, "light", "feat", false)   // lightweight
	testutil.TagOrigin(t, o, "v1-alias", "main", true) // same commit as v1
	if _, err := (Net{}).FetchTagsFrom(ctx, primary, "origin"); err != nil {
		t.Fatal(err)
	}
	mainOID := testutil.Git(t, primary, "rev-parse", "origin/main")
	featOID := testutil.Git(t, primary, "rev-parse", "origin/feat")

	for _, tc := range []struct{ tag, dir, want string }{
		{"v1", "v1", mainOID},
		{"light", "light", featOID},
		{"v1-alias", "alias", mainOID},
	} {
		oid, err := WorktreeAddTag(ctx, primary, filepath.Join(repo, tc.dir), tc.tag)
		if err != nil {
			t.Fatalf("WorktreeAddTag(%s): %v", tc.tag, err)
		}
		if oid != tc.want {
			t.Errorf("WorktreeAddTag(%s) = %s, want the peeled commit %s", tc.tag, oid, tc.want)
		}
	}
	check := func(when string) {
		t.Helper()
		wts := mustList(t, primary)
		for _, tc := range []struct{ dir, tag, head string }{
			{"v1", "v1", mainOID}, {"light", "light", featOID}, {"alias", "v1-alias", mainOID},
		} {
			w := findWorktree(t, wts, filepath.Join(repo, tc.dir))
			if !w.Detached || w.Branch != "" || w.Tag != tc.tag || w.Head != tc.head {
				t.Errorf("%s: worktree %s = %+v, want detached at %s pinned to tag %s", when, tc.dir, w, tc.head, tc.tag)
			}
		}
		if p := findWorktree(t, wts, primary); p.Tag != "" {
			t.Errorf("%s: primary has tag %q", when, p.Tag)
		}
	}
	check("fresh")
	// The record survives the tag being moved or deleted (no network needed).
	testutil.Git(t, primary, "tag", "-f", "v1", "origin/feat")
	testutil.Git(t, primary, "tag", "-d", "light")
	check("tag moved and deleted")
	// The record follows `git worktree move` (it lives in the worktree's
	// administrative directory, which git keeps).
	testutil.Git(t, primary, "worktree", "move", filepath.Join(repo, "light"), filepath.Join(repo, "moved"))
	if w := findWorktree(t, mustList(t, primary), filepath.Join(repo, "moved")); w.Tag != "light" {
		t.Errorf("moved worktree: %+v, want tag light", w)
	}
	testutil.Git(t, primary, "worktree", "move", filepath.Join(repo, "moved"), filepath.Join(repo, "light"))
	// A worktree no longer detached is not pinned any more.
	testutil.Git(t, filepath.Join(repo, "alias"), "switch", "-q", "-c", "work")
	if w := findWorktree(t, mustList(t, primary), filepath.Join(repo, "alias")); w.Tag != "" || w.Branch != "work" {
		t.Errorf("switched to a branch: %+v, want no tag", w)
	}
	// A detached worktree created by hand at a tagged commit is not a tag
	// worktree.
	manual := filepath.Join(repo, "manual")
	testutil.Git(t, primary, "worktree", "add", "-q", "--detach", manual, "v1-alias")
	if w := findWorktree(t, mustList(t, primary), manual); w.Tag != "" {
		t.Errorf("manual detached worktree has tag %q", w.Tag)
	}
}

func TestWorktreeAddTagRefusals(t *testing.T) {
	t.Parallel()
	_, repo, primary := layout(t, testutil.OriginOpts{})
	ctx := context.Background()
	testutil.Git(t, primary, "tag", "v1")
	tree := testutil.Git(t, primary, "rev-parse", "HEAD^{tree}")
	testutil.Git(t, primary, "tag", "treetag", tree)

	if _, err := WorktreeAddTag(ctx, primary, filepath.Join(repo, "nope"), "nope"); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("missing tag: err = %v, want ErrTagNotFound", err)
	}
	// A branch of that name is not a tag.
	testutil.Git(t, primary, "branch", "onlybranch")
	if _, err := WorktreeAddTag(ctx, primary, filepath.Join(repo, "onlybranch"), "onlybranch"); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("branch name: err = %v, want ErrTagNotFound", err)
	}
	if _, err := WorktreeAddTag(ctx, primary, filepath.Join(repo, "treetag"), "treetag"); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Errorf("tag of a tree: err = %v, want a not-a-commit error", err)
	}
	occupied := filepath.Join(repo, "occupied")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WorktreeAddTag(ctx, primary, occupied, "v1"); !errors.Is(err, ErrWorktreePathOccupied) {
		t.Errorf("occupied: err = %v", err)
	}
	if n := len(mustList(t, primary)); n != 1 {
		t.Errorf("refusals created worktrees: %d listed", n)
	}
}

// FetchTags must never delete local tags, even with fetch.pruneTags set
// (git fetch --prune would then delete tags missing on the remote).
func TestFetchTagsKeepsLocalTags(t *testing.T) {
	t.Parallel()
	o, _, primary := layout(t, testutil.OriginOpts{})
	testutil.Git(t, primary, "tag", "local-only")
	testutil.Git(t, primary, "config", "fetch.prune", "true")
	testutil.Git(t, primary, "config", "fetch.pruneTags", "true")
	testutil.TagOrigin(t, o, "remote-new", "main", true)
	if _, err := (Net{}).FetchTagsFrom(context.Background(), primary, "origin"); err != nil {
		t.Fatal(err)
	}
	tags := testutil.Git(t, primary, "tag", "--list")
	if !strings.Contains(tags, "local-only") || !strings.Contains(tags, "remote-new") {
		t.Fatalf("tags after FetchTags:\n%s", tags)
	}
}

// FetchTags fetches every tag of origin, also those on commits no branch
// contains (which git's automatic tag following does not bring).
func TestFetchTagsFetchesTagsOutsideBranches(t *testing.T) {
	t.Parallel()
	o, _, primary := layout(t, testutil.OriginOpts{})
	oid := testutil.TagOriginDangling(t, o, "hotfix-1", "main")
	if _, err := (Net{}).FetchTagsFrom(context.Background(), primary, "origin"); err != nil {
		t.Fatal(err)
	}
	if got, err := TagCommit(context.Background(), primary, "hotfix-1"); err != nil || got != oid {
		t.Fatalf("TagCommit(hotfix-1) = %q, %v; want %s", got, err, oid)
	}
}

// A local tag that differs from origin's tag of the same name is kept (git
// refuses to clobber it): FetchTags reports it instead of failing, and the
// rest of the fetch is applied. A user's fetch.output=compact must not
// break the parsing.
func TestFetchTagsReportsKeptLocalTags(t *testing.T) {
	t.Parallel()
	o, _, primary := layout(t, testutil.OriginOpts{Branches: []string{"feat"}})
	testutil.TagOrigin(t, o, "v1", "main", true)
	testutil.TagOrigin(t, o, "release/2.0", "main", false)
	if _, err := (Net{}).FetchTagsFrom(context.Background(), primary, "origin"); err != nil {
		t.Fatal(err)
	}
	localV1 := testutil.Git(t, primary, "rev-parse", "v1")
	localRel := testutil.Git(t, primary, "rev-parse", "release/2.0")
	// Both tags are moved on origin, origin/main advances, a new tag appears.
	testutil.Git(t, o.Path, "tag", "-f", "-a", "-m", "moved", "v1", "feat")
	testutil.Git(t, o.Path, "tag", "-f", "release/2.0", "feat")
	testutil.AdvanceOrigin(t, o, "main", "n.txt", "n\n", "new on main")
	testutil.TagOrigin(t, o, "v2", "main", true)
	testutil.Git(t, primary, "config", "fetch.output", "compact")

	kept, err := Net{}.FetchTagsFrom(context.Background(), primary, "origin")
	if err != nil {
		t.Fatalf("FetchTags: %v", err)
	}
	if strings.Join(kept, ",") != "release/2.0,v1" {
		t.Errorf("kept = %q, want [release/2.0 v1]", kept)
	}
	if got := testutil.Git(t, primary, "rev-parse", "v1"); got != localV1 {
		t.Errorf("local v1 overwritten: %s, want %s", got, localV1)
	}
	if got := testutil.Git(t, primary, "rev-parse", "release/2.0"); got != localRel {
		t.Errorf("local release/2.0 overwritten: %s, want %s", got, localRel)
	}
	if got, want := testutil.Git(t, primary, "rev-parse", "origin/main"), testutil.Git(t, o.Path, "rev-parse", "main"); got != want {
		t.Errorf("origin/main = %s, want %s (rest of the fetch not applied)", got, want)
	}
	testutil.Git(t, primary, "rev-parse", "--verify", "refs/tags/v2")
}

// Other fetch failures stay errors.
func TestFetchTagsFailure(t *testing.T) {
	t.Parallel()
	_, _, primary := layout(t, testutil.OriginOpts{})
	testutil.Git(t, primary, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "missing.git"))
	if kept, err := (Net{}).FetchTagsFrom(context.Background(), primary, "origin"); err == nil || len(kept) != 0 {
		t.Fatalf("FetchTags from a missing origin = %v, %v; want an error", kept, err)
	}
}

// A start point that does not resolve is ErrBaseNotFound; nothing is
// created.
func TestWorktreeAddBaseNotFound(t *testing.T) {
	t.Parallel()
	_, repo, primary := layout(t, testutil.OriginOpts{})
	_, err := WorktreeAdd(context.Background(), primary, filepath.Join(repo, "x"), "x", WorktreeAddOpts{Base: "origin/nope"})
	if !errors.Is(err, ErrBaseNotFound) || !strings.Contains(err.Error(), "origin/nope") {
		t.Fatalf("err = %v, want ErrBaseNotFound naming origin/nope", err)
	}
	if n := len(mustList(t, primary)); n != 1 {
		t.Errorf("%d worktrees listed", n)
	}
	if _, err := Run(context.Background(), primary, "rev-parse", "--verify", "--quiet", "refs/heads/x"); err == nil {
		t.Error("branch x created")
	}
}

// A refused tag among other failures is a failure.
func TestFetchTagsClobberAndOtherFailure(t *testing.T) {
	t.Parallel()
	o, _, primary := layout(t, testutil.OriginOpts{})
	testutil.TagOrigin(t, o, "v1", "main", true)
	if _, err := (Net{}).FetchTagsFrom(context.Background(), primary, "origin"); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, o.Path, "tag", "-f", "v1", "main~0^{tree}")
	testutil.AdvanceOrigin(t, o, "main", "n.txt", "n\n", "new on main")
	lock := filepath.Join(testutil.Git(t, primary, "rev-parse", "--git-common-dir"), "refs", "remotes", "origin", "main.lock")
	if !filepath.IsAbs(lock) {
		lock = filepath.Join(primary, lock)
	}
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if kept, err := (Net{}).FetchTagsFrom(context.Background(), primary, "origin"); err == nil {
		t.Fatalf("FetchTags with a locked ref = %v, nil; want an error", kept)
	}
	// The parser alone: an error line besides the refusals.
	stderr := "From file:///o\n ! [rejected]        v1         -> v1  (would clobber existing tag)\nerror: some local refs could not be updated"
	if kept := keptTags(stderr); kept != nil {
		t.Errorf("keptTags with an error line = %q, want nil", kept)
	}
	stderr = "From file:///o\n ! a80b951..942c273  main       -> origin/main  (unable to update local ref)\n ! [rejected]        v1         -> v1  (would clobber existing tag)"
	if kept := keptTags(stderr); kept != nil {
		t.Errorf("keptTags with a failed branch update = %q, want nil", kept)
	}
	if kept := keptTags("From file:///o\n ! [rejected]        v1         -> v1  (would clobber existing tag)"); len(kept) != 1 || kept[0] != "v1" {
		t.Errorf("keptTags = %q, want [v1]", kept)
	}
}

// HEAD moved away from the pinned commit: the tag is kept, TagMoved is set.
func TestWorktreeTagMoved(t *testing.T) {
	t.Parallel()
	_, repo, primary := layout(t, testutil.OriginOpts{Branches: []string{"feat"}})
	testutil.Git(t, primary, "tag", "v1")
	testutil.Git(t, primary, "tag", "v2")
	ctx := context.Background()
	for _, tag := range []string{"v1", "v2"} {
		if _, err := WorktreeAddTag(ctx, primary, filepath.Join(repo, tag), tag); err != nil {
			t.Fatal(err)
		}
	}
	w := findWorktree(t, mustList(t, primary), filepath.Join(repo, "v1"))
	if w.Tag != "v1" || w.TagMoved {
		t.Fatalf("fresh: %+v", w)
	}
	testutil.CommitFile(t, filepath.Join(repo, "v1"), "x.txt", "x\n", "on detached HEAD")
	testutil.Git(t, filepath.Join(repo, "v2"), "checkout", "-q", "--detach", "origin/feat")
	for _, tag := range []string{"v1", "v2"} {
		if w := findWorktree(t, mustList(t, primary), filepath.Join(repo, tag)); w.Tag != tag || !w.TagMoved {
			t.Errorf("moved %s: %+v, want tag kept and TagMoved", tag, w)
		}
	}
}

// Server messages (remote: ...) and ssh's known-hosts notice do not hide the
// refusals; hint: lines are failures like error: and fatal:.
func TestKeptTagsIgnoresRemoteAndSSHNotices(t *testing.T) {
	t.Parallel()
	refused := " ! [rejected]        v1         -> v1  (would clobber existing tag)"
	stderr := "Warning: Permanently added 'github.com' (ED25519) to the list of known hosts.\n" +
		"remote: Enumerating objects: 5, done.\nremote: Total 3 (delta 0), reused 0 (delta 0)\n" +
		"From github.com:o/a\n" + refused
	if kept := keptTags(stderr); len(kept) != 1 || kept[0] != "v1" {
		t.Errorf("keptTags = %q, want [v1]", kept)
	}
	for _, bad := range []string{"hint: something", "error: x", "fatal: y"} {
		if kept := keptTags(refused + "\n" + bad); kept != nil {
			t.Errorf("keptTags with %q = %q, want nil", bad, kept)
		}
	}
}

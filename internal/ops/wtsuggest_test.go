package ops

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/testutil"
)

// The suggestions of the home screen's w prompt (B47) are read from local
// refs with one git process: the local branches and the branches of the
// remotes wt add searches (origin; the fork and upstream for a clone of a
// fork), without the remote prefix nor HEAD, deduplicated and sorted, and
// the tags. Other remotes are not suggested: wt add would not find their
// branches.
func TestWorktreeSuggestions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "plain")
	testutil.Git(t, primary, "branch", "local-only")
	testutil.Git(t, primary, "tag", "v1.0")
	testutil.Git(t, primary, "update-ref", "refs/remotes/upstream/not-searched", "HEAD")
	testutil.Git(t, primary, "update-ref", "refs/remotes/originals/not-searched-either", "HEAD")
	_, forked := f.addForked(t, "forked", testutil.OriginOpts{Branches: []string{"both"}})
	testutil.Git(t, forked, "update-ref", "refs/remotes/origin/on-fork", "HEAD")
	testutil.Git(t, forked, "update-ref", "refs/remotes/upstream/on-upstream", "HEAD")
	testutil.Git(t, forked, "update-ref", "refs/remotes/other/elsewhere", "HEAD")
	testutil.Git(t, forked, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	testutil.Git(t, forked, "tag", "v2")
	for _, tc := range []struct {
		repo string
		want WorktreeSuggestions
	}{
		{"plain", WorktreeSuggestions{Branches: []string{"feat", "local-only", "main"}, Tags: []string{"v1.0"}}},
		{"forked", WorktreeSuggestions{Branches: []string{"both", "main", "on-fork", "on-upstream"}, Tags: []string{"v2"}}},
	} {
		ctx, counter := gitx.WithCallCounter(context.Background())
		got, err := SuggestWorktree(ctx, f.w, tc.repo)
		if err != nil {
			t.Fatalf("%s: %v", tc.repo, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: suggestions %+v, want %+v", tc.repo, got, tc.want)
		}
		if n := len(counter.Calls()); n != 1 {
			t.Errorf("%s: %d git processes, want 1", tc.repo, n)
		}
	}
	// Names that the prompt would display or accept changed (control
	// characters, C1 included, or invalid UTF-8), i.e. another ref, are
	// not suggested. They are written to packed-refs: APFS refuses the
	// loose ref file of an invalid UTF-8 name ("Illegal byte sequence").
	unsafe := []string{"refs/heads/caf\u0085e", "refs/heads/bad\xff", "refs/tags/t\u009b1", "refs/remotes/origin/x\u0085y"}
	addPackedRefs(t, primary, testutil.Git(t, primary, "rev-parse", "HEAD"), unsafe...)
	listed := strings.Split(testutil.Git(t, primary, "for-each-ref", "--format=%(refname)"), "\n")
	for _, ref := range unsafe {
		if !slices.Contains(listed, ref) {
			t.Fatalf("git for-each-ref does not list %q: %q", ref, listed)
		}
	}
	if got, err := SuggestWorktree(context.Background(), f.w, "plain"); err != nil || !reflect.DeepEqual(got, WorktreeSuggestions{Branches: []string{"feat", "local-only", "main"}, Tags: []string{"v1.0"}}) {
		t.Errorf("with unsafe names: %+v, %v", got, err)
	}
	// An unknown repository, or a primary checkout without its .git
	// directory (git would read an enclosing repository), give an error,
	// without running git.
	if err := os.Rename(filepath.Join(primary, ".git"), filepath.Join(primary, "git.moved")); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"plain", "nosuch"} {
		ctx, counter := gitx.WithCallCounter(context.Background())
		if got, err := SuggestWorktree(ctx, f.w, repo); err == nil || got.Branches != nil || got.Tags != nil || len(counter.Calls()) > 0 {
			t.Errorf("%s: %+v, error %v, %d git processes", repo, got, err, len(counter.Calls()))
		}
	}
}

// addPackedRefs makes refs point to oid in the repository of dir by writing
// them to packed-refs, without loose ref files (which some file systems
// refuse for some names). It packs the existing refs first and keeps the
// file sorted, as its header says.
func addPackedRefs(t *testing.T, dir, oid string, refs ...string) {
	t.Helper()
	gitDir := testutil.Git(t, dir, "rev-parse", "--absolute-git-dir")
	if _, err := os.Stat(filepath.Join(gitDir, "reftable")); err == nil {
		t.Fatalf("%s uses the reftable backend: no packed-refs file", gitDir)
	}
	testutil.Git(t, dir, "pack-refs", "--all")
	path := filepath.Join(gitDir, "packed-refs")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A record is a "<oid> <ref>" line and the "^<oid>" line that may
	// follow it (the peeled value of an annotated tag).
	type record struct{ ref, lines string }
	var recs []record
	for line := range strings.Lines(string(data)) {
		switch {
		case strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "^"):
			recs[len(recs)-1].lines += line
		default:
			_, ref, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
			recs = append(recs, record{ref, line})
		}
	}
	for _, ref := range refs {
		recs = append(recs, record{ref, oid + " " + ref + "\n"})
	}
	slices.SortFunc(recs, func(a, b record) int { return strings.Compare(a.ref, b.ref) })
	var b strings.Builder
	b.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for _, r := range recs {
		b.WriteString(r.lines)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

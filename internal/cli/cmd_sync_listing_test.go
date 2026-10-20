package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
)

// runWith runs the CLI with another hosting implementation.
func (e *env) runWith(t *testing.T, h hosting.Hosting, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	d := e.deps()
	d.Hosting = func() hosting.Hosting { return h }
	var out, errb strings.Builder
	code = executeDeps(Streams{In: strings.NewReader(""), Out: &out, Err: &errb}, d, args)
	return out.String(), errb.String(), code
}

// redirecting answers lookups of moved repositories with their current
// identity, like GitHub does after a transfer.
type redirecting struct {
	*hostingtest.Fake
	moved map[string]hosting.Repo // lower-case old slug -> current repo
}

func (h redirecting) Repo(ctx context.Context, owner, name string) (hosting.Repo, error) {
	if r, ok := h.moved[strings.ToLower(owner+"/"+name)]; ok {
		return r, nil
	}
	return h.Fake.Repo(ctx, owner, name)
}

// The org deleted a; its remote no longer exists, the clone may be the
// last copy.
func TestSyncRemoveGoneDeletedRemote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o := e.origin(t, "o", "a", false)
	e.origin(t, "o", "b", false)
	root := filepath.Join(t.TempDir(), "bundle")
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = root
	e.mustRun(t, "add", "--org", "o", "--repo", "a")
	if err := os.RemoveAll(o.Path); err != nil {
		t.Fatal(err)
	}
	e.fake = hostingtest.NewFake()
	e.origin(t, "o", "b", false)

	out, errs, code := e.run(t, "sync", "--remove-gone", "--yes")
	if code != 1 || !strings.Contains(errs, "the remote repository appears to be gone") || strings.Contains(errs, "--offline") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	out, errs, code = e.run(t, "sync", "--remove-gone", "--yes", "--offline")
	if code != 1 || !strings.Contains(errs, "--offline is not supported") {
		t.Fatalf("--offline: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if !exists(filepath.Join(root, "a", "main", ".git")) || !slices.Equal(names(load(t, root)), []string{"a"}) {
		t.Fatal("a was removed")
	}
	e.mustRun(t, "sync", "--remove-gone", "--yes", "--force")
	if exists(filepath.Join(root, "a")) || len(names(load(t, root))) != 0 {
		t.Fatal("a not removed with --force")
	}
}

// An empty but successful listing (e.g. a token without access to
// private repositories) must not remove everything.
func TestSyncEmptyListingNeedsAllowMassRemoval(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	e.mustRun(t, "add", "--org", "o", "--repo", "b")
	e.mustRun(t, "add", "--org", "o", "--repo", "c")
	e.fake = hostingtest.NewFake()
	e.fake.AddOwner("o")
	out, errs, code := e.run(t, "sync")
	if code != 0 || !strings.Contains(out, "then run: sheaf --bundle "+root+" sync --org o --remove-gone --yes --allow-mass-removal\n") {
		t.Fatalf("dry run: exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	out, errs, code = e.run(t, "sync", "--remove-gone", "--yes")
	if code != 1 || !strings.Contains(out, "0 done, 0 skipped, 3 failed") || !strings.Contains(errs, "--allow-mass-removal") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if got := names(load(t, root)); len(got) != 3 {
		t.Fatalf("repositories = %v", got)
	}
	// The remotes still answer: the listing, not the repositories, is
	// wrong (token scope), so --allow-mass-removal alone does not remove.
	out, errs, code = e.run(t, "sync", "--remove-gone", "--yes", "--allow-mass-removal")
	if code != 1 || !strings.Contains(out, "0 done, 0 skipped, 3 failed") || !strings.Contains(errs, "the GitHub API cannot see o/a but its remote still answers") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if got := names(load(t, root)); len(got) != 3 {
		t.Fatalf("repositories = %v", got)
	}
	e.mustRun(t, "sync", "--remove-gone", "--yes", "--allow-mass-removal", "--force")
	if got := names(load(t, root)); len(got) != 0 {
		t.Fatalf("repositories = %v", got)
	}
}

// Empty repositories are skipped, not failed.
func TestSyncAddNewSkipsEmptyRepository(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: "empty", CloneURL: "file:///nowhere"})
	out, errs, code := e.run(t, "sync", "--exclude", "b", "--exclude", "c", "--add-new")
	if code != 0 || !strings.Contains(out, "skipped  empty") || !strings.Contains(out, "0 done, 1 skipped, 0 failed") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if exists(filepath.Join(root, "empty")) {
		t.Fatal("directory created for the empty repository")
	}
}

// A transfer between two tracked organizations is reported, never
// removed nor added again.
func TestSyncTransferBetweenTrackedOrgs(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	e.origin(t, "p", "z", false)
	e.mustRun(t, "add", "--org", "p", "--repo", "z")
	a, _ := load(t, root).Repository("a")
	e.fake = hostingtest.NewFake()
	e.origin(t, "o", "b", false)
	e.fake.AddRepo(hosting.Repo{Owner: "p", Name: "z", CloneURL: a.URL, DefaultBranch: "main"})
	moved := hosting.Repo{Owner: "p", Name: "a", CloneURL: a.URL, DefaultBranch: "main"}
	e.fake.AddRepo(moved)
	h := redirecting{Fake: e.fake, moved: map[string]hosting.Repo{"o/a": moved}}
	out, errs, code := e.runWith(t, h, "sync")
	if code != 0 || !strings.Contains(out, "moved    o/a") || !strings.Contains(out, "o/a → p/a") || strings.Contains(out, "new      p/a") || strings.Contains(out, "gone") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	out, errs, code = e.runWith(t, h, "sync", "--add-new", "--remove-gone", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if got := names(load(t, root)); !slices.Equal(got, []string{"a", "b", "z"}) {
		t.Fatalf("repositories = %v", got)
	}
}

func TestSyncExcludeErrorsExplainWhy(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	before := readYAML(t, root)
	if _, errs, code := e.run(t, "sync", "--exclude", "old"); code == 0 || !strings.Contains(errs, "--include-archived") {
		t.Fatalf("archived: exit %d, stderr %s", code, errs)
	}
	e.fake.ListErr["o"] = errors.New("boom")
	if _, errs, code := e.run(t, "sync", "--exclude", "b"); code == 0 || !strings.Contains(errs, "listing repositories of o failed") {
		t.Fatalf("failed listing: exit %d, stderr %s", code, errs)
	}
	if readYAML(t, root) != before {
		t.Fatal("sheaf.yaml changed")
	}
}

// twoOrgs tracks o (with a) and p; both organizations list a new x.
func twoOrgs(t *testing.T) (*env, string) {
	t.Helper()
	e, root := syncBundle(t)
	e.fake = hostingtest.NewFake()
	e.origin(t, "o", "a", false)
	e.origin(t, "o", "x", false)
	e.origin(t, "p", "x", false)
	e.origin(t, "p", "y", false)
	e.mustRun(t, "add", "--org", "p", "--repo", "y")
	return e, root
}

func TestSyncExcludeAmbiguousAcrossOrgs(t *testing.T) {
	t.Parallel()
	e, root := twoOrgs(t)
	if _, errs, code := e.run(t, "sync", "--exclude", "x"); code == 0 || !strings.Contains(errs, "several organizations") || !strings.Contains(errs, "<org>/x") {
		t.Fatalf("exit %d, stderr %s", code, errs)
	}
	e.mustRun(t, "sync", "--exclude", "p/x")
	b := load(t, root)
	for _, s := range b.Sources {
		if want := map[string]int{"o": 0, "p": 1}[s.Org]; len(s.Exclude) != want {
			t.Fatalf("sources = %+v", b.Sources)
		}
	}
}

func TestSyncAddNewSameNameInTwoOrgs(t *testing.T) {
	t.Parallel()
	e, root := twoOrgs(t)
	out, errs, code := e.run(t, "sync", "--add-new")
	if code != 1 || !strings.Contains(out, "1 done, 0 skipped, 1 failed") || !strings.Contains(errs, "--as <dir>") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if got := names(load(t, root)); !slices.Equal(got, []string{"a", "x", "y"}) {
		t.Fatalf("repositories = %v", got)
	}
}

func TestSyncJSONReviewDetails(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	a, _ := load(t, root).Repository("a")
	e.fake = hostingtest.NewFake()
	e.origin(t, "o", "b", false)
	out := e.mustRun(t, "sync", "--json")
	var doc struct {
		Proposals []map[string]any `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var gone map[string]any
	for _, p := range doc.Proposals {
		if p["kind"] == "gone" {
			gone = p
		}
	}
	if gone == nil || gone["url"] != a.URL || gone["name"] != "a" {
		t.Fatalf("gone = %v\n%s", gone, out)
	}
	for _, k := range []string{"fork", "archived"} {
		if _, ok := gone[k]; ok {
			t.Fatalf("gone proposal has %q:\n%s", k, out)
		}
	}
	for _, p := range doc.Proposals {
		if p["kind"] == "new" {
			if _, ok := p["fork"]; !ok {
				t.Fatalf("new proposal lacks fork:\n%s", out)
			}
		}
	}

	e.fake.ListErr["o"] = errors.New("boom")
	out, errs, code := e.run(t, "sync", "--json")
	if code != 1 || !strings.Contains(errs, "boom") {
		t.Fatalf("failed listing: exit %d\nstderr:\n%s", code, errs)
	}
	var withErr struct {
		Orgs []struct {
			Error string `json:"error"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal([]byte(out), &withErr); err != nil || len(withErr.Orgs) != 1 || !strings.Contains(withErr.Orgs[0].Error, "boom") {
		t.Fatalf("doc = %+v, %v\n%s", withErr, err, out)
	}
}

func TestSyncTruncatedWarningPluralization(t *testing.T) {
	t.Parallel()
	e, _ := syncBundle(t)
	e.fake = hostingtest.NewFake()
	e.origin(t, "o", "a", false)
	_, errs, code := e.runWith(t, truncatedHosting{e.fake}, "sync")
	if code != 0 || !strings.Contains(errs, "(1 repository listed)") {
		t.Fatalf("exit %d, stderr %s", code, errs)
	}
}

// failingLookup lists normally but every lookup fails (e.g. rate limit).
type failingLookup struct{ *hostingtest.Fake }

func (h failingLookup) Repo(context.Context, string, string) (hosting.Repo, error) {
	return hosting.Repo{}, errors.New("HTTP 403: API rate limit exceeded")
}

func TestSyncUnconfirmedGoneExitsNonZero(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	e.fake = hostingtest.NewFake()
	e.origin(t, "o", "b", false) // a is a gone candidate, its lookup fails
	before := readYAML(t, root)
	out, errs, code := e.runWith(t, failingLookup{e.fake}, "sync", "--json")
	if code != 1 || !strings.Contains(errs, "could not confirm that o/a is gone") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	var doc struct {
		Orgs []struct {
			Unconfirmed []string `json:"unconfirmed"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Orgs) != 1 || !slices.Equal(doc.Orgs[0].Unconfirmed, []string{"o/a"}) {
		t.Fatalf("doc = %+v, %v\n%s", doc, err, out)
	}
	if readYAML(t, root) != before {
		t.Fatal("sheaf.yaml changed")
	}
}

func TestSyncSingleOrgPossiblyMoved(t *testing.T) {
	t.Parallel()
	e, root := syncBundle(t)
	a, _ := load(t, root).Repository("a")
	e.origin(t, "p", "z", false)
	e.mustRun(t, "add", "--org", "p", "--repo", "z")
	e.fake.AddRepo(hosting.Repo{Owner: "p", Name: "a", CloneURL: a.URL, DefaultBranch: "main"})
	out, errs, code := e.run(t, "sync", "--org", "p", "--add-new")
	if code != 0 || !strings.Contains(out, "new      p/a: possibly moved from o/a; run sync for that org too") || !strings.Contains(out, "0 done, 1 skipped, 0 failed") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	out = e.mustRun(t, "sync", "--org", "p", "--json")
	if !strings.Contains(out, `"possibly_moved": "o/a"`) {
		t.Fatalf("json:\n%s", out)
	}
	if got := names(load(t, root)); !slices.Equal(got, []string{"a", "z"}) {
		t.Fatalf("repositories = %v", got)
	}
}

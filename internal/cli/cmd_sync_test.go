package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
)

// syncBundle creates a bundle tracking organization o with repository a
// cloned; the fake also lists b, c and the archived old.
func syncBundle(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.origin(t, "o", n, false)
	}
	e.origin(t, "o", "old", true)
	root := filepath.Join(t.TempDir(), "bundle")
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = root
	e.mustRun(t, "add", "--org", "o", "--repo", "a")
	return e, root
}

func readYAML(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "sheaf.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSyncDryRunListsProposals(t *testing.T) {
	e, root := syncBundle(t)
	before := readYAML(t, root)
	out, errs, code := e.run(t, "sync")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"new      o/b", "new      o/c", "sheaf sync --add-new"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "old") {
		t.Fatalf("archived repository proposed:\n%s", out)
	}
	if readYAML(t, root) != before || exists(filepath.Join(root, "b")) {
		t.Fatal("dry run changed the bundle")
	}
	out = e.mustRun(t, "sync", "--include-archived")
	if !strings.Contains(out, "new      o/old (archived)") {
		t.Fatalf("--include-archived:\n%s", out)
	}
}

func TestSyncJSON(t *testing.T) {
	e, _ := syncBundle(t)
	out := e.mustRun(t, "sync", "--json")
	var doc struct {
		Version int `json:"version"`
		Orgs    []struct {
			Org       string `json:"org"`
			Listed    int    `json:"listed"`
			Truncated bool   `json:"truncated"`
		} `json:"orgs"`
		Proposals []struct {
			Key, Kind, Org, Name, Slug, URL string
		} `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if doc.Version != 1 || len(doc.Orgs) != 1 || doc.Orgs[0].Org != "o" || doc.Orgs[0].Listed != 4 {
		t.Fatalf("doc = %+v", doc)
	}
	var keys []string
	for _, p := range doc.Proposals {
		keys = append(keys, p.Key)
		if p.Kind != "new" || p.Org != "o" || p.URL == "" {
			t.Fatalf("proposal = %+v", p)
		}
	}
	if !slices.Equal(keys, []string{"new:o/b", "new:o/c"}) {
		t.Fatalf("keys = %v", keys)
	}
	if _, errs, code := e.run(t, "sync", "--json", "--add-new"); code == 0 || !strings.Contains(errs, "--json") {
		t.Fatalf("--json --add-new: exit %d, stderr %s", code, errs)
	}
}

func TestSyncAddNewWithExclude(t *testing.T) {
	e, root := syncBundle(t)
	if _, errs, code := e.run(t, "sync", "--exclude", "nope"); code == 0 || !strings.Contains(errs, "nope") {
		t.Fatalf("--exclude nope: exit %d, stderr %s", code, errs)
	}
	out := e.mustRun(t, "sync", "--exclude", "o/C", "--add-new")
	if !strings.Contains(out, "1 done, 0 skipped, 0 failed") {
		t.Fatalf("summary:\n%s", out)
	}
	b := load(t, root)
	if got := names(b); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("repositories = %v", got)
	}
	if r, _ := b.Repository("b"); r.Origin != "org:o" {
		t.Fatalf("b = %+v", r)
	}
	if len(b.Sources) != 1 || !slices.Equal(b.Sources[0].Exclude, []string{"c"}) {
		t.Fatalf("sources = %+v", b.Sources)
	}
	if !exists(filepath.Join(root, "b", "main", ".git")) || exists(filepath.Join(root, "c")) {
		t.Fatal("wrong checkouts on disk")
	}
	out = e.mustRun(t, "sync")
	if !strings.Contains(out, "nothing to sync") {
		t.Fatalf("second sync:\n%s", out)
	}
}

func TestSyncRemoveGone(t *testing.T) {
	e, root := syncBundle(t)
	e.mustRun(t, "add", "--org", "o", "--repo", "b")
	if err := os.WriteFile(filepath.Join(root, "b", "main", "precious.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a and b are deleted on GitHub, remotes included.
	b := load(t, root)
	for _, n := range []string{"a", "b"} {
		r, _ := b.Repository(n)
		if err := os.RemoveAll(strings.TrimPrefix(r.URL, "file://")); err != nil {
			t.Fatal(err)
		}
	}
	e.fake = hosting.NewFake()
	e.origin(t, "o", "c", false)

	out := e.mustRun(t, "sync")
	for _, want := range []string{"gone     o/a", "gone     o/b", "--remove-gone --yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
	if _, errs, code := e.run(t, "sync", "--remove-gone"); code == 0 || !strings.Contains(errs, "--yes") {
		t.Fatalf("--remove-gone without --yes: exit %d, stderr %s", code, errs)
	}
	if got := names(load(t, root)); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("removed without --yes: %v", got)
	}
	// Both tracked repositories of o look gone: a mass removal, allowed here
	// (refusal covered by TestSyncEmptyListingNeedsAllowMassRemoval). A gone
	// remote is a data risk (only copy), so nothing goes without --force.
	out, errs, code := e.run(t, "sync", "--remove-gone", "--yes", "--allow-mass-removal")
	if code != 1 || !strings.Contains(out, "failed   a") || !strings.Contains(out, "failed   b") ||
		!strings.Contains(errs, "appears to be gone") || !strings.Contains(errs, "1 untracked files") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if got := names(load(t, root)); !slices.Equal(got, []string{"a", "b"}) || !exists(filepath.Join(root, "a")) {
		t.Fatalf("repositories = %v", got)
	}
	e.mustRun(t, "sync", "--remove-gone", "--yes", "--allow-mass-removal", "--force")
	if got := names(load(t, root)); len(got) != 0 || exists(filepath.Join(root, "b")) {
		t.Fatalf("repositories after --force = %v", got)
	}
}

// truncatedHosting reports every listing as possibly truncated.
type truncatedHosting struct{ *hosting.Fake }

func (h truncatedHosting) ListRepos(ctx context.Context, owner string) ([]hosting.Repo, error) {
	rs, err := h.Fake.ListRepos(ctx, owner)
	if err != nil {
		return nil, err
	}
	return rs, fmt.Errorf("%w: %q has more than %d repositories", hosting.ErrPossiblyTruncated, owner, len(rs))
}

func TestSyncTruncatedAndFailedListings(t *testing.T) {
	e, root := syncBundle(t)
	e.fake = hosting.NewFake()
	e.origin(t, "o", "c", false) // a looks gone, but the listing may be truncated
	d := e.deps()
	d.Hosting = func() hosting.Hosting { return truncatedHosting{e.fake} }
	var out, errb strings.Builder
	cmd := NewRootCmdWithDeps(Streams{In: strings.NewReader(""), Out: &out, Err: &errb}, d)
	if code := execute(cmd, []string{"sync", "--remove-gone", "--yes"}); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "gone     ") || !strings.Contains(out.String(), "no gone repository to remove") || !strings.Contains(out.String(), "new      o/c") || !strings.Contains(errb.String(), "truncated") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out.String(), errb.String())
	}
	if got := names(load(t, root)); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("repositories = %v", got)
	}

	e.fake.ListErr["o"] = fmt.Errorf("gh: boom")
	stdout, errs, code := e.run(t, "sync")
	if code != 1 || !strings.Contains(errs, "boom") {
		t.Fatalf("failed listing: exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, errs)
	}
}

func TestSyncFlagValidation(t *testing.T) {
	e, root := syncBundle(t)
	before := readYAML(t, root)
	for _, tc := range []struct {
		args   []string
		reason string
	}{
		{[]string{"sync", "--yes"}, "only apply to --remove-gone"},
		{[]string{"sync", "--force"}, "only apply to --remove-gone"},
		{[]string{"sync", "--allow-mass-removal"}, "only apply to --remove-gone"},
		{[]string{"sync", "--offline"}, "--offline is not supported"},
		{[]string{"sync", "--remove-gone", "--yes", "--offline"}, "--offline is not supported"},
		{[]string{"sync", "--remove-gone"}, "confirm with --yes"},
		{[]string{"sync", "--json", "--add-new"}, "--json only lists"},
		{[]string{"sync", "--json", "--exclude", "b"}, "--json only lists"},
		{[]string{"sync", "--json", "--remove-gone", "--yes"}, "--json only lists"},
		{[]string{"sync", "--org", "unknown"}, `"unknown" is not a tracked organization`},
	} {
		if _, errs, code := e.run(t, tc.args...); code != 1 || !strings.Contains(errs, tc.reason) {
			t.Errorf("%v: exit %d, stderr %q; want reason %q", tc.args, code, errs, tc.reason)
		}
	}
	if readYAML(t, root) != before {
		t.Fatal("sheaf.yaml changed")
	}
}

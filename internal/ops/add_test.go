package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

type fixture struct {
	w    *Workspace
	fake *hostingtest.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	testutil.IsolateGit(t)
	root, err := Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{w: w, fake: hostingtest.NewFake()}
}

// openFixture returns a fixture for the existing bundle root (a copy of a
// cached fixture, see testutil.CachedTree), with an empty fake.
func openFixture(t *testing.T, root string) *fixture {
	t.Helper()
	testutil.IsolateGit(t)
	w, err := OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{w: w, fake: hostingtest.NewFake()}
}

// repo registers owner/name in the fake, backed by a fresh bare origin.
func (f *fixture) repo(t *testing.T, owner, name, defaultBranch string, archived bool) hosting.Repo {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{DefaultBranch: defaultBranch})
	r := hosting.Repo{Owner: owner, Name: name, CloneURL: o.URL, DefaultBranch: defaultBranch, Archived: archived}
	f.fake.AddRepo(r)
	return r
}

func (f *fixture) add(t *testing.T, req AddRequest) (Summary, error) {
	t.Helper()
	return Add(context.Background(), f.w, f.fake, req, 4, Discard)
}

func (f *fixture) onDisk(t *testing.T) *bundle.Bundle {
	t.Helper()
	b, err := bundle.Load(f.w.Root)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func statuses(s Summary) map[string]Status {
	m := map[string]Status{}
	for _, r := range s.Results {
		m[r.Repo] = r.Status
	}
	return m
}

func repoNames(b *bundle.Bundle) []string {
	var n []string
	for _, r := range b.Repositories {
		n = append(n, r.Name)
	}
	return n
}

func TestAddSlugsClonesIntoLayoutAndRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "o", "b", "dev/next", false)
	s, err := f.add(t, AddRequest{Slugs: []string{"o/a", "O/B"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Err(); err != nil {
		t.Fatalf("summary: %+v", s)
	}
	for dir, branch := range map[string]string{"a/main": "main", "b/dev-next": "dev/next"} {
		p := filepath.Join(f.w.Root, dir)
		got, err := gitx.CurrentBranch(context.Background(), p)
		if err != nil || got != branch {
			t.Fatalf("%s: branch = %q, %v; want %q", dir, got, err, branch)
		}
	}
	b := f.onDisk(t)
	want := []bundle.Repository{
		{Name: "a", Slug: "o/a", URL: f.mustRepo(t, "o", "a").CloneURL, DefaultBranch: "main", Origin: "explicit"},
		{Name: "b", Slug: "o/b", URL: f.mustRepo(t, "o", "b").CloneURL, DefaultBranch: "dev/next", Origin: "explicit"},
	}
	if !slices.Equal(b.Repositories, want) {
		t.Fatalf("repositories =\n%+v\nwant\n%+v", b.Repositories, want)
	}
}

func (f *fixture) mustRepo(t *testing.T, owner, name string) hosting.Repo {
	t.Helper()
	r, err := f.fake.Repo(context.Background(), owner, name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAddSkipsExistingAndReportsPerRepoFailures(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "o", "b", "main", false)
	f.fake.AddRepo(hosting.Repo{Owner: "o", Name: "broken", CloneURL: "file:///nonexistent/origin.git", DefaultBranch: "main"})
	f.fake.AddRepo(hosting.Repo{Owner: "o", Name: "empty", CloneURL: "file:///nonexistent/empty.git"})
	if s, err := f.add(t, AddRequest{Slugs: []string{"o/a"}}); err != nil || s.Err() != nil {
		t.Fatalf("first add: %+v, %v", s, err)
	}
	s, err := f.add(t, AddRequest{Slugs: []string{"o/a", "o/b", "o/broken", "o/missing", "o/empty"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Status{"a": StatusSkipped, "b": StatusDone, "broken": StatusFailed, "o/missing": StatusFailed, "empty": StatusFailed}
	if got := statuses(s); !mapsEqual(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}
	for _, r := range s.Results {
		if r.Repo == "o/missing" && !errors.Is(r.Err, hosting.ErrNotFound) {
			t.Fatalf("missing: err = %v, want ErrNotFound", r.Err)
		}
	}
	if s.Err() == nil {
		t.Fatal("summary reports no failure")
	}
	if got := repoNames(f.onDisk(t)); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("recorded = %v, want [a b]", got)
	}
	for _, d := range []string{"broken", "empty"} {
		if _, err := os.Lstat(filepath.Join(f.w.Root, d)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed clone left %s behind: %v", d, err)
		}
	}
}

func mapsEqual(a, b map[string]Status) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestAddNameCollisionNeedsAs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "p", "a", "main", false)
	f.repo(t, "q", "a", "main", false)
	if s, err := f.add(t, AddRequest{Slugs: []string{"o/a"}}); err != nil || s.Err() != nil {
		t.Fatalf("first add: %+v, %v", s, err)
	}
	s, err := f.add(t, AddRequest{Slugs: []string{"p/a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Results) != 1 || s.Results[0].Status != StatusFailed || !strings.Contains(s.Results[0].Err.Error(), "--as") {
		t.Fatalf("collision result = %+v, want failure suggesting --as", s.Results)
	}
	// Two new repos with the same name in one request: the second fails.
	s, err = f.add(t, AddRequest{Slugs: []string{"p/a", "q/a"}})
	if err != nil || s.Count(StatusFailed) != 2 {
		t.Fatalf("results = %+v, %v; want two failures (both clash with o/a)", s.Results, err)
	}
	s, err = f.add(t, AddRequest{Slugs: []string{"p/a"}, As: "a-p"})
	if err != nil || s.Err() != nil {
		t.Fatalf("add --as: %+v, %v", s, err)
	}
	if _, err := os.Stat(filepath.Join(f.w.Root, "a-p", "main", ".git")); err != nil {
		t.Fatal(err)
	}
	if r, ok := f.onDisk(t).Repository("a-p"); !ok || r.Slug != "p/a" {
		t.Fatalf("a-p entry = %+v", r)
	}
}

func TestAddRefusesUnregisteredExistingDirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	keep := filepath.Join(f.w.Root, "a", "notes.txt")
	if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := f.add(t, AddRequest{Slugs: []string{"o/a"}})
	if err != nil || s.Count(StatusFailed) != 1 {
		t.Fatalf("results = %+v, %v; want failure", s.Results, err)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "mine" {
		t.Fatalf("existing directory was touched: %q, %v", b, err)
	}
}

func TestAddInvalidRequests(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	for name, req := range map[string]AddRequest{
		"nothing":       {},
		"as with many":  {Slugs: []string{"o/a", "o/b"}, As: "x"},
		"bad slug":      {Slugs: []string{"oa"}},
		"bad slug 2":    {Slugs: []string{"o/a/b"}},
		"slugs and org": {Slugs: []string{"o/a"}, Org: "o", All: true},
		"all and repos": {Org: "o", All: true, Repos: []string{"a"}},
		"invalid as":    {Slugs: []string{"o/a"}, As: "../x"},
		"unknown repo":  {Org: "o", Repos: []string{"nope"}},
		"repos w/o org": {Repos: []string{"a"}},
		"as with --all": {Org: "o", All: true, As: "x"},
	} {
		if _, err := f.add(t, req); err == nil {
			t.Errorf("%s: Add succeeded, want error", name)
		}
	}
	if b := f.onDisk(t); len(b.Repositories) != 0 || len(b.Sources) != 0 {
		t.Fatalf("invalid requests changed the bundle: %+v", b)
	}
}

func TestAddOrg(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "Org", "a", "main", false)
	f.repo(t, "Org", "b", "main", false)
	f.repo(t, "Org", "old", "main", true)

	_, err := f.add(t, AddRequest{Org: "org"})
	var sel *SelectionRequiredError
	if !errors.As(err, &sel) {
		t.Fatalf("err = %v, want SelectionRequiredError", err)
	}
	var avail []string
	for _, r := range sel.Available {
		avail = append(avail, r.Name)
	}
	if !slices.Equal(avail, []string{"a", "b"}) {
		t.Fatalf("available = %v, want [a b] (archived hidden)", avail)
	}

	s, err := f.add(t, AddRequest{Org: "org", Repos: []string{"B"}})
	if err != nil || s.Err() != nil || len(s.Results) != 1 {
		t.Fatalf("add --repo: %+v, %v", s, err)
	}
	s, err = f.add(t, AddRequest{Org: "org", All: true})
	if err != nil || s.Err() != nil {
		t.Fatalf("add --all: %+v, %v", s, err)
	}
	if want := map[string]Status{"a": StatusDone, "b": StatusSkipped}; !mapsEqual(statuses(s), want) {
		t.Fatalf("statuses = %v, want %v", statuses(s), want)
	}
	b := f.onDisk(t)
	if len(b.Sources) != 1 || b.Sources[0].Org != "Org" {
		t.Fatalf("sources = %+v, want [Org]", b.Sources)
	}
	for _, r := range b.Repositories {
		if r.Origin != "org:Org" {
			t.Fatalf("repo %s origin = %q, want org:Org", r.Name, r.Origin)
		}
	}
	if _, err := f.add(t, AddRequest{Org: "org", Repos: []string{"old"}}); err == nil {
		t.Fatal("adding an archived repo by name without IncludeArchived succeeded")
	} else if want := "; to add it, run: sheaf add --org org --include-archived --repo old"; !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("archived: err = %v, want it to end with %q", err, want)
	}
	s, err = f.add(t, AddRequest{Org: "org", All: true, IncludeArchived: true})
	if err != nil || statuses(s)["old"] != StatusDone {
		t.Fatalf("include archived: %+v, %v", s, err)
	}
}

func TestAddCancelledMidwayLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep := ReporterFunc(func(e Event) {
		if e.Kind == KindStart && e.Phase == PhaseClone {
			cancel()
		}
	})
	s, err := Add(ctx, f.w, f.fake, AddRequest{Slugs: []string{"o/a"}}, 1, rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Results) != 1 || s.Results[0].Status != StatusFailed || !errors.Is(s.Results[0].Err, context.Canceled) {
		t.Fatalf("results = %+v, want one failure with context.Canceled", s.Results)
	}
	if _, err := os.Lstat(filepath.Join(f.w.Root, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled clone left a directory: %v", err)
	}
	if len(f.onDisk(t).Repositories) != 0 {
		t.Fatal("cancelled clone was recorded")
	}
}

func hostingRepo(owner, name, url string) hosting.Repo {
	return hosting.Repo{Owner: owner, Name: name, CloneURL: url, DefaultBranch: "main"}
}

// The org is tracked only when the request did not entirely fail.
func TestAddOrgSourceRecordedUnlessEverythingFailed(t *testing.T) {
	t.Parallel()
	t.Run("all clones failed", func(t *testing.T) {
		f := newFixture(t)
		f.fake.AddRepo(hosting.Repo{Owner: "o", Name: "broken", CloneURL: "file:///nonexistent/b.git", DefaultBranch: "main"})
		s, err := f.add(t, AddRequest{Org: "o", All: true})
		if err != nil || s.Count(StatusFailed) != 1 {
			t.Fatalf("%+v, %v", s, err)
		}
		if n := len(f.onDisk(t).Sources); n != 0 {
			t.Fatalf("%d sources recorded, want 0", n)
		}
	})
	t.Run("empty org with --all", func(t *testing.T) {
		f := newFixture(t)
		f.fake.AddOwner("o")
		if _, err := f.add(t, AddRequest{Org: "o", All: true}); err != nil {
			t.Fatal(err)
		}
		if n := len(f.onDisk(t).Sources); n != 1 {
			t.Fatalf("%d sources, want 1 (explicit --all on an empty org)", n)
		}
	})
	t.Run("everything already present", func(t *testing.T) {
		f := newFixture(t)
		f.repo(t, "o", "a", "main", false)
		if _, err := f.add(t, AddRequest{Slugs: []string{"o/a"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.add(t, AddRequest{Org: "o", All: true}); err != nil {
			t.Fatal(err)
		}
		if n := len(f.onDisk(t).Sources); n != 1 {
			t.Fatalf("%d sources, want 1", n)
		}
	})
}

// A truncated listing still lets named repositories be added.
func TestAddOrgTruncatedListing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.fake.ListErr["o"] = fmt.Errorf("listing o: %w", hosting.ErrPossiblyTruncated)
	s, err := f.add(t, AddRequest{Org: "o", Repos: []string{"a"}})
	if err != nil || s.Count(StatusDone) != 1 {
		t.Fatalf("add --repo with truncated listing: %+v, %v", s, err)
	}
	if _, err := f.add(t, AddRequest{Org: "o", All: true}); !errors.Is(err, hosting.ErrPossiblyTruncated) {
		t.Fatalf("add --all with truncated listing: err = %v, want ErrPossiblyTruncated", err)
	}
	// The failed lookup of a repository missing from the listing names it
	// (the gh adapter's errors do not, see hosting.GHError).
	f.fake.RepoErr["o/b"] = hosting.ErrGHNotAuthenticated
	if _, err := f.add(t, AddRequest{Org: "o", Repos: []string{"b"}}); err == nil || err.Error() != "o/b: gh is not authenticated" || !errors.Is(err, hosting.ErrGHNotAuthenticated) {
		t.Fatalf("add --repo b with truncated listing and failed lookup: err = %v", err)
	}
}

// Cancelling a clone that is really running removes the partial
// directory and leaves sheaf.yaml unchanged. upload-pack runs pack-objects
// through uploadpack.packObjectsHook (only honoured from protected
// configuration: here the test's isolated global config file, since git
// strips GIT_CONFIG_* from the environment of a local upload-pack), a
// script that signals it started and sleeps; cancellation kills git's whole
// process group.
func TestAddCancelKillsRunningClone(t *testing.T) {
	testutil.IsolateGitPerTest(t) // writable global configuration
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	scripts := t.TempDir()
	marker := filepath.Join(scripts, "started")
	slow := filepath.Join(scripts, "slow-pack-objects")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\ntouch '"+marker+"'\nsleep 30\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.OpenFile(os.Getenv("GIT_CONFIG_GLOBAL"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(cfg, "[uploadpack]\n\tpackObjectsHook = %s\n", slow); err != nil {
		t.Fatal(err)
	}
	cfg.Close()
	before, err := os.ReadFile(filepath.Join(f.w.Root, bundle.FileName))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	s, err := Add(ctx, f.w, f.fake, AddRequest{Slugs: []string{"o/a"}}, 1, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Fatal("the slow upload-pack never ran; the test proves nothing")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Add returned after %v", d)
	}
	if len(s.Results) != 1 || !errors.Is(s.Results[0].Err, context.Canceled) {
		t.Fatalf("results = %+v, want cancellation", s.Results)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("partial clone left behind")
	}
	after, _ := os.ReadFile(filepath.Join(f.w.Root, bundle.FileName))
	if string(after) != string(before) {
		t.Fatalf("sheaf.yaml changed:\n%s", after)
	}
}

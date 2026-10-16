package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/hosting/hostingtest"
	"github.com/jponge/sheaf/internal/testutil"
)

// sortedDefaultCalls returns the SetDefaultRepo calls of fake sorted by
// directory (tasks run in parallel).
func sortedDefaultCalls(fake *hostingtest.Fake) []hostingtest.DefaultRepoCall {
	calls := fake.DefaultRepoCalls()
	slices.SortFunc(calls, func(a, b hostingtest.DefaultRepoCall) int { return strings.Compare(a.Dir, b.Dir) })
	return calls
}

func wantDefaultCalls(t *testing.T, fake *hostingtest.Fake, want ...hostingtest.DefaultRepoCall) {
	t.Helper()
	if got := sortedDefaultCalls(fake); !slices.Equal(got, want) {
		t.Fatalf("gh default repository calls = %+v\nwant %+v", got, want)
	}
}

// messagesWith returns the event messages containing s.
func messagesWith(evs []Event, s string) []string {
	var out []string
	for _, e := range evs {
		if strings.Contains(e.Message, s) {
			out = append(out, e.Message)
		}
	}
	return out
}

func TestAddSetsGHDefaultRepoOfEachClone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "o", "b", "dev", false)
	rec := &recorder{}
	s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"o/a", "O/B"}}, 2, rec)
	if err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	wantDefaultCalls(t, f.fake,
		hostingtest.DefaultRepoCall{Dir: filepath.Join(f.w.Root, "a", "main"), Slug: "o/a"},
		hostingtest.DefaultRepoCall{Dir: filepath.Join(f.w.Root, "b", "dev"), Slug: "o/b"})
	if got := messagesWith(rec.events, "gh default repository: o/"); len(got) != 2 {
		t.Fatalf("progress about the gh default repository: %q", got)
	}
}

func TestAddOrgSetsGHDefaultRepo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "o", "b", "main", false)
	if s, err := Add(context.Background(), f.w, f.fake, AddRequest{Org: "o", All: true}, 2, Discard); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	wantDefaultCalls(t, f.fake,
		hostingtest.DefaultRepoCall{Dir: filepath.Join(f.w.Root, "a", "main"), Slug: "o/a"},
		hostingtest.DefaultRepoCall{Dir: filepath.Join(f.w.Root, "b", "main"), Slug: "o/b"})
}

// For a fork, the default is the reference repository (upstream), in its
// canonical spelling, not the fork (origin).
func TestAddForkSetsReferenceAsGHDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.forkedRepo(t, "Up", "x", "x-renamed")
	if s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"up/X"}}, 1, Discard); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	wantDefaultCalls(t, f.fake, hostingtest.DefaultRepoCall{Dir: filepath.Join(f.w.Root, "x", "main"), Slug: "Up/x"})
}

func TestSyncAddNewSetsGHDefaultRepo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.repo(t, "o", "n", "main", false)
	props := []Proposal{{Kind: ProposalNew, Org: "o", Name: "n", Slug: "o/n", Repo: r, URL: r.CloneURL}}
	if s := SyncAddNewWithOptions(context.Background(), f.w, props, CloneOptions{Hosting: f.fake}, 1, Discard); s.Err() != nil {
		t.Fatalf("sync add: %+v", s)
	}
	wantDefaultCalls(t, f.fake, hostingtest.DefaultRepoCall{Dir: filepath.Join(f.w.Root, "n", "main"), Slug: "o/n"})
}

// gh missing, not authenticated or not answering: one warning for the whole
// command, no further gh calls, and every repository is still added.
func TestGHDefaultUnusableWarnsOncePerCommand(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"gh missing":                   {fmt.Errorf("%w (gh): install it", hosting.ErrGHNotFound), "install the GitHub CLI"},
		"not authenticated":            {fmt.Errorf("gh repo set-default: %w: exit status 4: To get started with GitHub CLI, please run:  gh auth login\nAlternatively, ...", hosting.ErrGHNotAuthenticated), "run `gh auth login`"},
		"not authenticated, no stderr": {fmt.Errorf("gh repo set-default o/a: %w: exit status 4", hosting.ErrGHNotAuthenticated), "run `gh auth login`"},
		"timeout":                      {fmt.Errorf("gh repo set-default o/a: %w", context.DeadlineExceeded), "gh did not answer"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			for _, n := range []string{"a", "b", "c"} {
				f.repo(t, "o", n, "main", false)
			}
			f.fake.DefaultRepoErr = c.err
			rec := &recorder{}
			s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"o/a", "o/b", "o/c"}}, 1, rec)
			if err != nil || s.Err() != nil || s.Count(StatusDone) != 3 {
				t.Fatalf("add must not fail because of gh: %+v, %v", s, err)
			}
			if n := len(f.fake.DefaultRepoCalls()); n != 1 {
				t.Fatalf("%d gh calls, want 1 (gh is not retried once unusable)", n)
			}
			warnings := messagesWith(rec.events, "gh default repository")
			if len(warnings) != 1 {
				t.Fatalf("want one warning, got %q", warnings)
			}
			w := warnings[0]
			for _, s := range []string{"warning: ", c.want, "sheaf adopt --recheck --all"} {
				if !strings.Contains(w, s) {
					t.Fatalf("warning %q lacks %q", w, s)
				}
			}
			if strings.Contains(w, "\n") {
				t.Fatalf("multi-line warning %q", w)
			}
			if n := strings.Count(w, "not authenticated"); n > 1 {
				t.Fatalf("warning repeats itself: %q", w)
			}
		})
	}
}

// A failure specific to a repository (e.g. no remote gh recognises) is
// warned about for that repository, with the fix, and does not stop the
// other repositories.
func TestGHDefaultOtherFailureWarnsPerRepo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.repo(t, "o", "b", "main", false)
	f.fake.DefaultRepoErr = errors.New("gh repo set-default: exit status 1: none of the git remotes configured for this repository point to a known GitHub host")
	rec := &recorder{}
	s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"o/a", "o/b"}}, 1, rec)
	if err != nil || s.Err() != nil || s.Count(StatusDone) != 2 {
		t.Fatalf("add: %+v, %v", s, err)
	}
	if n := len(f.fake.DefaultRepoCalls()); n != 2 {
		t.Fatalf("%d gh calls, want 2", n)
	}
	for _, n := range []string{"a", "b"} {
		fix := "fix: run `cd " + filepath.Join(f.w.Root, n, "main") + " && gh repo set-default o/" + n + "`"
		got := messagesWith(rec.events, fix)
		if len(got) != 1 || !strings.HasPrefix(got[0], "warning: ") || !strings.Contains(got[0], "known GitHub host") {
			t.Fatalf("warnings for %s: %q (all: %+v)", n, got, rec.events)
		}
	}
}

func adoptWith(t *testing.T, f *fixture, dir string, opts AdoptOptions) []Event {
	t.Helper()
	var evs []Event
	if _, err := Adopt(context.Background(), f.w, dir, opts, func(e Event) { evs = append(evs, e) }); err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestAdoptSetsGHDefaultRepo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
	adoptWith(t, f, dir, AdoptOptions{Hosting: f.fake, Host: "github.com"})
	r, _ := f.onDisk(t).Repository("x")
	wantDefaultCalls(t, f.fake, hostingtest.DefaultRepoCall{Dir: filepath.Join(dir, "main"), Slug: r.Slug})
}

// gh only knows GitHub: a repository of another host (here "local", a
// file:// origin) is left alone.
func TestAdoptNonGitHubDoesNotSetGHDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
	adoptWith(t, f, dir, AdoptOptions{Hosting: f.fake})
	wantDefaultCalls(t, f.fake)
}

func TestAdoptForkSetsReferenceAsGHDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	manualForkClone(t, fo, dir)
	adoptWith(t, f, dir, AdoptOptions{Hosting: f.fake, Host: "github.com"})
	wantDefaultCalls(t, f.fake, hostingtest.DefaultRepoCall{Dir: filepath.Join(dir, "main"), Slug: "upstream/repo"})
}

func TestAdoptFailureDoesNotSetGHDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
	testutil.Git(t, dir, "remote", "remove", "origin")
	if _, err := Adopt(context.Background(), f.w, dir, AdoptOptions{Hosting: f.fake, Host: "github.com", DefaultBranch: "main"}, noEmit); err == nil {
		t.Fatal("adopt without origin succeeded")
	}
	wantDefaultCalls(t, f.fake)
}

func TestAdoptAllGHUnusableWarnsOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var dirs []string
	for _, n := range []string{"a", "b"} {
		dir := filepath.Join(f.w.Root, n)
		plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
		dirs = append(dirs, dir)
	}
	f.fake.DefaultRepoErr = fmt.Errorf("%w: exit status 4", hosting.ErrGHNotAuthenticated)
	rec := &recorder{}
	s := AdoptAll(context.Background(), f.w, dirs, AdoptOptions{Hosting: f.fake, Host: "github.com"}, 1, rec)
	if s.Err() != nil || s.Count(StatusDone) != 2 {
		t.Fatalf("adopt must not fail because of gh: %+v", s)
	}
	if n := len(f.fake.DefaultRepoCalls()); n != 1 {
		t.Fatalf("%d gh calls, want 1", n)
	}
	if w := messagesWith(rec.events, "gh default repository"); len(w) != 1 || !strings.Contains(w[0], "gh auth login") {
		t.Fatalf("warnings = %q", w)
	}
}

// adopt --recheck sets the default of a repository registered before sheaf
// did it (unchanged entry), and the reference repository once a fork is
// recorded.
func TestRecheckSetsGHDefaultRepo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "z")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
	adoptWith(t, f, dir, AdoptOptions{Host: "github.com"}) // no hosting: as registered by an older sheaf
	wantDefaultCalls(t, f.fake)
	adoptWith(t, f, dir, AdoptOptions{Recheck: true, Hosting: f.fake})
	r, _ := f.onDisk(t).Repository("z")
	wantDefaultCalls(t, f.fake, hostingtest.DefaultRepoCall{Dir: filepath.Join(dir, "main"), Slug: r.Slug})
}

func TestRecheckRecordingForkSetsReferenceAsGHDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, fo.Fork, dir)
	testutil.Git(t, dir, "remote", "add", "upstream", fo.Upstream.URL) // not fetched: no fork recorded
	adoptWith(t, f, dir, AdoptOptions{Host: "github.com"})
	primary := filepath.Join(dir, "main")
	testutil.Git(t, primary, "fetch", "-q", "upstream")
	adoptWith(t, f, dir, AdoptOptions{Recheck: true, Hosting: f.fake, Host: "github.com"})
	if r, _ := f.onDisk(t).Repository("x"); !r.Forked() {
		t.Fatalf("fork not recorded: %+v", r)
	}
	wantDefaultCalls(t, f.fake, hostingtest.DefaultRepoCall{Dir: primary, Slug: "upstream/repo"})
}

// Several workers reaching gh at once: after the first "unusable" answer
// no further gh call starts and a single warning is printed. The fake holds
// each call until a second one arrives (or 2 s pass), so unserialized calls
// overlap for sure.
func TestGHDefaultUnusableWarnsOnceWithConcurrentJobs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var slugs []string
	for _, n := range []string{"a", "b", "c", "d"} {
		f.repo(t, "o", n, "main", false)
		slugs = append(slugs, "o/"+n)
	}
	f.fake.DefaultRepoErr = fmt.Errorf("%w: exit status 4", hosting.ErrGHNotAuthenticated)
	var mu sync.Mutex
	arrived, second := 0, make(chan struct{})
	f.fake.DefaultRepoHook = func(context.Context) {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(second)
		}
		mu.Unlock()
		select {
		case <-second:
		case <-time.After(2 * time.Second):
		}
	}
	rec := &recorder{}
	s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: slugs}, 4, rec)
	if err != nil || s.Err() != nil || s.Count(StatusDone) != 4 {
		t.Fatalf("add: %+v, %v", s, err)
	}
	if n := len(f.fake.DefaultRepoCalls()); n != 1 {
		t.Fatalf("%d gh calls, want 1", n)
	}
	if w := messagesWith(rec.events, "gh default repository not set"); len(w) != 1 {
		t.Fatalf("warnings = %q, want one", w)
	}
}

// An interrupted command says nothing about gh: neither when cancelled
// before the call nor when gh fails because of the cancellation; gh is not
// marked unusable either.
func TestGHDefaultCancelledIsQuiet(t *testing.T) {
	t.Parallel()
	entry := bundle.Repository{Name: "a", Slug: "o/a"}
	var evs []Event
	emit := func(e Event) { evs = append(evs, e) }

	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q")
	fake := hostingtest.NewFake()
	gd := newGHDefaults(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gd.set(ctx, dir, entry, emit)
	if n := len(fake.DefaultRepoCalls()); n != 0 || len(evs) != 0 {
		t.Fatalf("cancelled before: %d calls, events %+v", n, evs)
	}

	fake.DefaultRepoErr = fmt.Errorf("%w: exit status 4", hosting.ErrGHNotAuthenticated)
	ctx, cancel = context.WithCancel(context.Background())
	fake.DefaultRepoHook = func(context.Context) { cancel() }
	gd.set(ctx, dir, entry, emit)
	if len(evs) != 0 {
		t.Fatalf("cancelled during the call: events %+v", evs)
	}
	fake.DefaultRepoHook, fake.DefaultRepoErr = nil, nil
	gd.set(context.Background(), dir, entry, emit)
	if n := len(fake.DefaultRepoCalls()); n != 2 {
		t.Fatalf("%d calls, want 2: a cancellation must not mark gh unusable", n)
	}
}

// The per-repository fix is a command that can be pasted as is.
func TestGHDefaultFixQuotesThePath(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	base := t.TempDir()
	dir := filepath.Join(base, "b 7", "it's", "a", "main")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, dir, "init", "-q")
	fake := hostingtest.NewFake()
	fake.DefaultRepoErr = errors.New("gh repo set-default o/a: exit status 1: none of the git remotes configured for this repository point to a known GitHub host")
	var evs []Event
	newGHDefaults(fake).set(context.Background(), dir, bundle.Repository{Name: "a", Slug: "o/a"}, func(e Event) { evs = append(evs, e) })
	want := "fix: run `cd '" + base + "/b 7/it'\\''s/a/main' && gh repo set-default o/a`"
	if len(evs) != 1 || !strings.Contains(evs[0].Message, want) {
		t.Fatalf("events %+v, want %s", evs, want)
	}
	if strings.Count(evs[0].Message, "gh repo set-default o/a") != 1 {
		t.Fatalf("warning repeats the command: %q", evs[0].Message)
	}
}

// ghResolved sets remote.<remote>.gh-resolved in dir, as gh repo
// set-default does.
func ghResolved(t *testing.T, dir, remote, value string) {
	t.Helper()
	testutil.Git(t, dir, "config", "remote."+remote+".gh-resolved", value)
}

// The configuration gh wrote on the user's Mac (testdata/real_gh_resolved.txt,
// `git config --get-regexp gh-resolved` in a clone of their fork of
// smallrye/smallrye-mutiny after `gh repo set-default`) reads as that
// repository.
func TestReadGHDefaultRecordedConfig(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q")
	testutil.Git(t, dir, "remote", "add", "origin", "git@github.com:jponge/smallrye-mutiny.git")
	testutil.Git(t, dir, "remote", "add", "upstream", "https://github.com/smallrye/smallrye-mutiny.git")
	raw, err := os.ReadFile(filepath.Join("testdata", "real_gh_resolved.txt"))
	if err != nil {
		t.Fatal(err)
	}
	key, value, _ := strings.Cut(strings.TrimSpace(string(raw)), " ")
	testutil.Git(t, dir, "config", key, value)
	got, err := readGHDefault(context.Background(), dir)
	want := []ghDefaultEntry{{Remote: "upstream", Value: "base", Slug: "smallrye/smallrye-mutiny"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("readGHDefault = %+v, %v; want %+v", got, err, want)
	}
}

func TestReadGHDefaultForms(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	dir := t.TempDir()
	testutil.Git(t, dir, "init", "-q")
	if got, err := readGHDefault(context.Background(), dir); err != nil || len(got) != 0 {
		t.Fatalf("no default: %+v, %v", got, err)
	}
	testutil.Git(t, dir, "remote", "add", "my.fork", "git@github.com:me/x.git")
	ghResolved(t, dir, "my.fork", "base")
	ghResolved(t, dir, "origin", "Up/X") // another value: taken as a slug
	got, err := readGHDefault(context.Background(), dir)
	want := []ghDefaultEntry{{Remote: "my.fork", Value: "base", Slug: "me/x"}, {Remote: "origin", Value: "Up/X", Slug: "Up/X"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("readGHDefault = %+v, %v; want %+v", got, err, want)
	}
}

// An existing default is kept: the same repository needs nothing, another
// one (or several defaults) gets a note and no gh call.
func TestAdoptKeepsExistingGHDefault(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		setup func(t *testing.T, dir string)
		note  string // "" means no note
	}{
		"same":      {func(t *testing.T, dir string) { ghResolved(t, dir, "origin", "base") }, ""},
		"same case": {func(t *testing.T, dir string) { ghResolved(t, dir, "origin", "<SLUG>") }, ""},
		"other":     {func(t *testing.T, dir string) { ghResolved(t, dir, "origin", "me/other") }, "note: gh default repository of x is me/other (reference: <slug>); kept"},
		"several": {func(t *testing.T, dir string) {
			testutil.Git(t, dir, "remote", "add", "mirror", "git@github.com:me/up.git") // not "upstream": no fork detection
			ghResolved(t, dir, "mirror", "base")
			ghResolved(t, dir, "origin", "base")
		}, "note: several gh default repositories are configured for x (me/up via mirror, <slug> via origin); kept"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			dir := filepath.Join(f.w.Root, "x")
			plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
			_, slug, err := ParseRemoteURL(remoteURLOf(t, dir, "origin"))
			if err != nil {
				t.Fatal(err)
			}
			if name == "same case" {
				ghResolved(t, dir, "origin", strings.ToUpper(slug))
			} else {
				c.setup(t, dir)
			}
			evs := adoptWith(t, f, dir, AdoptOptions{Hosting: f.fake, Host: "github.com"})
			wantDefaultCalls(t, f.fake)
			var notes []string
			for _, e := range evs {
				if strings.Contains(e.Message, "gh default repositor") || strings.HasPrefix(e.Message, "note: ") {
					notes = append(notes, e.Message)
				}
			}
			if c.note == "" {
				if len(notes) != 0 {
					t.Fatalf("unexpected events %q", notes)
				}
				return
			}
			want := strings.ReplaceAll(c.note, "<slug>", slug)
			if len(notes) != 1 || notes[0] != want {
				t.Fatalf("events %q, want %q", notes, want)
			}
		})
	}
}

func TestRecheckKeepsOtherGHDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "z")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
	adoptWith(t, f, dir, AdoptOptions{Host: "github.com"})
	ghResolved(t, filepath.Join(dir, "main"), "origin", "me/z")
	evs := adoptWith(t, f, dir, AdoptOptions{Recheck: true, Hosting: f.fake})
	wantDefaultCalls(t, f.fake)
	if n := messagesWith(evs, "gh default repository of z is me/z"); len(n) != 1 {
		t.Fatalf("events %+v", evs)
	}
}

func TestDoctorGHDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, n := range []string{"none", "same", "other", "several", "several-other"} {
		f.repo(t, "o", n, "main", false)
	}
	if s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"o/none", "o/same", "o/other", "o/several", "o/several-other"}}, 2, Discard); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	p := func(n string) string { return filepath.Join(f.w.Root, n, "main") }
	ghResolved(t, p("same"), "origin", "O/Same")
	ghResolved(t, p("other"), "origin", "me/other")
	testutil.Git(t, p("several"), "remote", "add", "upstream", "git@github.com:me/several.git")
	ghResolved(t, p("several"), "origin", "o/several")
	ghResolved(t, p("several"), "upstream", "base")
	ghResolved(t, p("several-other"), "origin", "me/a")
	ghResolved(t, p("several-other"), "mirror", "me/b")
	cs := f.doctor(t, DoctorOptions{})
	c := wantCheck(t, cs, CheckWarn, "gh-default", "none", "no gh default repository")
	if want := "cd " + p("none") + " && gh repo set-default o/none"; !strings.Contains(c.Hint, want) {
		t.Fatalf("hint %q, want %q", c.Hint, want)
	}
	wantCheck(t, cs, CheckOK, "gh-default", "same", "O/Same")
	c = wantCheck(t, cs, CheckInfo, "gh-default", "other", "me/other")
	if !strings.Contains(c.Message, "o/other") {
		t.Fatalf("message %q lacks the reference", c.Message)
	}
	c = wantCheck(t, cs, CheckWarn, "gh-default", "several", "several gh default repositories")
	if !strings.Contains(c.Message, "me/several via upstream") || !strings.Contains(c.Hint, "config --unset remote.upstream.gh-resolved") {
		t.Fatalf("check %+v", c)
	}
	if strings.Contains(c.Hint, "remote.origin.gh-resolved") || strings.Contains(c.Hint, "gh repo set-default") {
		t.Fatalf("hint %q must keep the setting naming the repository", c.Hint)
	}
	c = wantCheck(t, cs, CheckWarn, "gh-default", "several-other", "me/b via mirror, me/a via origin")
	for _, want := range []string{"config --unset remote.mirror.gh-resolved", "config --unset remote.origin.gh-resolved", "gh repo set-default o/several-other"} {
		if !strings.Contains(c.Hint, want) {
			t.Fatalf("hint %q lacks %q", c.Hint, want)
		}
	}
}

func TestDoctorGHDefaultSkipsOtherHosts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "x")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), dir)
	adoptWith(t, f, dir, AdoptOptions{}) // host "local"
	for _, c := range f.doctor(t, DoctorOptions{}) {
		if c.Check == "gh-default" {
			t.Fatalf("gh-default check for a non-GitHub repository: %+v", c)
		}
	}
}

// The fix printed by doctor is pasteable: the path is quoted when needed.
func TestDoctorGHDefaultHintQuotesPath(t *testing.T) {
	t.Parallel()
	testutil.IsolateGit(t)
	root, err := Create(filepath.Join(t.TempDir(), "b 7"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := OpenWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{w: w, fake: hostingtest.NewFake()}
	f.repo(t, "o", "x", "main", false)
	if s, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"o/x"}}, 1, Discard); err != nil || s.Err() != nil {
		t.Fatalf("add: %+v, %v", s, err)
	}
	c := wantCheck(t, f.doctor(t, DoctorOptions{}), CheckWarn, "gh-default", "x", "no gh default repository")
	if want := "cd '" + filepath.Join(f.w.Root, "x", "main") + "' && gh repo set-default o/x"; !strings.Contains(c.Hint, want) {
		t.Fatalf("hint %q, want %q", c.Hint, want)
	}
}

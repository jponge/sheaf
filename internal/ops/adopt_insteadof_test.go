package ops

import (
	"path/filepath"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/testutil"
)

// insteadOf makes git rewrite URLs starting with prefix to base in dir
// (url.<base>.insteadOf, local configuration).
func insteadOf(t *testing.T, dir, base, prefix string) {
	t.Helper()
	testutil.Git(t, dir, "config", "--add", "url."+base+".insteadOf", prefix)
}

// noRemoteURLWarning fails when doctor warns about the URL of a remote of
// repo.
func noRemoteURLWarning(t *testing.T, f *fixture, repo string) {
	t.Helper()
	cs := f.doctor(t, DoctorOptions{})
	for _, c := range cs {
		if c.Repo == repo && (c.Check == "origin-url" || c.Check == "upstream-url") {
			t.Fatalf("unexpected %s check %+v\n%s", c.Check, c, dump(cs))
		}
	}
}

// adopt records origin's configured URL (remote.origin.url), not the URL
// git rewrites it to with url.<base>.insteadOf, and derives the host and
// slug from it; doctor then compares like with like. Neither a push URL nor
// a pushInsteadOf rule changes what is recorded.
func TestAdoptRecordsConfiguredURLDespiteInsteadOf(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "newcli")
	plainClone(t, o, dir)
	const url = "https://github.com/acme/newcli.git"
	testutil.Git(t, dir, "remote", "set-url", "origin", url)
	insteadOf(t, dir, o.URL, url)
	testutil.Git(t, dir, "config", "remote.origin.pushurl", "git@github.com:acme/pushed.git")
	testutil.Git(t, dir, "config", "url.file:///nowhere/.pushInsteadOf", "https://github.com/")
	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	r, ok := f.onDisk(t).Repository("newcli")
	want := bundle.Repository{Name: "newcli", Slug: "acme/newcli", URL: url, DefaultBranch: "main", Origin: bundle.OriginAdopted}
	if !ok || *r != want {
		t.Fatalf("entry = %+v\nwant    %+v", r, want)
	}
	if !r.IsGitHub() {
		t.Fatalf("entry %+v is not a GitHub repository", r)
	}
	noRemoteURLWarning(t, f, "newcli")
}

// The same for a fork: upstream and origin are recorded with their
// configured URLs. The default branch of upstream is asked to the remote,
// through the rewrite (git operations keep applying insteadOf).
func TestAdoptForkRecordsConfiguredURLsDespiteInsteadOf(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "newcli")
	manualForkClone(t, fo, dir)
	const upURL, forkURL = "https://github.com/acme/newcli.git", "git@github.com:me/newcli.git"
	testutil.Git(t, dir, "remote", "set-url", "upstream", upURL)
	testutil.Git(t, dir, "remote", "set-url", "origin", forkURL)
	insteadOf(t, dir, fo.Upstream.URL, upURL)
	insteadOf(t, dir, fo.Fork.URL, forkURL)
	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	r, ok := f.onDisk(t).Repository("newcli")
	want := bundle.Repository{Name: "newcli", Slug: "acme/newcli", URL: upURL, DefaultBranch: "main", Origin: bundle.OriginAdopted,
		Fork: bundle.Fork{Slug: "me/newcli", URL: forkURL}}
	if !ok || *r != want {
		t.Fatalf("entry = %+v\nwant    %+v", r, want)
	}
	noRemoteURLWarning(t, f, "newcli")
}

// A laid-out repository whose only checkout is named after neither branch
// is accepted when its origin's configured URL names the directory, even
// though git rewrites that URL to another repository name.
func TestAdoptLaidOutMatchesConfiguredOriginName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{}) // .../repo.git
	child := filepath.Join(f.w.Root, "newcli", "somewhere")
	plainClone(t, o, child)
	const url = "https://github.com/acme/newcli.git"
	testutil.Git(t, child, "remote", "set-url", "origin", url)
	insteadOf(t, child, o.URL, url)
	if _, err := adopt(t, f, filepath.Join(f.w.Root, "newcli"), AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	if r, ok := f.onDisk(t).Repository("newcli"); !ok || r.URL != url {
		t.Fatalf("entry = %+v, %v", r, ok)
	}
}

// adopt --recheck reads the configured URLs too.
func TestAdoptRecheckRecordsConfiguredForkURLDespiteInsteadOf(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo := testutil.NewForkedOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "newcli")
	plainClone(t, fo.Fork, dir)
	const upURL, forkURL = "https://github.com/acme/newcli.git", "git@github.com:me/newcli.git"
	testutil.Git(t, dir, "remote", "set-url", "origin", forkURL)
	insteadOf(t, dir, fo.Fork.URL, forkURL)
	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	primary := filepath.Join(dir, "main")
	testutil.Git(t, primary, "remote", "add", "upstream", upURL)
	insteadOf(t, primary, fo.Upstream.URL, upURL)
	testutil.Git(t, primary, "fetch", "-q", "upstream")
	if _, _, err := recheck(t, f, dir); err != nil {
		t.Fatal(err)
	}
	r, ok := f.onDisk(t).Repository("newcli")
	want := bundle.Repository{Name: "newcli", Slug: "acme/newcli", URL: upURL, DefaultBranch: "main", Origin: bundle.OriginAdopted,
		Fork: bundle.Fork{Slug: "me/newcli", URL: forkURL}}
	if !ok || *r != want {
		t.Fatalf("entry = %+v\nwant    %+v", r, want)
	}
	noRemoteURLWarning(t, f, "newcli")
}

// With several URLs (`git remote set-url --add`, mirrors), git fetches from
// the first one: adopt records it, and doctor compares the first one.
func TestAdoptRecordsFirstOfSeveralURLs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	dir := filepath.Join(f.w.Root, "proj")
	plainClone(t, o, dir)
	testutil.Git(t, dir, "remote", "set-url", "--add", "origin", "https://gitlab.com/mirror/proj.git")
	if _, err := adopt(t, f, dir, AdoptOptions{}); err != nil {
		t.Fatal(err)
	}
	if r, ok := f.onDisk(t).Repository("proj"); !ok || r.URL != o.URL || r.Slug != originSlug(t, o) || r.Host != bundle.HostLocal {
		t.Fatalf("entry = %+v, %v; want URL %s", r, ok, o.URL)
	}
	noRemoteURLWarning(t, f, "proj")
}

func TestDoctorComparesFirstOfSeveralURLs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	primary, o := f.clonedRepo(t, "a")
	// A mirror added after the recorded URL is not a mismatch.
	testutil.Git(t, primary, "remote", "set-url", "--add", "origin", "https://gitlab.com/mirror/a.git")
	noRemoteURLWarning(t, f, "a")
	// The recorded URL only as a mirror is one: git fetches from the first.
	testutil.Git(t, primary, "config", "--unset-all", "remote.origin.url")
	testutil.Git(t, primary, "config", "--add", "remote.origin.url", "file:///elsewhere/a.git")
	testutil.Git(t, primary, "config", "--add", "remote.origin.url", o.URL)
	wantCheck(t, f.doctor(t, DoctorOptions{}), CheckWarn, "origin-url", "a", "file:///elsewhere/a.git")
}

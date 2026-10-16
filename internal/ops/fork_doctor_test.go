package ops

import (
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestDoctorForkedRepositoryHealthy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.addForked(t, "x", testutil.OriginOpts{})
	cs := f.doctor(t, DoctorOptions{})
	for _, c := range cs {
		if c.Repo == "x" && c.Level >= CheckWarn {
			t.Fatalf("unexpected finding:\n%s", dump(cs))
		}
	}
}

func TestDoctorForkedRepositoryRemoteMismatches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	testutil.Git(t, primary, "remote", "set-url", "origin", fo.Upstream.URL)
	testutil.Git(t, primary, "remote", "remove", "upstream")
	cs := f.doctor(t, DoctorOptions{})
	c := wantCheck(t, cs, CheckWarn, "origin-url", "x", "your fork")
	if !strings.Contains(c.Hint, "remote set-url origin "+fo.Fork.URL) {
		t.Fatalf("hint = %q", c.Hint)
	}
	c = wantCheck(t, cs, CheckWarn, "upstream-url", "x", "no upstream remote")
	if !strings.Contains(c.Hint, "remote add upstream "+fo.Upstream.URL) {
		t.Fatalf("hint = %q", c.Hint)
	}
}

func TestDoctorForkedDefaultBranchTracking(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, primary := f.addForked(t, "x", testutil.OriginOpts{})
	testutil.Git(t, primary, "branch", "--set-upstream-to=origin/main", "main")
	cs := f.doctor(t, DoctorOptions{})
	c := wantCheck(t, cs, CheckWarn, "tracking", "x", "main tracks origin/main, not upstream/main")
	if !strings.Contains(c.Hint, "branch --set-upstream-to=upstream/main main") {
		t.Fatalf("hint = %q", c.Hint)
	}
}

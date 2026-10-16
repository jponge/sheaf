package ops

import (
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

func TestPRWorktreeForkSameRepositoryPRTracksUpstream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	testutil.Git(t, fo.Upstream.Path, "branch", "feat", "main")
	testutil.AdvanceOrigin(t, fo.Upstream, "feat", "feat.txt", "feat\n", "feat on upstream")
	// A stale branch of the same name on the fork must not be used.
	testutil.Git(t, fo.Fork.Path, "branch", "feat", "main")
	f.fake.AddPR("up", "x", hosting.PR{Number: 3, HeadBranch: "feat", HeadOwner: "up", HeadRepo: "x"})
	res, err := f.wtPR(t, "x", 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.FromPullRef || res.Branch != "feat" {
		t.Fatalf("result %+v", res)
	}
	if up := testutil.Git(t, res.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "upstream/feat" {
		t.Fatalf("upstream %q", up)
	}
	if head(t, res.Path, "HEAD") != head(t, fo.Upstream.Path, "feat") {
		t.Fatal("not at the upstream head")
	}
}

func TestPRWorktreeForkPRFromYourForkTracksOrigin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	testutil.Git(t, fo.Fork.Path, "branch", "topic", "main")
	testutil.AdvanceOrigin(t, fo.Fork, "topic", "t.txt", "t\n", "topic on the fork")
	f.fake.AddPR("up", "x", hosting.PR{Number: 5, HeadBranch: "topic", HeadOwner: "ME", HeadRepo: "x", IsCrossRepository: true})
	res, err := f.wtPR(t, "x", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.FromPullRef || res.Branch != "topic" {
		t.Fatalf("result %+v", res)
	}
	if up := testutil.Git(t, res.Path, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/topic" {
		t.Fatalf("upstream %q", up)
	}
}

func TestPRWorktreeForkPRFromOtherForkUsesUpstreamPullRef(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, primary := f.addForked(t, "x", testutil.OriginOpts{})
	oid := pushPullRef(t, fo.Upstream, 7, "main", "other.txt")
	f.fake.AddPR("up", "x", hosting.PR{Number: 7, HeadBranch: "main", HeadOwner: "someone", HeadRepo: "x", IsCrossRepository: true})
	res, err := f.wtPR(t, "x", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FromPullRef || res.Branch != "pr/7" || head(t, res.Path, "HEAD") != oid {
		t.Fatalf("result %+v", res)
	}
	if r := testutil.Git(t, primary, "config", "branch.pr/7.remote"); r != "upstream" {
		t.Fatalf("branch.pr/7.remote = %q", r)
	}
	// Refreshing works against upstream too.
	oid2 := pushPullRef(t, fo.Upstream, 7, oid, "more.txt")
	if _, err := f.wtPR(t, "x", 7); err != nil {
		t.Fatal(err)
	}
	if head(t, res.Path, "HEAD") != oid2 {
		t.Fatal("not refreshed from upstream")
	}
}

func TestPRWorktreeForkPRFromYourForkWithDeletedBranchUsesPullRef(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	fo, _ := f.addForked(t, "x", testutil.OriginOpts{})
	oid := pushPullRef(t, fo.Upstream, 8, "main", "gone.txt")
	f.fake.AddPR("up", "x", hosting.PR{Number: 8, HeadBranch: "deleted", HeadOwner: "me", HeadRepo: "x", IsCrossRepository: true})
	res, err := f.wtPR(t, "x", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FromPullRef || head(t, res.Path, "HEAD") != oid || len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "no longer exists on origin") {
		t.Fatalf("result %+v", res)
	}
}

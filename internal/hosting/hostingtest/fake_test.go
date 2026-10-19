package hostingtest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/hosting"
)

var _ hosting.Hosting = (*Fake)(nil)

func TestFakeListAndRepo(t *testing.T) {
	f := NewFake()
	f.AddRepo(hosting.Repo{Owner: "o", Name: "r1", CloneURL: "file:///tmp/r1.git", DefaultBranch: "main"})
	f.AddRepo(hosting.Repo{Owner: "o", Name: "r2", Archived: true})
	ctx := context.Background()

	rs, err := f.ListRepos(ctx, "o")
	if err != nil || len(rs) != 2 || rs[0].CloneURL != "file:///tmp/r1.git" {
		t.Fatalf("ListRepos = %v, %v", rs, err)
	}
	rs[0].Name = "mutated"
	if again, _ := f.ListRepos(ctx, "o"); again[0].Name != "r1" {
		t.Fatalf("ListRepos must return a copy")
	}
	r, err := f.Repo(ctx, "o", "r2")
	if err != nil || !r.Archived {
		t.Fatalf("Repo = %v, %v", r, err)
	}
}

func TestFakeNotFound(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if _, err := f.ListRepos(ctx, "nobody"); !errors.Is(err, hosting.ErrNotFound) {
		t.Fatalf("ListRepos err = %v", err)
	}
	if _, err := f.Repo(ctx, "o", "x"); !errors.Is(err, hosting.ErrNotFound) {
		t.Fatalf("Repo err = %v", err)
	}
	if _, err := f.PR(ctx, "o", "x", 1); !errors.Is(err, hosting.ErrNotFound) {
		t.Fatalf("PR err = %v", err)
	}
}

func TestFakePR(t *testing.T) {
	f := NewFake()
	f.AddPR("o", "r", hosting.PR{Number: 3, HeadBranch: "b"})
	p, err := f.PR(context.Background(), "o", "r", 3)
	if err != nil || p.HeadBranch != "b" {
		t.Fatalf("PR = %v, %v", p, err)
	}
}

func TestFakeInjectedErrors(t *testing.T) {
	boom := errors.New("boom")
	f := NewFake()
	f.AddRepo(hosting.Repo{Owner: "o", Name: "r"})
	f.ListErr["o"] = boom
	f.RepoErr["o/r"] = boom
	f.PRErr["o/r"] = boom
	ctx := context.Background()
	if _, err := f.ListRepos(ctx, "o"); !errors.Is(err, boom) {
		t.Fatalf("ListRepos err = %v", err)
	}
	if _, err := f.Repo(ctx, "o", "r"); !errors.Is(err, boom) {
		t.Fatalf("Repo err = %v", err)
	}
	if _, err := f.PR(ctx, "o", "r", 1); !errors.Is(err, boom) {
		t.Fatalf("PR err = %v", err)
	}
}

func TestFakeEmptyOwnerIsNotAnError(t *testing.T) {
	f := NewFake()
	f.AddOwner("empty")
	rs, err := f.ListRepos(context.Background(), "empty")
	if err != nil || len(rs) != 0 {
		t.Fatalf("got %v, %v", rs, err)
	}
}

func TestFakeCaseInsensitiveReturnsCanonical(t *testing.T) {
	f := NewFake()
	f.AddRepo(hosting.Repo{Owner: "SmallRye", Name: "Boiler-Vroom"})
	f.AddPR("SmallRye", "Boiler-Vroom", hosting.PR{Number: 1})
	ctx := context.Background()
	rs, err := f.ListRepos(ctx, "smallrye")
	if err != nil || len(rs) != 1 || rs[0].Owner != "SmallRye" {
		t.Fatalf("ListRepos = %v, %v", rs, err)
	}
	r, err := f.Repo(ctx, "SMALLRYE", "boiler-vroom")
	if err != nil || r.Owner != "SmallRye" || r.Name != "Boiler-Vroom" {
		t.Fatalf("Repo = %+v, %v", r, err)
	}
	if _, err := f.PR(ctx, "smallrye", "BOILER-VROOM", 1); err != nil {
		t.Fatal(err)
	}
	f.RepoErr["smallrye/boiler-vroom"] = errors.New("boom")
	if _, err := f.Repo(ctx, "SmallRye", "Boiler-Vroom"); err == nil {
		t.Fatal("injected error (lower-case key) must apply")
	}
}

func TestFakeValidatesInputLikeGH(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if _, err := f.ListRepos(ctx, "-x"); !errors.Is(err, hosting.ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.Repo(ctx, "o", ".."); !errors.Is(err, hosting.ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.PR(ctx, "o", "r", 0); !errors.Is(err, hosting.ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
}

func TestFakeAcceptsDotGithubAndUnderscoreOwner(t *testing.T) {
	f := NewFake()
	f.AddRepo(hosting.Repo{Owner: "smallrye", Name: ".github"})
	f.AddRepo(hosting.Repo{Owner: "name_shortcode", Name: "-odd"})
	ctx := context.Background()
	if _, err := f.Repo(ctx, "smallrye", ".github"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Repo(ctx, "name_shortcode", "-odd"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ListRepos(ctx, "name_shortcode"); err != nil {
		t.Fatal(err)
	}
}

func TestFakeMixedCaseErrorKeys(t *testing.T) {
	boom := errors.New("boom")
	f := NewFake()
	f.AddRepo(hosting.Repo{Owner: "SmallRye", Name: "X"})
	f.ListErr["SmallRye"] = boom
	f.RepoErr["SmallRye/X"] = boom
	f.PRErr["SmallRye/X"] = boom
	ctx := context.Background()
	if _, err := f.ListRepos(ctx, "smallrye"); !errors.Is(err, boom) {
		t.Fatalf("ListRepos err = %v", err)
	}
	if _, err := f.Repo(ctx, "smallrye", "x"); !errors.Is(err, boom) {
		t.Fatalf("Repo err = %v", err)
	}
	if _, err := f.PR(ctx, "smallrye", "x", 1); !errors.Is(err, boom) {
		t.Fatalf("PR err = %v", err)
	}
}

func TestFakeForks(t *testing.T) {
	t.Parallel()
	f := NewFake()
	ctx := context.Background()
	login, err := f.CurrentUser(ctx)
	if err != nil || login == "" {
		t.Fatalf("default CurrentUser = %q, %v", login, err)
	}
	if forks, err := f.ListForks(ctx, login); err != nil || len(forks) != 0 {
		t.Fatalf("no forks registered: %v, %v", forks, err)
	}
	f.User = "jponge"
	fk := hosting.Fork{Repo: hosting.Repo{Owner: "jponge", Name: "x-renamed", CloneURL: "file:///f.git", Fork: true}, ParentOwner: "up", ParentName: "x"}
	f.AddFork(fk)
	forks, err := f.ListForks(ctx, "JPonge")
	if err != nil || len(forks) != 1 || forks[0] != fk {
		t.Fatalf("ListForks = %+v, %v", forks, err)
	}
	forks[0].Name = "mutated"
	if again, _ := f.ListForks(ctx, "jponge"); again[0].Name != "x-renamed" {
		t.Fatal("ListForks must return a copy")
	}
	boom := errors.New("boom")
	f.UserErr = boom
	if _, err := f.CurrentUser(ctx); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	f.ForksErr = boom
	if _, err := f.ListForks(ctx, "jponge"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if f.ForkCalls() != 4 {
		t.Fatalf("ForkCalls = %d", f.ForkCalls())
	}
}

func TestFakeSetDefaultRepoRecordsCalls(t *testing.T) {
	t.Parallel()
	f := NewFake()
	ctx := context.Background()
	if err := f.SetDefaultRepo(ctx, "/b/x/main", "Up", "x"); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	f.DefaultRepoErr = boom
	if err := f.SetDefaultRepo(ctx, "/b/y/main", "o", "y"); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range []struct{ dir, owner, name string }{{"", "o", "r"}, {"rel", "o", "r"}, {"/b/z", "-o", "r"}, {"/b/z", "o", ".."}} {
		if err := f.SetDefaultRepo(ctx, c.dir, c.owner, c.name); !errors.Is(err, hosting.ErrInvalidInput) {
			t.Errorf("SetDefaultRepo(%q, %q, %q) = %v, want ErrInvalidInput", c.dir, c.owner, c.name, err)
		}
	}
	want := []DefaultRepoCall{{Dir: "/b/x/main", Slug: "Up/x"}, {Dir: "/b/y/main", Slug: "o/y"}}
	if got := f.DefaultRepoCalls(); !slices.Equal(got, want) {
		t.Fatalf("calls = %+v\nwant    %+v", got, want)
	}
}

func TestFakeBrowseRecordsCalls(t *testing.T) {
	t.Parallel()
	f := NewFake()
	ctx := context.Background()
	if err := f.Browse(ctx, "o", "x", hosting.Page{PR: 3}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	f.BrowseErr = boom
	if err := f.Browse(ctx, "o", "y", hosting.Page{Branch: "feat"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := f.Browse(ctx, "o", "z", hosting.Page{Branch: "-x"}); !errors.Is(err, hosting.ErrInvalidInput) {
		t.Fatalf("invalid branch: err = %v", err)
	}
	want := []BrowseCall{{Slug: "o/x", Page: hosting.Page{PR: 3}}, {Slug: "o/y", Page: hosting.Page{Branch: "feat"}}}
	if got := f.BrowseCalls(); !slices.Equal(got, want) {
		t.Fatalf("calls = %+v\nwant    %+v", got, want)
	}
}

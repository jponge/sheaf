package hosting

import (
	"context"
	"errors"
	"testing"
)

var _ Hosting = (*Fake)(nil)
var _ Hosting = (*GH)(nil)

func TestFakeListAndRepo(t *testing.T) {
	f := NewFake()
	f.AddRepo(Repo{Owner: "o", Name: "r1", CloneURL: "file:///tmp/r1.git", DefaultBranch: "main"})
	f.AddRepo(Repo{Owner: "o", Name: "r2", Archived: true})
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
	if _, err := f.ListRepos(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ListRepos err = %v", err)
	}
	if _, err := f.Repo(ctx, "o", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Repo err = %v", err)
	}
	if _, err := f.PR(ctx, "o", "x", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PR err = %v", err)
	}
}

func TestFakePR(t *testing.T) {
	f := NewFake()
	f.AddPR("o", "r", PR{Number: 3, HeadBranch: "b"})
	p, err := f.PR(context.Background(), "o", "r", 3)
	if err != nil || p.HeadBranch != "b" {
		t.Fatalf("PR = %v, %v", p, err)
	}
}

func TestFakeInjectedErrors(t *testing.T) {
	boom := errors.New("boom")
	f := NewFake()
	f.AddRepo(Repo{Owner: "o", Name: "r"})
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
	f.AddRepo(Repo{Owner: "SmallRye", Name: "Boiler-Vroom"})
	f.AddPR("SmallRye", "Boiler-Vroom", PR{Number: 1})
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
	if _, err := f.ListRepos(ctx, "-x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.Repo(ctx, "o", ".."); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.PR(ctx, "o", "r", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
}

func TestFakeAcceptsDotGithubAndUnderscoreOwner(t *testing.T) {
	f := NewFake()
	f.AddRepo(Repo{Owner: "smallrye", Name: ".github"})
	f.AddRepo(Repo{Owner: "name_shortcode", Name: "-odd"})
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
	f.AddRepo(Repo{Owner: "SmallRye", Name: "X"})
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

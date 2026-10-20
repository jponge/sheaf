package hosting

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	userArgs  = "api user --jq .login"
	forksArgs = "repo list jponge --fork --limit 1001 --json name,owner,parent,url,sshUrl,isFork"
)

func TestGHCurrentUserReal(t *testing.T) {
	// Not parallel: writes an executable stub (ETXTBSY with concurrent forks).
	path, calls := stubGH(t, map[string]resp{"api user": {fixture: "real_user_login.txt"}})
	gh := NewGH(GHOptions{Path: path, Protocol: "ssh"})
	login, err := gh.CurrentUser(context.Background())
	if err != nil || login != "jponge" {
		t.Fatalf("CurrentUser = %q, %v", login, err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{userArgs}) {
		t.Fatalf("calls = %q", got)
	}
}

func TestGHCurrentUserFailures(t *testing.T) {
	// Not parallel: writes an executable stub (ETXTBSY with concurrent forks).
	t.Run("not logged in", func(t *testing.T) {
		path, _ := stubGH(t, map[string]resp{"api user": {stderr: "gh auth login required", code: 4}})
		// gh's stderr is kept in the *GHError (its text is one line, see
		// TestGHErrorsOneLine).
		_, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).CurrentUser(context.Background())
		if ge, ok := errors.AsType[*GHError](err); err == nil || !ok || ge.Stderr != "gh auth login required" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty output", func(t *testing.T) {
		path, _ := stubGH(t, map[string]resp{"api user": {}})
		if _, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).CurrentUser(context.Background()); err == nil {
			t.Fatal("empty login accepted")
		}
	})
	t.Run("invalid login", func(t *testing.T) {
		path, _ := stubGH(t, map[string]resp{"api user": {stdout: "-x y"}})
		if _, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).CurrentUser(context.Background()); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestGHListForksReal(t *testing.T) {
	// Not parallel: writes an executable stub (ETXTBSY with concurrent forks).
	for _, proto := range []string{"ssh", "https"} {
		t.Run(proto, func(t *testing.T) {
			path, calls := stubGH(t, map[string]resp{"repo list": {fixture: "real_repo_list_forks.json"}})
			forks, err := NewGH(GHOptions{Path: path, Protocol: proto}).ListForks(context.Background(), "jponge")
			if err != nil {
				t.Fatal(err)
			}
			if got := calls(); !reflect.DeepEqual(got, []string{forksArgs}) {
				t.Fatalf("calls = %q", got)
			}
			if len(forks) != 3 {
				t.Fatalf("forks = %+v", forks)
			}
			want := Fork{
				Repo: Repo{Owner: "jponge", Name: "smallrye-mutiny",
					HTTPSURL: "https://github.com/jponge/smallrye-mutiny.git", SSHURL: "git@github.com:jponge/smallrye-mutiny.git", Fork: true},
				ParentOwner: "smallrye", ParentName: "smallrye-mutiny",
			}
			want.CloneURL = want.HTTPSURL
			if proto == "ssh" {
				want.CloneURL = want.SSHURL
			}
			if forks[0] != want {
				t.Fatalf("got  %+v\nwant %+v", forks[0], want)
			}
			if forks[1].ParentSlug() != "quarkusio/quarkus" || forks[1].Slug() != "jponge/quarkus" {
				t.Fatalf("forks[1] = %+v", forks[1])
			}
		})
	}
}

func TestGHListForksSkipsEntriesWithoutParent(t *testing.T) {
	// Not parallel: writes an executable stub (ETXTBSY with concurrent forks).
	path, _ := stubGH(t, map[string]resp{"repo list": {fixture: "forks_mixed.json"}})
	forks, err := NewGH(GHOptions{Path: path, Protocol: "https"}).ListForks(context.Background(), "jponge")
	if err != nil {
		t.Fatal(err)
	}
	if len(forks) != 1 || forks[0].Name != "x-renamed" || forks[0].ParentSlug() != "Upstream-Org/x" {
		t.Fatalf("forks = %+v", forks)
	}
}

func TestGHListForksTruncatedAndErrors(t *testing.T) {
	// Not parallel: writes an executable stub (ETXTBSY with concurrent forks).
	t.Run("truncated", func(t *testing.T) {
		path, calls := stubGH(t, map[string]resp{"repo list": {fixture: "real_repo_list_forks.json"}})
		forks, err := NewGH(GHOptions{Path: path, Protocol: "ssh", Limit: 2}).ListForks(context.Background(), "jponge")
		if !errors.Is(err, ErrPossiblyTruncated) || len(forks) != 2 {
			t.Fatalf("got %d, %v", len(forks), err)
		}
		if got := calls(); len(got) != 1 || !strings.Contains(got[0], "--limit 3 ") {
			t.Fatalf("calls = %q", got)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		path, _ := stubGH(t, map[string]resp{"repo list": {fixture: "malformed.json"}})
		if _, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).ListForks(context.Background(), "jponge"); err == nil {
			t.Fatal("malformed JSON accepted")
		}
	})
	t.Run("invalid owner", func(t *testing.T) {
		if _, err := NewGH(GHOptions{Path: "/nonexistent/gh", Protocol: "ssh"}).ListForks(context.Background(), "-x"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestGHRealRepoViewNonForkHasNoParent(t *testing.T) {
	// Not parallel: writes an executable stub (ETXTBSY with concurrent forks).
	path, _ := stubGH(t, map[string]resp{"repo view": {fixture: "real_repo_view_nonfork.json"}})
	r, err := NewGH(GHOptions{Path: path, Protocol: "https"}).Repo(context.Background(), "smallrye", "smallrye-mutiny")
	if err != nil || r.Fork || r.Owner != "smallrye" || r.Name != "smallrye-mutiny" {
		t.Fatalf("Repo = %+v, %v", r, err)
	}
}

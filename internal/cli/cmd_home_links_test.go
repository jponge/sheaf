package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/ops"
	"github.com/jponge/sheaf/internal/tui"
)

// The rows of the overview link the repository name to its page on GitHub
// (the reference repository for a clone of a fork, as browse opens it) and
// "PR #<n>" to the pull request (B46); nothing for a repository on another
// host (even one that gh may reach through an alias, whose web address is
// unknown) or unknown to the bundle.
func TestHomeLinks(t *testing.T) {
	t.Parallel()
	gh := &bundle.Repository{Name: "alpha", Slug: "o/alpha"}
	fork := &bundle.Repository{Name: "alpha", Slug: "o/alpha", Fork: bundle.Fork{Slug: "me/alpha", URL: "file:///fork"}}
	repoLink := tui.Link{Cell: 0, Text: "alpha", URL: "https://github.com/o/alpha"}
	clean := &ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "alpha"}, Status: &gitx.RepoStatus{Branch: "main", Upstream: "origin/main"}}
	pr := &ops.CheckoutStatus{Checkout: ops.Checkout{Repo: "alpha"}, Status: &gitx.RepoStatus{Branch: "pr/7"}, PullRequest: &ops.PullRequestHead{Number: 7}}
	for _, tc := range []struct {
		name string
		r    *bundle.Repository
		c    *ops.CheckoutStatus
		want []tui.Link
	}{
		{"github", gh, clean, []tui.Link{repoLink}},
		{"fork", fork, clean, []tui.Link{repoLink}},
		{"problem row", gh, nil, []tui.Link{repoLink}},
		{"pull request", gh, pr, []tui.Link{repoLink, {Cell: 3, Text: "PR #7", URL: "https://github.com/o/alpha/pull/7"}}},
		{"pull request of a fork's clone", fork, pr, []tui.Link{repoLink, {Cell: 3, Text: "PR #7", URL: "https://github.com/o/alpha/pull/7"}}},
		{"other host", &bundle.Repository{Name: "alpha", Slug: "o/alpha", Host: "gitlab.com"}, pr, nil},
		{"ssh alias", &bundle.Repository{Name: "alpha", Slug: "o/alpha", Host: "gh"}, pr, nil},
		{"unknown repository", nil, clean, nil},
	} {
		if got := homeLinks(tc.r, tc.c); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: links %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

// Without a terminal (the output of the in-process home screen is a
// buffer), the overview renders no hyperlink (B46).
func TestHomeNoHyperlinksWithoutTerminal(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	errs, code := runHomeWith(t, e.deps(), newScript(t, false, ovKeys(t, &out, "STATE", 1, "q")), &out)
	if code != 0 || !strings.Contains(out.String(), "alpha") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if strings.Contains(out.String(), "\x1b]8;") {
		t.Errorf("hyperlinks without a terminal:\n%q", out.String())
	}
}

// The overview of the home screen renders hyperlinks only with terminal
// controls (see terminalControls), like the window title and the mouse.
func TestHomeOverviewHyperlinksFollowTerminalControls(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	for _, controls := range []bool{false, true} {
		h := &home{d: e.deps(), s: &tuiSession{ctx: context.Background(), controls: controls}, root: e.cwd, jobs: 1}
		ov, err := h.overview()
		if err != nil {
			t.Fatal(err)
		}
		raw := ov.View().Content
		if got := strings.Contains(raw, "\x1b]8;;https://github.com/o/alpha\x07alpha"); got != controls {
			t.Errorf("controls %v: hyperlink %v:\n%q", controls, got, raw)
		}
	}
}

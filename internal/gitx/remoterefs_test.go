package gitx

import (
	"context"
	"slices"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

func TestParseRemotes(t *testing.T) {
	t.Parallel()
	out := "remote.origin.url\nfile:///o\x00remote.origin.fetch\n+refs/heads/*:refs/remotes/origin/*\x00" +
		"remote.a.b.url\nu\x00remote.a.b.skipfetchall\x00" + // implicit true
		"remote.c.url\nu\x00remote.c.skipfetchall\nfalse\x00" +
		"remote.d.url\nu\x00remote.d.skipfetchall\nmaybe\x00" + // invalid: skipped
		"remote.pushdefault\norigin\x00"
	m := parseRemotes(out)
	if len(m) != 4 || m["origin"].fetch[0] != "+refs/heads/*:refs/remotes/origin/*" || m["origin"].urls[0] != "file:///o" {
		t.Fatalf("parsed %+v", m)
	}
	if !m["a.b"].skipFetchAll || m["c"].skipFetchAll || !m["d"].skipFetchAll {
		t.Fatalf("skipFetchAll: a.b %v c %v d %v", m["a.b"].skipFetchAll, m["c"].skipFetchAll, m["d"].skipFetchAll)
	}
	if got := fetchAllRemotes(m); !slices.Equal(got, []string{"c", "origin"}) {
		t.Fatalf("fetchAllRemotes = %v", got)
	}
}

// The at-risk set of a remote keeps every ref it may have written.
func TestRemoteNamespaceRefs(t *testing.T) {
	t.Parallel()
	_, dir := cloneOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, dir, "config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/remotes/mirror/*")
	testutil.Git(t, dir, "config", "--add", "remote.origin.fetch", "refs/heads/x:refs/remotes/pinned/x")
	testutil.Git(t, dir, "config", "--add", "remote.origin.fetch", "^refs/heads/old")
	s, err := RemoteNamespaceRefs(context.Background(), dir, "origin")
	if err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string]bool{
		"refs/remotes/origin/a/b":  true,
		"refs/remotes/mirror/main": true,
		"refs/remotes/pinned/x":    true,
		"refs/remotes/other/main":  false,
	} {
		if got := s.Contains(ref); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ref, got, want)
		}
	}
}

func TestRefSetOf(t *testing.T) {
	t.Parallel()
	conf := map[string]*remoteConf{
		"origin":   {urls: []string{"u"}, fetch: []string{"+refs/heads/*:refs/remotes/origin/*", "refs/heads/main:refs/remotes/pinned/main", "refs/tags/*:refs/tags/*"}},
		"origin/x": {urls: []string{"u"}, fetch: []string{"+refs/heads/*:refs/remotes/origin/x/*"}},
		"neg":      {urls: []string{"u"}, fetch: []string{"+refs/heads/*:refs/remotes/neg/*", "^refs/heads/old"}},
		"nourl":    {fetch: []string{"+refs/heads/*:refs/remotes/nourl/*"}},
		"pin":      {urls: []string{"u"}, fetch: []string{"refs/heads/x:refs/remotes/origin/x/pinned"}},
	}
	s := refSetOf(conf, []string{"origin", "neg", "nourl", "pin", "missing"})
	for ref, want := range map[string]bool{
		"refs/remotes/origin/main":     true,
		"refs/remotes/origin/a/b":      true,
		"refs/remotes/pinned/main":     true,
		"refs/remotes/pinned/other":    false,
		"refs/remotes/origin/x/work":   false, // the remote origin/x's
		"refs/remotes/origin/x/pinned": false, // pin's, but inside origin/x's namespace
		"refs/remotes/neg/main":        false, // negative refspec
		"refs/remotes/nourl/main":      false, // no URL
		"refs/remotes/missing/main":    false, // not configured
		"refs/tags/v1":                 false, // not a remote-tracking ref
	} {
		if got := s.Contains(ref); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ref, got, want)
		}
	}
	lead, revs := s.RevListArgs()
	if !slices.Equal(lead, []string{"--ignore-missing"}) || !slices.Contains(revs, "--glob=refs/remotes/origin/*") ||
		!slices.Contains(revs, "--exclude=refs/remotes/origin/x/*") || !slices.Contains(revs, "refs/remotes/pinned/main") ||
		slices.Contains(revs, "refs/remotes/origin/x/pinned") {
		t.Fatalf("RevListArgs = %v %v", lead, revs)
	}
	if lead, revs := refSetOf(conf, nil).RevListArgs(); lead != nil || revs != nil {
		t.Fatalf("empty set: %v %v", lead, revs)
	}
}

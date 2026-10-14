package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/testutil"
)

func TestParseRemoteURLGitHubAliasesAndSSHConfigHosts(t *testing.T) {
	for in, want := range map[string]string{
		"ssh://git@ssh.github.com:443/o/x.git": "github.com",
		"https://www.github.com/o/x":           "github.com",
		"git@github_work:o/x.git":              "github_work", // SSH config alias: recorded as-is
		"git@github-work:o/x.git":              "github-work",
	} {
		h, s, err := ParseRemoteURL(in)
		if err != nil || h != want || s != "o/x" {
			t.Errorf("ParseRemoteURL(%q) = %q, %q, %v; want host %q", in, h, s, err, want)
		}
	}
}

func TestAdoptHostAliasAndOverride(t *testing.T) {
	f := newFixture(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	alias := filepath.Join(f.w.Root, "alias")
	plainClone(t, o, alias)
	testutil.Git(t, alias, "remote", "set-url", "origin", "git@github_work:o/alias.git")
	if _, err := adopt(t, f, alias, AdoptOptions{DefaultBranch: "main"}); err != nil {
		t.Fatalf("adopt with an SSH alias: %v", err)
	}
	over := filepath.Join(f.w.Root, "over")
	plainClone(t, o, over)
	testutil.Git(t, over, "remote", "set-url", "origin", "git@github_work:o/over.git")
	if _, err := adopt(t, f, over, AdoptOptions{DefaultBranch: "main", Host: "github.com"}); err != nil {
		t.Fatalf("adopt --host github.com: %v", err)
	}
	b := f.onDisk(t)
	if r, _ := b.Repository("alias"); r == nil || r.Host != "github_work" || r.IsGitHub() {
		t.Fatalf("alias = %+v, want host github_work", r)
	}
	if r, _ := b.Repository("over"); r == nil || r.Host != "" || !r.IsGitHub() {
		t.Fatalf("over = %+v, want github.com (stored empty)", r)
	}

	bad := filepath.Join(f.w.Root, "bad")
	plainClone(t, o, bad)
	_, err := adopt(t, f, bad, AdoptOptions{Host: "bad host"})
	if err == nil || !strings.Contains(err.Error(), "bad host") {
		t.Fatalf("err = %v, want invalid host refusal", err)
	}
	if !exists(filepath.Join(bad, ".git")) {
		t.Fatal("moved despite an invalid host")
	}
}

// NIT: the post-confirmation re-check ignores informational changes.
func TestRemoveRecheckIgnoresInformationalChanges(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	writeFile(t, filepath.Join(primary, ".git", "info", "exclude"), "*.log\n")
	writeFile(t, filepath.Join(primary, "a.log"), "x")
	if _, err := f.remove(t, "a", false, func(RemovePlan) bool {
		writeFile(t, filepath.Join(primary, "b.log"), "build output appeared meanwhile")
		return true
	}); err != nil {
		t.Fatalf("remove aborted by an informational change: %v", err)
	}
}

// NIT: the working directory comes from the caller.
func TestRemoveUsesGivenWorkingDir(t *testing.T) {
	f := newFixture(t)
	primary, _ := f.clonedRepo(t, "a")
	_, err := f.removeOpts(t, "a", RemoveOptions{Force: true, Confirm: yes, WorkingDir: filepath.Join(primary, "sub")})
	if err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("err = %v, want refusal about the working directory", err)
	}
}

// NIT: create refuses nested bundles but allows a work tree.
func TestCreateRefusesNestedBundle(t *testing.T) {
	testutil.IsolateGit(t)
	outer, err := Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(filepath.Join(outer, "inner")); err == nil || !strings.Contains(err.Error(), outer) {
		t.Fatalf("err = %v, want refusal naming %s", err, outer)
	}
	if exists(filepath.Join(outer, "inner", bundle.FileName)) {
		t.Fatal("nested sheaf.yaml written")
	}
	clone := filepath.Join(t.TempDir(), "c")
	plainClone(t, testutil.NewOrigin(t, testutil.OriginOpts{}), clone)
	if _, err := Create(filepath.Join(clone, "fresh")); err != nil {
		t.Fatalf("create inside a work tree: %v", err)
	}
	_ = context.Background
}

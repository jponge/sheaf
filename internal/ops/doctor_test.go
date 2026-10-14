package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

func goodGit(context.Context) (gitx.GitVersion, error) {
	return gitx.GitVersion{Major: 2, Minor: 47}, nil
}
func goodGH(context.Context) error { return nil }

func (f *fixture) doctor(t *testing.T, opts DoctorOptions) []Check {
	t.Helper()
	if opts.Root == "" && opts.RootErr == nil {
		opts.Root = f.w.Root
	}
	if opts.GitVersion == nil {
		opts.GitVersion = goodGit
	}
	if opts.GH == nil {
		opts.GH = goodGH
	}
	return Doctor(context.Background(), opts)
}

func dump(cs []Check) string {
	var sb strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&sb, "%-4v %s %s: %s (%s)\n", c.Level, c.Check, c.Repo, c.Message, c.Hint)
	}
	return sb.String()
}

// findCheck returns the first check with that name and repository whose
// message contains substr.
func findCheck(cs []Check, name, repo, substr string) (Check, bool) {
	for _, c := range cs {
		if c.Check == name && c.Repo == repo && strings.Contains(c.Message, substr) {
			return c, true
		}
	}
	return Check{}, false
}

func wantCheck(t *testing.T, cs []Check, level CheckLevel, name, repo, substr string) Check {
	t.Helper()
	c, ok := findCheck(cs, name, repo, substr)
	if !ok || c.Level != level {
		t.Fatalf("want %v %s %s %q; got %+v, %v\n%s", level, name, repo, substr, c, ok, dump(cs))
	}
	return c
}

func TestCheckLevelText(t *testing.T) {
	for l, want := range map[CheckLevel]string{CheckOK: "ok", CheckInfo: "info", CheckWarn: "warn", CheckFail: "fail"} {
		if b, _ := l.MarshalText(); string(b) != want || l.String() != want {
			t.Errorf("%d: %q", int(l), b)
		}
	}
}

func TestDoctorHealthyBundle(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.mustAddWT(t, "a", "feat")
	// A url.insteadOf rewrite is not a mismatch: the configured URL counts.
	testutil.Git(t, filepath.Join(f.w.Root, "a", "main"), "config", "url.file:///rewritten.insteadOf", "file://")
	cs := f.doctor(t, DoctorOptions{})
	if DoctorFailed(cs) {
		t.Fatalf("failed:\n%s", dump(cs))
	}
	for _, c := range cs {
		if c.Level == CheckWarn {
			t.Errorf("unexpected warning %+v", c)
		}
	}
	wantCheck(t, cs, CheckOK, "git", "", "2.47.0")
	wantCheck(t, cs, CheckOK, "gh", "", "")
	wantCheck(t, cs, CheckOK, "bundle", "", f.w.Root)
	wantCheck(t, cs, CheckOK, "primary", "a", filepath.Join(f.w.Root, "a", "main"))
}

func TestDoctorEnvironment(t *testing.T) {
	f := newFixture(t)
	old := func(context.Context) (gitx.GitVersion, error) { return gitx.GitVersion{Major: 2, Minor: 30}, nil }
	cs := f.doctor(t, DoctorOptions{GitVersion: old, GH: func(context.Context) error { return hosting.ErrGHNotFound }})
	wantCheck(t, cs, CheckFail, "git", "", "2.30.0")
	wantCheck(t, cs, CheckWarn, "gh", "", "not found")
	if !DoctorFailed(cs) {
		t.Fatal("not failed")
	}
	cs = f.doctor(t, DoctorOptions{GH: func(context.Context) error { return errors.New("You are not logged into any GitHub hosts") }})
	c := wantCheck(t, cs, CheckWarn, "gh", "", "not logged")
	if !strings.Contains(c.Hint, "gh auth login") {
		t.Fatalf("hint %q", c.Hint)
	}
	if DoctorFailed(cs) {
		t.Fatalf("failed:\n%s", dump(cs))
	}
}

func TestDoctorBundleProblems(t *testing.T) {
	f := newFixture(t)
	cs := f.doctor(t, DoctorOptions{RootErr: errors.New("no sheaf.yaml found")})
	wantCheck(t, cs, CheckFail, "bundle", "", "no sheaf.yaml found")
	wantCheck(t, cs, CheckOK, "git", "", "")

	writeFile(t, filepath.Join(f.w.Root, bundle.FileName), "version: 99\n")
	cs = f.doctor(t, DoctorOptions{})
	wantCheck(t, cs, CheckFail, "bundle", "", bundle.FileName)
}

func TestDoctorRepositoryProblems(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	f.clonedRepo(t, "b")
	f.clonedRepo(t, "c")
	f.clonedRepo(t, "d")
	root := f.w.Root
	// a: directory missing.
	if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	// b: primary checkout renamed.
	if err := os.Rename(filepath.Join(root, "b", "main"), filepath.Join(root, "b", "trunk")); err != nil {
		t.Fatal(err)
	}
	// c: origin URL differs from sheaf.yaml, hosted elsewhere.
	cPrimary := filepath.Join(root, "c", "main")
	testutil.Git(t, cPrimary, "remote", "set-url", "origin", "file:///elsewhere/c.git")
	if err := f.w.update(func(b *bundle.Bundle) error {
		r, _ := b.Repository("c")
		r.Host = "gitlab.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// d: a prunable worktree, a worktree outside the bundle, stale and
	// fresh lock files.
	dPrimary := filepath.Join(root, "d", "main")
	f.mustAddWT(t, "d", "feat")
	if err := os.RemoveAll(filepath.Join(root, "d", "feat")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(resolved(t, t.TempDir()), "elsewhere")
	testutil.Git(t, dPrimary, "worktree", "add", "-q", "-b", "far", outside)
	stale := filepath.Join(dPrimary, ".git", "index.lock")
	writeFile(t, stale, "")
	then := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, then, then); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dPrimary, ".git", "refs", "heads", "main.lock")
	writeFile(t, fresh, "")

	cs := f.doctor(t, DoctorOptions{})
	wantCheck(t, cs, CheckFail, "repo-dir", "a", filepath.Join(root, "a"))
	wantCheck(t, cs, CheckFail, "primary", "b", filepath.Join(root, "b", "main"))
	wantCheck(t, cs, CheckWarn, "origin-url", "c", "file:///elsewhere/c.git")
	wantCheck(t, cs, CheckInfo, "host", "c", "gitlab.com")
	wantCheck(t, cs, CheckWarn, "worktree", "d", filepath.Join(root, "d", "feat"))
	wantCheck(t, cs, CheckWarn, "worktree", "d", outside)
	c := wantCheck(t, cs, CheckWarn, "lock", "d", stale)
	if c.Hint == "" {
		t.Error("no hint for the stale lock")
	}
	wantCheck(t, cs, CheckInfo, "lock", "d", fresh)
	if !DoctorFailed(cs) {
		t.Fatal("not failed")
	}
	// The primary of c, off its default branch, is fine (info only).
	testutil.Git(t, cPrimary, "switch", "-q", "feat")
	cs = f.doctor(t, DoctorOptions{})
	wantCheck(t, cs, CheckInfo, "primary", "c", "feat")
}

func TestDoctorUnregisteredDirectories(t *testing.T) {
	f := newFixture(t)
	f.clonedRepo(t, "a")
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	testutil.Git(t, f.w.Root, "clone", "-q", o.URL, "stray")
	if err := os.Mkdir(filepath.Join(f.w.Root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.w.Root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	cs := f.doctor(t, DoctorOptions{})
	c := wantCheck(t, cs, CheckWarn, "adoptable", "", "stray")
	if !strings.Contains(c.Hint, "sheaf adopt stray") {
		t.Fatalf("hint %q", c.Hint)
	}
	wantCheck(t, cs, CheckInfo, "unknown-dir", "", "notes")
	if _, ok := findCheck(cs, "unknown-dir", "", ".hidden"); ok {
		t.Fatalf("hidden directory reported\n%s", dump(cs))
	}
	if DoctorFailed(cs) {
		t.Fatalf("failed:\n%s", dump(cs))
	}
}

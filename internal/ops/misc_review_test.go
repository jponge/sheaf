package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

func TestExecuteRecoversPanics(t *testing.T) {
	s := Execute(context.Background(), 2, []Task{
		{Repo: "a", Run: func(context.Context, func(Event)) (string, error) { panic("kaboom") }},
		{Repo: "b", Run: func(context.Context, func(Event)) (string, error) { return "", nil }},
	}, Discard)
	if s.Results[0].Status != StatusFailed || !strings.Contains(s.Results[0].Err.Error(), "kaboom") || !strings.Contains(s.Results[0].Err.Error(), "goroutine") {
		t.Fatalf("a = %+v, want failure with panic value and stack", s.Results[0])
	}
	if s.Results[1].Status != StatusDone {
		t.Fatalf("b = %+v", s.Results[1])
	}
}

// S4: two Workspaces on the same bundle (two sheaf processes) must not lose
// each other's updates.
func TestTwoWorkspacesDoNotLoseUpdates(t *testing.T) {
	f := newFixture(t)
	w2, err := OpenWorkspace(f.w.Root)
	if err != nil {
		t.Fatal(err)
	}
	f.repo(t, "o", "one", "main", false)
	f.repo(t, "o", "two", "main", false)
	if _, err := Add(context.Background(), f.w, f.fake, AddRequest{Slugs: []string{"o/one"}}, 1, Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(context.Background(), w2, f.fake, AddRequest{Slugs: []string{"o/two"}}, 1, Discard); err != nil {
		t.Fatal(err)
	}
	if got := repoNames(f.onDisk(t)); len(got) != 2 {
		t.Fatalf("sheaf.yaml lists %v, want one and two", got)
	}

	// Concurrent updates through independent Workspaces.
	var ws []*Workspace
	for range 4 {
		w, err := OpenWorkspace(f.w.Root)
		if err != nil {
			t.Fatal(err)
		}
		ws = append(ws, w)
	}
	var wg sync.WaitGroup
	for i, w := range ws {
		for j := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := w.update(func(b *bundle.Bundle) error { return b.AddSource(fmt.Sprintf("org%d-%d", i, j)) }); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	wg.Wait()
	if n := len(f.onDisk(t).Sources); n != 40 {
		t.Fatalf("%d sources on disk, want 40", n)
	}
}

// NIT: the org is tracked only when the request did not entirely fail.
func TestAddOrgSourceRecordedUnlessEverythingFailed(t *testing.T) {
	t.Run("all clones failed", func(t *testing.T) {
		f := newFixture(t)
		f.fake.AddRepo(hosting.Repo{Owner: "o", Name: "broken", CloneURL: "file:///nonexistent/b.git", DefaultBranch: "main"})
		s, err := f.add(t, AddRequest{Org: "o", All: true})
		if err != nil || s.Count(StatusFailed) != 1 {
			t.Fatalf("%+v, %v", s, err)
		}
		if n := len(f.onDisk(t).Sources); n != 0 {
			t.Fatalf("%d sources recorded, want 0", n)
		}
	})
	t.Run("empty org with --all", func(t *testing.T) {
		f := newFixture(t)
		f.fake.AddOwner("o")
		if _, err := f.add(t, AddRequest{Org: "o", All: true}); err != nil {
			t.Fatal(err)
		}
		if n := len(f.onDisk(t).Sources); n != 1 {
			t.Fatalf("%d sources, want 1 (explicit --all on an empty org)", n)
		}
	})
	t.Run("everything already present", func(t *testing.T) {
		f := newFixture(t)
		f.repo(t, "o", "a", "main", false)
		if _, err := f.add(t, AddRequest{Slugs: []string{"o/a"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.add(t, AddRequest{Org: "o", All: true}); err != nil {
			t.Fatal(err)
		}
		if n := len(f.onDisk(t).Sources); n != 1 {
			t.Fatalf("%d sources, want 1", n)
		}
	})
}

// NIT: a truncated listing still lets named repositories be added.
func TestAddOrgTruncatedListing(t *testing.T) {
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	f.fake.ListErr["o"] = fmt.Errorf("listing o: %w", hosting.ErrPossiblyTruncated)
	s, err := f.add(t, AddRequest{Org: "o", Repos: []string{"a"}})
	if err != nil || s.Count(StatusDone) != 1 {
		t.Fatalf("add --repo with truncated listing: %+v, %v", s, err)
	}
	if _, err := f.add(t, AddRequest{Org: "o", All: true}); !errors.Is(err, hosting.ErrPossiblyTruncated) {
		t.Fatalf("add --all with truncated listing: err = %v, want ErrPossiblyTruncated", err)
	}
}

// NIT: init refuses inside a work tree or another bundle, and writes
// nothing when detection fails.
func TestInitRefusals(t *testing.T) {
	testutil.IsolateGit(t)
	o := testutil.NewOrigin(t, testutil.OriginOpts{})
	clone := filepath.Join(t.TempDir(), "c")
	plainClone(t, o, clone)
	sub := filepath.Join(clone, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{clone, sub} {
		if _, _, err := Init(context.Background(), d); err == nil || !strings.Contains(err.Error(), "work tree") {
			t.Errorf("Init(%s) err = %v, want refusal inside a work tree", d, err)
		}
		if exists(filepath.Join(d, bundle.FileName)) {
			t.Errorf("sheaf.yaml written in %s", d)
		}
	}

	outer, err := Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Init(context.Background(), inner); err == nil || !strings.Contains(err.Error(), outer) {
		t.Errorf("Init inside a bundle err = %v, want refusal naming %s", err, outer)
	}

	unreadable := filepath.Join(t.TempDir(), "u")
	if err := os.Mkdir(unreadable, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0o755) })
	if _, _, err := Init(context.Background(), unreadable); err == nil {
		t.Error("Init on an unreadable directory succeeded")
	}
	if _, err := os.Lstat(filepath.Join(unreadable, bundle.FileName)); err == nil {
		t.Error("sheaf.yaml written although detection failed")
	}
}

// Test gap: cancelling a clone that is really running removes the partial
// directory and leaves sheaf.yaml unchanged. upload-pack runs pack-objects
// through uploadpack.packObjectsHook (only honoured from protected
// configuration: here the test's isolated global config file, since git
// strips GIT_CONFIG_* from the environment of a local upload-pack), a
// script that signals it started and sleeps; cancellation kills git's whole
// process group.
func TestAddCancelKillsRunningClone(t *testing.T) {
	f := newFixture(t)
	f.repo(t, "o", "a", "main", false)
	scripts := t.TempDir()
	marker := filepath.Join(scripts, "started")
	slow := filepath.Join(scripts, "slow-pack-objects")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\ntouch '"+marker+"'\nsleep 30\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.OpenFile(os.Getenv("GIT_CONFIG_GLOBAL"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(cfg, "[uploadpack]\n\tpackObjectsHook = %s\n", slow); err != nil {
		t.Fatal(err)
	}
	cfg.Close()
	before, err := os.ReadFile(filepath.Join(f.w.Root, bundle.FileName))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	s, err := Add(ctx, f.w, f.fake, AddRequest{Slugs: []string{"o/a"}}, 1, Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Fatal("the slow upload-pack never ran; the test proves nothing")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Add returned after %v", d)
	}
	if len(s.Results) != 1 || !errors.Is(s.Results[0].Err, context.Canceled) {
		t.Fatalf("results = %+v, want cancellation", s.Results)
	}
	if exists(filepath.Join(f.w.Root, "a")) {
		t.Fatal("partial clone left behind")
	}
	after, _ := os.ReadFile(filepath.Join(f.w.Root, bundle.FileName))
	if string(after) != string(before) {
		t.Fatalf("sheaf.yaml changed:\n%s", after)
	}
}

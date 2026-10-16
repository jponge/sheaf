package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// B6 S4: repository names are matched exactly (case-sensitively) by every
// command, which all report a miss with the same message; a name that only
// differs by case gets a hint.
func TestRepositoryNamesAreExactEverywhere(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "alpha")
	f.clonedRepo(t, "beta")
	root := f.w.Root
	want := `no repository named "BETA" in the bundle (repositories: alpha, beta); did you mean "beta"?`
	ctx := context.Background()
	for what, err := range map[string]error{
		"ResolveRepo": func() error { _, err := ResolveRepo(f.w, "BETA", root); return err }(),
		"ListWorktrees": func() error {
			_, _, err := ListWorktrees(ctx, f.w, "BETA")
			return err
		}(),
		"Remove": func() error {
			_, err := Remove(ctx, f.w, "BETA", RemoveOptions{WorkingDir: root, Force: true, Confirm: yes}, noEmit)
			return err
		}(),
		"AddWorktree": func() error {
			_, err := AddWorktree(ctx, f.w, "BETA", "x", WorktreeAddOptions{NoFetch: true}, noEmit)
			return err
		}(),
		"RemoveWorktree": func() error {
			_, err := RemoveWorktree(ctx, f.w, "BETA", WorktreeTarget{Name: "x"}, WorktreeRemoveOptions{WorkingDir: root}, noEmit)
			return err
		}(),
		"AddTagWorktree": func() error {
			_, err := AddTagWorktree(ctx, f.w, "BETA", "v1", WorktreeAddOptions{NoFetch: true}, noEmit)
			return err
		}(),
		"CollectStatus --repo": func() error {
			_, err := CollectStatus(ctx, f.w, StatusOptions{Filter: CheckoutFilter{Repos: []string{"BETA"}}}, Discard)
			return err
		}(),
		"PlanPrune --repo": func() error {
			_, err := PlanPrune(ctx, f.w, PruneOptions{Repos: []string{"BETA"}, Offline: true, WorkingDir: root}, Discard)
			return err
		}(),
		"adopt --recheck": func() error {
			_, err := Adopt(ctx, f.w, filepath.Join(root, "BETA"), AdoptOptions{Recheck: true}, noEmit)
			return err
		}(),
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", what, err, want)
		}
	}
	if len(f.onDisk(t).Repositories) != 2 || !exists(filepath.Join(root, "beta", "main")) {
		t.Fatal("something was changed")
	}
	// No hint without a case-only match; a pattern keeps its own message.
	if _, err := ResolveRepo(f.w, "gamma", root); err == nil || strings.Contains(err.Error(), "did you mean") ||
		!strings.Contains(err.Error(), `no repository named "gamma" in the bundle (repositories: alpha, beta)`) {
		t.Errorf("gamma: err = %v", err)
	}
	if _, err := CollectStatus(ctx, f.w, StatusOptions{Filter: CheckoutFilter{Repos: []string{"BE*"}}}, Discard); err == nil ||
		!strings.Contains(err.Error(), `"BE*" matches no repository of the bundle`) {
		t.Errorf("pattern: err = %v", err)
	}
}

// adopt names a missing directory plainly (not with a raw lstat error).
func TestAdoptMissingDirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(f.w.Root, "nosuch")
	_, err := Adopt(context.Background(), f.w, dir, AdoptOptions{}, noEmit)
	if err == nil || err.Error() != "cannot adopt "+dir+": no such directory" {
		t.Fatalf("err = %v", err)
	}
}

// Review of B6: re-examining a name that is neither registered nor a
// directory does not suggest adopting it.
func TestRecheckOfUnknownMissingName(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.clonedRepo(t, "alpha")
	_, err := Adopt(context.Background(), f.w, filepath.Join(f.w.Root, "nope"), AdoptOptions{Recheck: true}, noEmit)
	want := `cannot re-examine nope: no repository named "nope" in the bundle (repositories: alpha), and no directory ` + filepath.Join(f.w.Root, "nope")
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

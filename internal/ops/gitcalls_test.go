package ops

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// gitCallsBundle builds the fixture of the git-call measurements: n
// repositories r1..rn, each with its primary checkout and two sibling
// worktrees feat-a and feat-b tracking origin/feat/a and origin/feat/b.
// feat/a is deleted on origin afterwards, so that once fetched it is gone
// (a prune candidate with a worktree).
func gitCallsBundle(t *testing.T, n int) *fixture {
	t.Helper()
	f := newFixture(t)
	var slugs []string
	origins := map[string]testutil.Origin{}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("r%d", i)
		o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: []string{"feat/a", "feat/b"}})
		f.fake.AddRepo(hosting.Repo{Owner: "o", Name: name, CloneURL: o.URL, DefaultBranch: "main"})
		slugs = append(slugs, "o/"+name)
		origins[name] = o
	}
	s, err := f.add(t, AddRequest{Slugs: slugs})
	if err != nil || s.Err() != nil {
		t.Fatalf("add: %v %v", err, s.Err())
	}
	for name, o := range origins {
		primary := filepath.Join(f.w.Root, name, "main")
		addWorktree(t, primary, "feat-a", "feat/a", true)
		addWorktree(t, primary, "feat-b", "feat/b", true)
		testutil.Git(t, o.Path, "branch", "-q", "-D", "feat/a")
	}
	return f
}

// gitCallKey condenses an invocation to its subcommand and leading options
// (e.g. "rev-parse --verify --quiet"), dropping operands.
func gitCallKey(args []string) string {
	var out []string
	i := 0
	for i+1 < len(args) && args[i] == "-c" { // -c name=value before the subcommand
		i += 2
	}
	for j, a := range args[i:] {
		if j > 0 && !strings.HasPrefix(a, "-") {
			break
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

// gitCallStats summarizes recorded invocations: the total, the number of
// repeated questions (same directory and arguments as an earlier call) and
// the most frequent keys.
func gitCallStats(calls []gitx.Call) string {
	seen := map[string]bool{}
	dups := 0
	byKey := map[string]int{}
	for _, c := range calls {
		id := c.Dir + "\x00" + strings.Join(c.Args, "\x00")
		if seen[id] {
			dups++
		}
		seen[id] = true
		byKey[gitCallKey(c.Args)]++
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if byKey[a] != byKey[b] {
			return byKey[b] - byKey[a]
		}
		return strings.Compare(a, b)
	})
	var top []string
	for _, k := range keys[:min(len(keys), 6)] {
		top = append(top, fmt.Sprintf("%s ×%d", k, byKey[k]))
	}
	return fmt.Sprintf("total %d, repeated %d; %s", len(calls), dups, strings.Join(top, ", "))
}

// gitCallCommands are the measured commands, run in this order on one
// fixture bundle (the mutating ones last), with their git-call budgets.
//
// A budget is the number of git processes the command started on the
// fixture when it was last set, plus a margin of 2. The counts are
// deterministic (the cache is invalidated per repository, and each
// repository is handled by one task at a time), so the margin only absorbs
// a call or two that a future change may legitimately add; one repeated
// call per repository (5) or per checkout (15) exceeds it.
//
// Updating a budget: run
//
//	go test ./internal/ops -run TestGitCallsPerCommand -v
//
// which logs, per command, the count, the repeated questions and the most
// frequent invocations. When a change adds calls on purpose (or removes
// some), set the budget to the new count + 2 and say why in the commit
// message; when it adds calls unexpectedly, look at the "repeated" figure
// and the invocation keys first.
var gitCallCommands = []struct {
	name   string
	budget int
	run    func(ctx context.Context, t *testing.T, f *fixture) error
}{
	{"status", 35 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, err := CollectStatus(ctx, f.w, StatusOptions{Jobs: 4}, Discard)
		return err
	}},
	// The home overview: status plus the date of each HEAD, one rev-list
	// per repository.
	{"overview", 40 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		r, err := CollectStatus(ctx, f.w, StatusOptions{Jobs: 4, HeadTimes: true}, Discard)
		for _, c := range r.Checkouts {
			if err == nil && c.HeadTime.IsZero() {
				err = fmt.Errorf("%s: no head time", c.Name())
			}
		}
		return err
	}},
	{"wt list", 20 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, _, err := ListWorktrees(ctx, f.w, "")
		return err
	}},
	{"path", 4 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, err := ResolvePath(ctx, f.w, PathQuery{Args: []string{"r3", "feat/b"}, Cwd: f.w.Root})
		return err
	}},
	// The suggestions of the home screen's w prompt.
	{"wt add suggest", 1 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		s, err := SuggestWorktree(ctx, f.w, "r1")
		if err == nil && len(s.Branches) == 0 {
			err = errors.New("no branch suggested")
		}
		return err
	}},
	{"doctor", 31 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		cs := Doctor(ctx, DoctorOptions{Root: f.w.Root, GH: goodGH})
		if DoctorFailed(cs) {
			return errors.New(dump(cs))
		}
		return nil
	}},
	{"fetch", 10 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		s, err := FetchRepos(ctx, f.w, FetchOptions{Jobs: 4}, Discard)
		if err == nil {
			err = s.Err()
		}
		return err
	}},
	{"update", 45 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, err := Update(ctx, f.w, UpdateOptions{Jobs: 4}, Discard)
		return err
	}},
	{"prune (dry-run)", 95 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		p, err := PlanPrune(ctx, f.w, PruneOptions{Jobs: 4, WorkingDir: f.w.Root}, Discard)
		if err == nil && len(p.Candidates) != 5 {
			err = fmt.Errorf("prune candidates = %v, want feat/a of each repository", candidateNames(p))
		}
		return err
	}},
	{"remove plan", 28 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, err := Remove(ctx, f.w, "r2", RemoveOptions{WorkingDir: f.w.Root, Force: true}, noEmit)
		if errors.Is(err, ErrNotConfirmed) {
			return nil
		}
		return fmt.Errorf("remove without confirmation: %v", err)
	}},
	{"wt add", 19 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, err := AddWorktree(ctx, f.w, "r1", "feat/new", WorktreeAddOptions{}, noEmit)
		return err
	}},
	{"wt rm", 27 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, err := RemoveWorktree(ctx, f.w, "r1", WorktreeTarget{Name: "feat/b"}, WorktreeRemoveOptions{WorkingDir: f.w.Root, Confirm: func(WorktreeRemovePlan) bool { return true }}, noEmit)
		return err
	}},
	// The home screen's r on two worktrees of a repository (47 calls
	// when they were removed one after the other).
	{"wt rm (2)", 46 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		items := []WorktreeRemoval{{Repo: "r2", Target: WorktreeTarget{Name: "feat-a", Kind: TargetDir}}, {Repo: "r2", Target: WorktreeTarget{Name: "feat-b", Kind: TargetDir}}}
		for _, r := range RemoveWorktrees(ctx, f.w, items, WorktreeRemoveOptions{WorkingDir: f.w.Root}, func([]WorktreeRemovalResult) bool { return true }, Discard) {
			if r.Err != nil {
				return r.Err
			}
		}
		return nil
	}},
	{"remove", 59 + 2, func(ctx context.Context, t *testing.T, f *fixture) error {
		_, err := Remove(ctx, f.w, "r5", RemoveOptions{WorkingDir: f.w.Root, Force: true, Confirm: yes}, noEmit)
		return err
	}},
}

// TestGitCallsPerCommand checks the git-call budgets of representative
// commands on a bundle of 5 repositories with 3 checkouts each, and logs
// what each command ran (see gitCallCommands for updating the budgets).
func TestGitCallsPerCommand(t *testing.T) {
	t.Parallel()
	f := gitCallsBundle(t, 5)
	for _, c := range gitCallCommands {
		ctx, counter := gitx.WithCallCounter(context.Background())
		if err := c.run(ctx, t, f); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		calls := counter.Calls()
		t.Logf("%-16s budget %3d, %s", c.name, c.budget, gitCallStats(calls))
		if len(calls) > c.budget {
			t.Errorf("%s started %d git processes, budget %d: %s", c.name, len(calls), c.budget, gitCallStats(calls))
		}
	}
}

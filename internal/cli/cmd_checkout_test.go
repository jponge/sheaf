package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/testutil"
)

// newBundleEnv creates a bundle and makes it the working directory.
func newBundleEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	root := filepath.Join(t.TempDir(), "bundle")
	e.mustRun(t, "create", root)
	root, _ = filepath.EvalSymlinks(root)
	e.cwd = root
	return e
}

// addRepo adds o/<name> with the given extra branches; it returns the
// origin and the primary checkout.
func (e *env) addRepo(t *testing.T, name string, branches ...string) (testutil.Origin, string) {
	t.Helper()
	o := testutil.NewOrigin(t, testutil.OriginOpts{Branches: branches})
	e.fake.AddRepo(hosting.Repo{Owner: "o", Name: name, CloneURL: o.URL, DefaultBranch: "main"})
	e.mustRun(t, "add", "o/"+name)
	return o, filepath.Join(e.cwd, name, "main")
}

// addWorktree adds the sibling worktree <dir> tracking origin/<branch>.
func addWorktree(t *testing.T, primary, dir, branch string) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(primary), dir)
	testutil.Git(t, primary, "worktree", "add", "-q", "--track", "-b", branch, p, "origin/"+branch)
	return p
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGitInteractiveDecision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tui      bool
		jobs     int
		stdinTTY bool
		want     bool
	}{
		{false, 1, true, true},
		{true, 1, true, false},
		{false, 2, true, false},
		{false, 1, false, false},
		{true, 4, false, false},
	} {
		if got := gitInteractive(tc.tui, tc.jobs, tc.stdinTTY); got != tc.want {
			t.Errorf("gitInteractive(tui=%v, jobs=%d, stdinTTY=%v) = %v, want %v", tc.tui, tc.jobs, tc.stdinTTY, got, tc.want)
		}
	}
}

var ansiEscape = regexp.MustCompile("\x1b\\[")

func TestStatusTable(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha", "feat")
	e.addRepo(t, "beta")
	wt := addWorktree(t, alpha, "feat", "feat")
	writeFile(t, filepath.Join(alpha, "stash.txt"), "s\n")
	testutil.Git(t, alpha, "stash", "push", "-q", "-u")
	writeFile(t, filepath.Join(wt, "file-1.txt"), "changed\n")
	testutil.Git(t, wt, "add", "file-1.txt")
	writeFile(t, filepath.Join(wt, "u.txt"), "u\n")

	out := e.mustRun(t, "status")
	if ansiEscape.MatchString(out) {
		t.Errorf("color escapes although stdout is not a terminal:\n%q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 3 rows:\n%s", out)
	}
	for _, h := range []string{"REPO", "DIR", "BRANCH", "UPSTREAM", "AHEAD", "BEHIND", "STAGED", "UNSTAGED", "UNTRACKED", "CONFLICTS", "STASHES", "STATE"} {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header lacks %s: %q", h, lines[0])
		}
	}
	f := func(i int) []string { return strings.Fields(lines[i]) }
	// alpha/main: clean, stash shown once for the repository.
	if got := strings.Join(f(1), " "); got != "alpha main main origin/main 0 0 0 0 0 0 1 clean" {
		t.Errorf("row 1 = %q", got)
	}
	if got := strings.Join(f(2), " "); got != "alpha feat feat origin/feat 0 0 1 0 1 0 dirty" {
		t.Errorf("row 2 = %q (stashes must only appear on the repository's first row)", got)
	}
	if got := strings.Join(f(3), " "); got != "beta main main origin/main 0 0 0 0 0 0 0 clean" {
		t.Errorf("row 3 = %q", got)
	}
	// Columns are aligned: every row has its DIR column at the same offset.
	col := strings.Index(lines[0], "DIR")
	for _, l := range lines[1:] {
		if len(l) <= col || l[col-1] != ' ' || l[col] == ' ' {
			t.Errorf("misaligned row %q (DIR at %d)", l, col)
		}
	}

	out = e.mustRun(t, "status", "--dirty-only")
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 1 || !strings.Contains(out, "feat") {
		t.Errorf("--dirty-only:\n%s", out)
	}
	out = e.mustRun(t, "status", "--repo", "b*")
	if strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("--repo b*:\n%s", out)
	}
}

// status colors its states like the level words of the other commands
// (see leveled): in TUI mode on a terminal whose color profile allows it;
// not with NO_COLOR, CLICOLOR=0, --no-tui (plain mode), when not on a
// terminal, nor with --json.
func TestStatusColor(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha", "feat")
	wt := addWorktree(t, alpha, "feat", "feat")
	writeFile(t, filepath.Join(wt, "u.txt"), "u\n")
	d := e.deps()
	plain, _, _ := runDeps(t, d, "status")
	out, _, code := runColored(t, d, true, colorTTY, "status")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{sgrGreen + "clean" + sgrReset, sgrYellow + "dirty" + sgrReset} {
		if !strings.Contains(out, want) {
			t.Errorf("colored status lacks %q:\n%q", want, out)
		}
	}
	if got := ansi.Strip(out); got != plain {
		t.Errorf("colored status without its colors differs from the plain output:\n%q\nwant\n%q", got, plain)
	}
	for _, tc := range []struct {
		name    string
		tuiMode bool
		environ []string
		args    []string
	}{
		{"NO_COLOR", true, append(colorTTY, "NO_COLOR=1"), []string{"status"}},
		{"CLICOLOR=0", true, append(colorTTY, "CLICOLOR=0"), []string{"status"}},
		{"not a terminal", true, []string{"TERM=xterm-256color"}, []string{"status"}},
		{"plain mode", false, colorTTY, []string{"status"}},
		{"json", true, colorTTY, []string{"status", "--json"}},
	} {
		got, _, _ := runColored(t, d, tc.tuiMode, tc.environ, tc.args...)
		if strings.Contains(got, "\x1b") {
			t.Errorf("%s: escape sequences in\n%q", tc.name, got)
		}
		if tc.name != "json" && got != plain {
			t.Errorf("%s: output changed:\n%q\nwant\n%q", tc.name, got, plain)
		}
	}
}

func TestStatusDetachedAndNoUpstream(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha")
	testutil.Git(t, alpha, "worktree", "add", "-q", "--detach", filepath.Join(filepath.Dir(alpha), "det"), "HEAD")
	testutil.Git(t, alpha, "worktree", "add", "-q", "-b", "local", filepath.Join(filepath.Dir(alpha), "local"))
	out := e.mustRun(t, "status")
	if !regexp.MustCompile(`alpha\s+det\s+\(detached [0-9a-f]{7,}\)\s+-\s+-\s+-`).MatchString(out) {
		t.Errorf("detached row:\n%s", out)
	}
	if !regexp.MustCompile(`alpha\s+local\s+local\s+-\s+-\s+-`).MatchString(out) {
		t.Errorf("no-upstream row:\n%s", out)
	}
}

type statusJSON struct {
	Version      int    `json:"version"`
	Bundle       string `json:"bundle"`
	Repositories []struct {
		Name      string `json:"name"`
		Stashes   int    `json:"stashes"`
		Checkouts []struct {
			Dir          string `json:"dir"`
			Path         string `json:"path"`
			Primary      bool   `json:"primary"`
			Branch       string `json:"branch"`
			Detached     bool   `json:"detached"`
			Head         string `json:"head"`
			Upstream     string `json:"upstream"`
			UpstreamGone bool   `json:"upstream_gone"`
			Ahead        *int   `json:"ahead"`
			Behind       *int   `json:"behind"`
			Staged       int    `json:"staged"`
			Unstaged     int    `json:"unstaged"`
			Untracked    int    `json:"untracked"`
			Conflicted   int    `json:"conflicted"`
			Clean        bool   `json:"clean"`
			Error        string `json:"error"`
		} `json:"checkouts"`
	} `json:"repositories"`
	Problems []struct {
		Repo  string `json:"repo"`
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"problems"`
}

func TestStatusJSON(t *testing.T) {
	e := newBundleEnv(t)
	o, alpha := e.addRepo(t, "alpha", "feat")
	wt := addWorktree(t, alpha, "feat", "feat")
	writeFile(t, filepath.Join(wt, "u.txt"), "u\n")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	e.addRepo(t, "beta")
	if err := os.RemoveAll(filepath.Join(e.cwd, "beta")); err != nil {
		t.Fatal(err)
	}

	out, errs, code := e.run(t, "status", "--json", "--fetch")
	if code == 0 {
		t.Errorf("exit 0 despite a problem; stderr %s", errs)
	}
	var s statusJSON
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if s.Version != 1 || s.Bundle != e.cwd || len(s.Repositories) != 1 || len(s.Problems) != 1 {
		t.Fatalf("document = %+v", s)
	}
	r := s.Repositories[0]
	if r.Name != "alpha" || len(r.Checkouts) != 2 {
		t.Fatalf("repository = %+v", r)
	}
	m, f := r.Checkouts[0], r.Checkouts[1]
	if !m.Primary || m.Dir != "main" || m.Path != alpha || m.Branch != "main" || m.Upstream != "origin/main" ||
		m.Behind == nil || *m.Behind != 1 || m.Ahead == nil || *m.Ahead != 0 || !m.Clean || len(m.Head) < 40 {
		t.Errorf("primary = %+v (--fetch must fetch first)", m)
	}
	if f.Primary || f.Dir != "feat" || f.Untracked != 1 || f.Clean {
		t.Errorf("worktree = %+v", f)
	}
	if p := s.Problems[0]; p.Repo != "beta" || p.Error == "" {
		t.Errorf("problem = %+v", p)
	}
}

func TestStatusProblemsListedAndNonZero(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha", "feat")
	wt := addWorktree(t, alpha, "feat", "feat")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	out, errs, code := e.run(t, "status")
	if code != 1 || !strings.Contains(out, "alpha") || !strings.Contains(out, wt) || !strings.Contains(out, "missing") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
}

func TestFetchCommand(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	o, alpha := e.addRepo(t, "alpha")
	e.addRepo(t, "beta")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	out, errs, code := e.run(t, "fetch")
	if code != 0 || !strings.Contains(out, "2 done, 0 skipped, 0 failed") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if got, want := testutil.Git(t, alpha, "rev-parse", "origin/main"), testutil.Git(t, o.Path, "rev-parse", "main"); got != want {
		t.Fatal("origin/main not fetched")
	}
}

func TestUpdateCommand(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	o, alpha := e.addRepo(t, "alpha", "dirty")
	dirty := addWorktree(t, alpha, "dirty", "dirty")
	testutil.AdvanceOrigin(t, o, "main", "r.txt", "r\n", "remote")
	testutil.AdvanceOrigin(t, o, "dirty", "r.txt", "r\n", "remote")
	writeFile(t, filepath.Join(dirty, "file-1.txt"), "staged\n")
	testutil.Git(t, dirty, "add", "file-1.txt")

	out, errs, code := e.run(t, "update")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if !regexp.MustCompile(`fast-forwarded\s+alpha/main\s+[0-9a-f]+\.\.[0-9a-f]+`).MatchString(out) {
		t.Errorf("no fast-forward line:\n%s", out)
	}
	if !regexp.MustCompile(`skipped\s+alpha/dirty\s+uncommitted changes \(1 staged, 0 unstaged\)`).MatchString(out) {
		t.Errorf("no dirty notice with counts:\n%s", out)
	}
	if !strings.Contains(out, "1 fast-forwarded, 0 up to date, 1 skipped, 0 failed") {
		t.Errorf("no summary line:\n%s", out)
	}
	if !strings.Contains(errs, "alpha/dirty: update skipped") {
		t.Errorf("no progress event on stderr:\n%s", errs)
	}

	out, _, code = e.run(t, "update", "--strict")
	if code != 1 || !strings.Contains(out, "1 skipped") {
		t.Errorf("--strict with a skip: exit %d\n%s", code, out)
	}
	out, _, code = e.run(t, "update", "--primary-only", "--strict")
	if code != 0 || strings.Contains(out, "alpha/dirty") || !regexp.MustCompile(`up to date\s+alpha/main`).MatchString(out) {
		t.Errorf("--primary-only: exit %d\n%s", code, out)
	}
}

func TestUpdateCommandFailureExitsNonZero(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha")
	testutil.Git(t, alpha, "remote", "set-url", "origin", "file://"+filepath.Join(t.TempDir(), "missing.git"))
	out, _, code := e.run(t, "update")
	if code != 1 || !regexp.MustCompile(`failed\s+alpha/main\s+fetch failed`).MatchString(out) || !strings.Contains(out, "1 failed") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestRunCommandBlocksAndSummary(t *testing.T) {
	t.Parallel()
	e := newBundleEnv(t)
	_, alpha := e.addRepo(t, "alpha", "feat")
	e.addRepo(t, "beta")
	addWorktree(t, alpha, "feat", "feat")

	// No "--" needed: flags after the command belong to it.
	out := e.mustRun(t, "run", "-j", "4", "git", "rev-parse", "--abbrev-ref", "HEAD")
	want := "==> alpha/main <==\nmain\n\n==> alpha/feat <==\nfeat\n\n==> beta/main <==\nmain\n\n3 succeeded, 0 failed\n"
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
	// -j 1 streams with the same layout.
	if out1 := e.mustRun(t, "run", "-j", "1", "--", "git", "rev-parse", "--abbrev-ref", "HEAD"); out1 != want {
		t.Errorf("-j 1 output:\n%s\nwant:\n%s", out1, want)
	}

	out, errs, code := e.run(t, "run", "--shell", "--", `printf 'no newline'; test "$SHEAF_DIR" != feat || exit 7`)
	if code != 1 {
		t.Errorf("exit %d, want 1; stderr %s", code, errs)
	}
	if !strings.Contains(out, "==> alpha/feat <==\nno newline\n[exit status 7]\n") {
		t.Errorf("failed block:\n%s", out)
	}
	if !strings.Contains(out, "2 succeeded, 1 failed\nfailed: alpha/feat (exit status 7)\n") {
		t.Errorf("summary:\n%s", out)
	}

	if _, errs, code := e.run(t, "run"); code == 0 || !strings.Contains(errs, "command") {
		t.Errorf("run without command: exit %d, stderr %s", code, errs)
	}
}

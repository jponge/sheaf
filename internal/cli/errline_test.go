package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
	"github.com/jponge/sheaf/internal/ops"
)

// The places that show an error on one line (overview message, progress
// rows, summary lines) show the Summary of a multi-line refusal, else its
// first line without a trailing colon (B52 A5).

func risksError() error {
	return &ops.RisksError{Plan: ops.RemovePlan{Repo: "old", Items: []ops.RemoveItem{
		{Path: "/b/old/main", Risks: []gitx.Risk{{Kind: gitx.RiskUntracked, Count: 1}}}}}}
}

func TestErrorLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		line string
		more bool
	}{
		{"one line", errors.New("boom: it failed"), "boom: it failed", false},
		{"one line ending in a colon", errors.New("boom:"), "boom:", false},
		{"several lines", errors.New("2 problem(s):\n  a\n  b"), "2 problem(s)", true},
		{"refusal", risksError(), "refusing to remove old: 1 untracked file", true},
		{"wrapped refusal", fmt.Errorf("sync: %w", risksError()), "refusing to remove old: 1 untracked file", true},
		// The error of a summary wraps those of its rows (a CleanupError is
		// found among them) but keeps its count, the rows being in the
		// report (B62 H4 re-review).
		{"summary with a refused row", ops.Summary{Results: []ops.Result{
			{Repo: "new", Status: ops.StatusDone}, {Repo: "old", Status: ops.StatusFailed, Err: risksError()}}}.Err(),
			"1 of 2 repositories failed", true},
	} {
		line, more := errorLine(tc.err)
		if line != tc.line || more != tc.more {
			t.Errorf("%s: errorLine = %q, %v; want %q, %v", tc.name, line, more, tc.line, tc.more)
		}
	}
}

func TestPrintSummaryShowsRefusalSummary(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	_ = printSummary(&b, ops.Summary{Results: []ops.Result{{Repo: "old", Status: ops.StatusFailed, Err: risksError()}}})
	if want := "failed   old: refusing to remove old: 1 untracked file\n"; !strings.HasPrefix(b.String(), want) {
		t.Fatalf("printSummary =\n%s\nwant it to start with\n%s", b.String(), want)
	}
}

// The overview message after a refused wt rm says what is at risk and
// points to the report, which has the details.
func TestHomeRefusedRemovalMessageSummarizes(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := homeBundle(t)
	var out syncBuffer
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "j", "r"), keys(), reportKey(t, &out, "wt rm", "q"), ovKeys(t, &out, "wt rm failed", 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if want := "wt rm failed: refusing to remove alpha/feat: 1 untracked file (see report)"; code != 0 || !strings.Contains(out.String(), want) {
		t.Fatalf("exit %d, want the message %q\nstdout:\n%s\nstderr:\n%s", code, want, out.String(), errs)
	}
}

// ghStub writes a gh stub printing stderr for "<cmd> <subcmd>" (e.g.
// "repo view") and exiting with code; other commands fail. It returns the
// dependencies of e with the gh adapter running it.
func ghStub(t *testing.T, e *env, code int, stderr map[string]string) Deps {
	t.Helper()
	dir := t.TempDir()
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\ncase \"$1 $2\" in\n")
	i := 0
	for cmd, msg := range stderr {
		i++
		f := filepath.Join(dir, fmt.Sprintf("stderr%d", i))
		if err := os.WriteFile(f, []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sb, "'%s') cat '%s' >&2; exit %d ;;\n", cmd, f, code)
	}
	sb.WriteString("*) echo \"unexpected: $*\" >&2; exit 99 ;;\nesac\n")
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(sb.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	d := e.deps()
	d.Hosting = func() hosting.Hosting { return hosting.NewGH(hosting.GHOptions{Path: path, Protocol: "https"}) }
	return d
}

// gh failures read as one actionable line, through the gh adapter (B62
// H2): no gh command line, exit status or multi-line gh text. sync says a
// failed listing once in full (its warning, before the proposals), its
// final error only names it.
func TestGHErrorsOneLine(t *testing.T) {
	t.Parallel()
	e, _ := syncBundle(t)
	// What gh prints without credentials (recorded, see the hosting tests).
	const unauth = "To get started with GitHub CLI, please run:  gh auth login\n" +
		"Alternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token."
	noAuth := ghStub(t, e, 4, map[string]string{"repo view": unauth, "pr view": unauth, "repo list": unauth})
	notFound := ghStub(t, e, 1, map[string]string{
		"repo view": "GraphQL: Could not resolve to a Repository with the name 'o/nope'. (repository)",
		"pr view":   "GraphQL: Could not resolve to a PullRequest with the number of 7. (repository.pullRequest)",
		"repo list": `the owner handle "nobody" was not recognized as either a GitHub user or an organization`,
	})
	const authLine = "gh is not authenticated: run `gh auth login`"
	for _, tc := range []struct {
		name     string
		d        Deps
		tui      bool
		args     []string
		out, err string // a line of standard output, the last line of standard error
	}{
		{"wt pr, no credentials", noAuth, false, []string{"wt", "pr", "a", "7"}, "", "sheaf: pull request #7 of o/a: " + authLine},
		{"add, no credentials", noAuth, false, []string{"add", "o/nope"}, "failed   o/nope: " + authLine, "sheaf: 1 of 1 repositories failed"},
		{"add --org, no credentials", noAuth, false, []string{"add", "--org", "o", "--all"}, "", "sheaf: listing repositories of o: " + authLine},
		{"sync, no credentials", noAuth, false, []string{"sync"}, "", "sheaf: listing repositories of o failed"},
		{"sync TUI, no credentials", noAuth, true, []string{"sync"}, "", "sheaf: listing repositories of o failed"},
		{"sync --json, no credentials", noAuth, false, []string{"sync", "--json"}, `"error": "listing repositories of o: ` + authLine + `"`, "sheaf: listing repositories of o failed"},
		{"wt pr, no such pull request", notFound, false, []string{"wt", "pr", "a", "7"}, "", "sheaf: pull request #7 of o/a: no such pull request on GitHub"},
		{"add, no such repository", notFound, false, []string{"add", "o/nope"}, "failed   o/nope: no such repository on GitHub (or not visible to your gh account)", "sheaf: 1 of 1 repositories failed"},
		{"add --org, no such owner", notFound, false, []string{"add", "--org", "nobody", "--all"}, "", "sheaf: listing repositories of nobody: no such user or organization on GitHub"},
		{"browse, no credentials", noAuth, false, []string{"browse", "a"}, "", "sheaf: opening o/a: " + authLine},
		{"browse, no such repository", notFound, false, []string{"browse", "a"}, "", "sheaf: opening o/a: no such repository on GitHub (or not visible to your gh account)"},
	} {
		d := tc.d
		d.UseTUI = func(io.Reader, io.Writer, bool) bool { return tc.tui }
		out, errs, code := runDeps(t, d, tc.args...)
		lines := strings.Split(strings.TrimSuffix(errs, "\n"), "\n")
		if code != 1 || lines[len(lines)-1] != tc.err || tc.out != "" && !strings.Contains(out, tc.out+"\n") {
			t.Errorf("%s: exit %d, want the output line %q and the last error line %q\nstdout:\n%s\nstderr:\n%s", tc.name, code, tc.out, tc.err, out, errs)
		}
		for _, bad := range []string{"exit status", "gh repo ", "gh pr ", "To get started", "GraphQL"} {
			if strings.Contains(out+errs, bad) {
				t.Errorf("%s: %q in the output\nstdout:\n%s\nstderr:\n%s", tc.name, bad, out, errs)
			}
		}
		if strings.HasPrefix(tc.name, "sync") {
			if n := strings.Count(errs, authLine); n != 1 || !strings.Contains(errs, "sheaf: warning: listing repositories of o: "+authLine+"\n") {
				t.Errorf("%s: the listing failure is not said once in full, as a warning (%d times):\n%s", tc.name, n, errs)
			}
		}
	}
}

// twoOrgSync is syncBundle also tracking the organization p, whose listing
// then fails as gh without credentials does: sync proposes b and c of o.
func twoOrgSync(t *testing.T) *env {
	t.Helper()
	e, _ := syncBundle(t)
	e.origin(t, "p", "x", false)
	e.mustRun(t, "add", "--org", "p", "--repo", "x")
	e.fake.ListErr["p"] = &hosting.GHError{Args: []string{"repo", "list", "p"}, Err: hosting.ErrGHNotAuthenticated}
	return e
}

// The menu of a TUI sync, one listing failed, says that failure once in
// full (its warning) whether nothing is selected or the selection is
// applied; the final error only names it.
func TestSyncTUIListingFailureSaidOnce(t *testing.T) {
	t.Parallel()
	const reason = "listing repositories of p: gh is not authenticated: run `gh auth login`"
	for name, in := range map[string]func(*syncBuffer) io.Reader{
		"nothing selected": func(*syncBuffer) io.Reader { return newScript(t, false, keys("\r")) },
		"applied": func(out *syncBuffer) io.Reader {
			return newScript(t, true, keys(" ", "\r"), []key{{wait: waitOutput(t, out, "Syncing: finished")}})
		},
	} {
		e := twoOrgSync(t)
		var out syncBuffer
		errs, code := e.runTUI(t, in(&out), &out, "sync")
		lines := strings.Split(strings.TrimSuffix(errs, "\n"), "\n")
		if code != 1 || strings.Count(errs, "gh is not authenticated") != 1 || !strings.Contains(errs, "sheaf: warning: "+reason+"\n") ||
			lines[len(lines)-1] != "sheaf: listing repositories of p failed" {
			t.Errorf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", name, code, out.String(), errs)
		}
	}
}

// On the home screen, the message of a sync whose listing failed gives
// the reason (one line), not only the name of the listing.
func TestHomeSyncListingFailureMessageGivesTheReason(t *testing.T) {
	t.Parallel()
	e := twoOrgSync(t)
	var out syncBuffer
	want := "sync failed: listing repositories of p: gh is not authenticated: run `gh auth login` (see report)"
	in := newScript(t, false, ovKeys(t, &out, "STATE", 1, "s"), keys("\r"), reportKey(t, &out, "sync", "q"), ovKeys(t, &out, want, 1, "q"))
	errs, code := runHomeWith(t, e.deps(), in, &out)
	if code != 0 || !strings.Contains(out.String(), want) {
		t.Fatalf("exit %d, want the message %q\nstdout:\n%s\nstderr:\n%s", code, want, out.String(), errs)
	}
}

// The summary of a failed listing is the first line of its reason (gh's
// other failures keep its text, which may have several lines).
func TestListingErrorSummaryIsOneLine(t *testing.T) {
	t.Parallel()
	err := &listingError{Org: "p", Err: errors.New("listing repositories of p: gh repo list p: exit status 1: HTTP 502\nmore")}
	if got, more := errorLine(fmt.Errorf("x: %w", errors.Join(err))); got != "listing repositories of p: gh repo list p: exit status 1: HTTP 502" || !more {
		t.Fatalf("errorLine = %q, %v", got, more)
	}
}

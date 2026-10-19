package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// tagBundle: repositories a (origin tag release/1.0 created after the clone,
// local tags v1 and v2) and b; returns the env (cwd = bundle root) and the
// resolved root.
func tagBundle(t *testing.T) (*env, string, testutil.Origin) {
	t.Helper()
	e := newBundleEnv(t)
	root := e.cwd
	o := e.origin(t, "o", "a", false)
	e.origin(t, "o", "b", false)
	e.mustRun(t, "add", "o/a", "o/b")
	testutil.TagOrigin(t, o, "release/1.0", "main", true)
	primary := filepath.Join(root, "a", "main")
	testutil.Git(t, primary, "tag", "v1")
	testutil.Git(t, primary, "tag", "v2")
	return e, root, o
}

func TestWorktreeAddTagCLI(t *testing.T) {
	t.Parallel()
	e, root, _ := tagBundle(t)
	tw := filepath.Join(root, "a", "release-1.0")
	out, errs, code := e.run(t, "wt", "add", "a", "--tag", "release/1.0")
	if code != 0 || out != tw+"\n" || !strings.Contains(errs, "fetching origin and its tags") {
		t.Fatalf("wt add --tag: exit %d, stdout %q, stderr:\n%s", code, out, errs)
	}
	// Repository inferred from the working directory.
	e.cwd = filepath.Join(root, "a", "main")
	if out := e.mustRun(t, "wt", "add", "--tag", "v1", "--no-fetch"); out != filepath.Join(root, "a", "v1")+"\n" {
		t.Fatalf("inferred repo: stdout %q", out)
	}
	e.cwd = root
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"wt", "add", "a", "--tag", "v2", "--base", "origin/main", "--no-fetch"}, "--base does not apply"},
		{[]string{"wt", "add", "a", "b", "--tag", "v2", "--no-fetch"}, "with --tag, expected at most 1 argument"},
		{[]string{"wt", "add", "a", "v2", "--no-fetch"}, "v2 is a tag; use --tag v2 for a detached worktree at that tag"},
		{[]string{"wt", "add", "a", "--tag", "a..b", "--no-fetch"}, "not a valid tag name"},
	} {
		if _, errs, code := e.run(t, tc.args...); code == 0 || !strings.Contains(errs, tc.want) {
			t.Errorf("sheaf %s: exit %d, stderr:\n%s\nwant %q", strings.Join(tc.args, " "), code, errs, tc.want)
		}
	}

	// list: text and JSON.
	list := e.mustRun(t, "wt", "list", "a")
	if !strings.Contains(list, "(tag release/1.0)") || !strings.Contains(list, "detached,tag") {
		t.Errorf("wt list:\n%s", list)
	}
	var doc struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	if err := json.Unmarshal([]byte(e.mustRun(t, "wt", "list", "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	tags := map[string]any{}
	for _, w := range doc.Worktrees {
		tags[w["path"].(string)] = w["tag"]
	}
	if tags[tw] != "release/1.0" || tags[filepath.Join(root, "a", "main")] != "" || tags[filepath.Join(root, "b", "main")] != "" {
		t.Errorf("wt list --json tags: %v", tags)
	}

	// status: text and JSON.
	if st := e.mustRun(t, "status"); !strings.Contains(st, "(tag release/1.0)") {
		t.Errorf("status:\n%s", st)
	}
	var sdoc struct {
		Repositories []struct {
			Name      string           `json:"name"`
			Checkouts []map[string]any `json:"checkouts"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(e.mustRun(t, "status", "--json")), &sdoc); err != nil {
		t.Fatal(err)
	}
	stags := map[string]any{}
	for _, r := range sdoc.Repositories {
		for _, c := range r.Checkouts {
			stags[r.Name+"/"+c["dir"].(string)] = c["tag"]
		}
	}
	if stags["a/release-1.0"] != "release/1.0" || stags["a/main"] != "" {
		t.Errorf("status --json tags: %v", stags)
	}

	// update: pinned, not a skip for --strict.
	up := e.mustRun(t, "update", "--strict")
	if !strings.Contains(up, "pinned at tag release/1.0") || !strings.Contains(up, "2 pinned") || !strings.Contains(up, "0 skipped") {
		t.Errorf("update --strict:\n%s", up)
	}

	// prune lists it as skipped.
	if pr := e.mustRun(t, "prune"); !strings.Contains(pr, "skipped a (tag release/1.0): pinned at tag release/1.0") {
		t.Errorf("prune:\n%s", pr)
	}

	// path by tag and by directory.
	for _, args := range [][]string{{"path", "a", "release/1.0"}, {"path", "a", "release-1.0"}} {
		if p := e.mustRun(t, args...); p != tw+"\n" {
			t.Errorf("sheaf %s = %q", strings.Join(args, " "), p)
		}
	}

	// rm: -d/-D refused, plain removal works by tag name.
	for _, flag := range []string{"-d", "-D"} {
		if _, errs, code := e.run(t, "wt", "rm", "a", "release/1.0", flag, "--yes", "--offline"); code == 0 || !strings.Contains(errs, "-d/-D do not apply") {
			t.Errorf("wt rm %s: exit %d, stderr:\n%s", flag, code, errs)
		}
	}
	_, errs, code = e.run(t, "wt", "rm", "a", "release/1.0", "--offline")
	if code == 0 || !strings.Contains(errs, "[tag release/1.0]") {
		t.Errorf("wt rm without --yes: exit %d, stderr:\n%s", code, errs)
	}
	out = e.mustRun(t, "wt", "rm", "a", "release/1.0", "--yes", "--offline")
	if !strings.Contains(out, "removed worktree release-1.0 [tag release/1.0]") || exists(tw) {
		t.Errorf("wt rm: %q", out)
	}
}

func TestCompleteTagWorktrees(t *testing.T) {
	t.Parallel()
	e, root, _ := tagBundle(t)
	e.mustRun(t, "wt", "add", "a", "--tag", "v1", "--no-fetch")
	// Local tags only (release/1.0 is on origin but not fetched), without
	// the ones that already have a worktree.
	e.wantComplete(t, []string{"v2"}, "wt", "add", "a", "--tag", "")
	e.wantComplete(t, []string{"a", "b"}, "wt", "add", "--tag", "v2", "")
	e.cwd = filepath.Join(root, "a", "main")
	e.wantComplete(t, []string{"v2"}, "wt", "add", "--tag", "")
	e.cwd = root
	e.wantComplete(t, []string{"v1"}, "wt", "rm", "a", "")
	e.wantComplete(t, []string{"main", "v1"}, "path", "a", "")
	// A tag worktree is completed by directory and by tag name.
	e.mustRun(t, "wt", "add", "a", "--tag", "release/1.0")
	e.wantComplete(t, []string{"main", "release-1.0", "release/1.0", "v1"}, "path", "a", "")
	e.wantComplete(t, []string{"release-1.0", "release/1.0", "v1"}, "wt", "rm", "a", "")
}

// wt add --base with a start point that does not exist.
func TestWorktreeAddBaseNotFoundCLI(t *testing.T) {
	t.Parallel()
	e, _, o := tagBundle(t)
	testutil.Git(t, o.Path, "branch", "2.9", "main")
	_, errs, code := e.run(t, "wt", "add", "a", "fix", "--base", "origin/2.9.x")
	want := "base origin/2.9.x not found in a (after fetching origin); closest: origin/2.9"
	if code == 0 || !strings.Contains(errs, want) || strings.Contains(errs, "Needed a single revision") {
		t.Errorf("exit %d, stderr:\n%s\nwant %q", code, errs, want)
	}
}

// The TUI removal dialog of a tag worktree names the tag and shows the
// commits at risk; -d is refused before any dialog.
func TestTUIWorktreeRemoveTagWorktree(t *testing.T) {
	e, root, _ := tagBundle(t)
	tw := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "--tag", "v1", "--no-fetch"))
	testutil.CommitFile(t, tw, "x.txt", "x\n", "detached work")
	var out syncBuffer
	errs, code := e.runTUI(t, newScript(t, false), &out, "wt", "rm", "-d", "a", "v1")
	if code == 0 || !strings.Contains(out.String()+errs, "-d/-D do not apply") {
		t.Errorf("-d: exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	out = syncBuffer{}
	in := newScript(t, true, []key{{keys: "y", wait: waitOutput(t, &out, "[y/N]")}})
	errs, code = e.runTUI(t, in, &out, "wt", "rm", "--force", "a", "v1")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errs)
	}
	if exists(tw) {
		t.Fatal("worktree not removed")
	}
	for _, want := range []string{"will remove worktree v1 [tag v1] at " + filepath.Join(root, "a", "v1"), "detached HEAD"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dialog lacks %q:\n%s", want, out.String())
		}
	}
}

func TestTagWorktreeMovedCLI(t *testing.T) {
	t.Parallel()
	e, _, _ := tagBundle(t)
	tw := strings.TrimSpace(e.mustRun(t, "wt", "add", "a", "--tag", "v1", "--no-fetch"))
	testutil.CommitFile(t, tw, "x.txt", "x\n", "on detached HEAD")
	head := testutil.Git(t, tw, "rev-parse", "HEAD")
	want := "(tag v1, moved to " + head[:7] + ")"
	if list := e.mustRun(t, "wt", "list", "a"); !strings.Contains(list, want) {
		t.Errorf("wt list lacks %q:\n%s", want, list)
	}
	if st := e.mustRun(t, "status", "--repo", "a"); !strings.Contains(st, want) {
		t.Errorf("status lacks %q:\n%s", want, st)
	}
	var doc struct {
		Worktrees []map[string]any `json:"worktrees"`
	}
	if err := json.Unmarshal([]byte(e.mustRun(t, "wt", "list", "--json", "a")), &doc); err != nil {
		t.Fatal(err)
	}
	var sdoc struct {
		Repositories []struct {
			Checkouts []map[string]any `json:"checkouts"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(e.mustRun(t, "status", "--json", "--repo", "a")), &sdoc); err != nil {
		t.Fatal(err)
	}
	all := doc.Worktrees
	for _, r := range sdoc.Repositories {
		all = append(all, r.Checkouts...)
	}
	n := 0
	for _, w := range all {
		switch w["dir"] {
		case "v1":
			n++
			if w["tag"] != "v1" || w["tag_moved"] != true || w["head"] != head {
				t.Errorf("JSON entry %v, want tag v1, tag_moved, head %s", w, head)
			}
		case "main":
			if w["tag_moved"] != false {
				t.Errorf("JSON entry %v, want tag_moved false", w)
			}
		}
	}
	if n != 2 {
		t.Errorf("v1 found %d times in wt list and status JSON", n)
	}
	up := e.mustRun(t, "update", "--repo", "a")
	if !strings.Contains(up, "detached (was pinned at tag v1; moved to "+head[:10]+")") || !strings.Contains(up, "1 skipped") {
		t.Errorf("update:\n%s", up)
	}
}

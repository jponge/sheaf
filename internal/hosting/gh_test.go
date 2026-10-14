package hosting

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	listArgs = "repo list smallrye --limit 1001 --json name,owner,url,sshUrl,defaultBranchRef,isArchived,isFork,description"
	viewArgs = "repo view jponge/boiler-vroom --json name,owner,url,sshUrl,defaultBranchRef,isArchived,isFork,description"
	cfgArgs  = "config get -h github.com git_protocol"
)

// resp is the canned behaviour of the stub gh for one "<cmd> <subcmd>" pair.
type resp struct {
	fixture string // file under testdata printed on stdout
	stdout  string // literal stdout line
	stderr  string
	code    int
	sleep   bool // sleep 30s (as a child process, so killing the shell leaves an orphan holding the pipes)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// stubGH writes a shell script acting as gh. It records one line per
// invocation (all args joined by spaces) and returns the script path and a
// function reading the recorded lines.
func stubGH(t *testing.T, rules map[string]resp) (string, func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	td, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("#!/bin/sh\necho \"$*\" >> " + shq(log) + "\ncase \"$1 $2\" in\n")
	for key, r := range rules {
		sb.WriteString("\"" + key + "\")\n")
		if r.sleep {
			sb.WriteString("  sleep 30\n")
		}
		if r.fixture != "" {
			sb.WriteString("  cat " + shq(filepath.Join(td, r.fixture)) + "\n")
		}
		if r.stdout != "" {
			sb.WriteString("  printf '%s\\n' " + shq(r.stdout) + "\n")
		}
		if r.stderr != "" {
			sb.WriteString("  printf '%s\\n' " + shq(r.stderr) + " >&2\n")
		}
		sb.WriteString("  exit " + strconv.Itoa(r.code) + " ;;\n")
	}
	sb.WriteString("*) echo \"unexpected: $*\" >&2; exit 99 ;;\nesac\n")
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(sb.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := func() []string {
		b, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return path, calls
}

func TestGHListReposMapsFieldsSSH(t *testing.T) {
	path, calls := stubGH(t, map[string]resp{
		"repo list": {fixture: "repo_list.json"},
	})
	gh := NewGH(GHOptions{Path: path, Protocol: "ssh"})
	repos, err := gh.ListRepos(context.Background(), "smallrye")
	if err != nil {
		t.Fatal(err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{listArgs}) {
		t.Fatalf("calls = %q", got)
	}
	if len(repos) != 4 {
		t.Fatalf("len = %d", len(repos))
	}
	z := repos[0]
	want := Repo{
		Owner: "smallrye", Name: "zeta",
		CloneURL:      "git@github.com:smallrye/zeta.git",
		HTTPSURL:      "https://github.com/smallrye/zeta.git",
		SSHURL:        "git@github.com:smallrye/zeta.git",
		DefaultBranch: "main", Description: "Zeta library",
	}
	if z != want {
		t.Fatalf("got %+v\nwant %+v", z, want)
	}
	if z.Slug() != "smallrye/zeta" {
		t.Fatalf("slug %q", z.Slug())
	}
	if !repos[1].Archived || repos[1].Fork || repos[1].DefaultBranch != "master" {
		t.Fatalf("archived mapping: %+v", repos[1])
	}
	if !repos[2].Fork || repos[2].Archived {
		t.Fatalf("fork mapping: %+v", repos[2])
	}
	if repos[3].DefaultBranch != "" {
		t.Fatalf("empty repo default branch: %+v", repos[3])
	}
}

func TestGHProtocolFromConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     resp
		wantURL string
	}{
		{"ssh", resp{stdout: "ssh"}, "git@github.com:jponge/boiler-vroom.git"},
		{"https", resp{stdout: "https"}, "https://github.com/jponge/boiler-vroom.git"},
		{"empty", resp{}, "https://github.com/jponge/boiler-vroom.git"},
		{"nonzero exit caches https", resp{stderr: "key not set", code: 1}, "https://github.com/jponge/boiler-vroom.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("GH_HOST", "")
			path, calls := stubGH(t, map[string]resp{"repo view": {fixture: "repo_view.json"}, "config get": c.cfg})
			gh := NewGH(GHOptions{Path: path})
			r, err := gh.Repo(context.Background(), "jponge", "boiler-vroom")
			if err != nil {
				t.Fatal(err)
			}
			if r.CloneURL != c.wantURL {
				t.Fatalf("CloneURL = %q, want %q", r.CloneURL, c.wantURL)
			}
			if r.SSHURL != "git@github.com:jponge/boiler-vroom.git" || r.HTTPSURL != "https://github.com/jponge/boiler-vroom.git" {
				t.Fatalf("urls: %+v", r)
			}
			if got := calls(); !reflect.DeepEqual(got, []string{cfgArgs, viewArgs}) {
				t.Fatalf("calls = %q", got)
			}
			// protocol is resolved once per adapter
			if _, err := gh.Repo(context.Background(), "jponge", "boiler-vroom"); err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, l := range calls() {
				if l == cfgArgs {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("config get invoked %d times", n)
			}
		})
	}
}

func TestGHProtocolUsesGHHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.com")
	path, calls := stubGH(t, map[string]resp{"repo view": {fixture: "repo_view.json"}, "config get": {stdout: "ssh"}})
	r, err := NewGH(GHOptions{Path: path}).Repo(context.Background(), "jponge", "boiler-vroom")
	if err != nil {
		t.Fatal(err)
	}
	if r.CloneURL != r.SSHURL {
		t.Fatalf("CloneURL = %q", r.CloneURL)
	}
	if got := calls(); len(got) != 2 || got[0] != "config get -h ghe.example.com git_protocol" {
		t.Fatalf("calls = %q", got)
	}
}

func TestGHProtocolContextErrorNotCached(t *testing.T) {
	t.Setenv("GH_HOST", "")
	path, calls := stubGH(t, map[string]resp{"repo view": {fixture: "repo_view.json"}, "config get": {stdout: "ssh"}})
	gh := NewGH(GHOptions{Path: path})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gh.Repo(ctx, "jponge", "boiler-vroom"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	r, err := gh.Repo(context.Background(), "jponge", "boiler-vroom")
	if err != nil {
		t.Fatal(err)
	}
	if r.CloneURL != r.SSHURL {
		t.Fatalf("a cancelled lookup must not cache https; CloneURL = %q (calls %q)", r.CloneURL, calls())
	}
}

func TestGHProtocolGHMissing(t *testing.T) {
	t.Setenv("GH_HOST", "")
	gh := NewGH(GHOptions{Path: filepath.Join(t.TempDir(), "nope")})
	if _, err := gh.Repo(context.Background(), "o", "r"); !errors.Is(err, ErrGHNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGHProtocolOptionOverridesConfigWithoutCallingIt(t *testing.T) {
	path, calls := stubGH(t, map[string]resp{"repo view": {fixture: "repo_view.json"}, "config get": {stdout: "ssh"}})
	gh := NewGH(GHOptions{Path: path, Protocol: "https"})
	r, err := gh.Repo(context.Background(), "jponge", "boiler-vroom")
	if err != nil {
		t.Fatal(err)
	}
	if r.CloneURL != "https://github.com/jponge/boiler-vroom.git" {
		t.Fatalf("CloneURL = %q", r.CloneURL)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{viewArgs}) {
		t.Fatalf("calls = %q", got)
	}
}

func TestGHInvalidProtocolOption(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo view": {fixture: "repo_view.json"}})
	gh := NewGH(GHOptions{Path: path, Protocol: "ftp"})
	_, err := gh.Repo(context.Background(), "jponge", "boiler-vroom")
	if err == nil || !strings.Contains(err.Error(), "ftp") {
		t.Fatalf("err = %v", err)
	}
}

func TestGHListReposEmptyOrg(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo list": {fixture: "empty_list.json"}})
	repos, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).ListRepos(context.Background(), "smallrye")
	if err != nil {
		t.Fatal(err)
	}
	if repos == nil || len(repos) != 0 {
		t.Fatalf("repos = %#v, want empty non-nil slice", repos)
	}
}

func TestGHListReposLimit(t *testing.T) {
	// The stub always returns 4 repos.
	t.Run("exactly limit: no error", func(t *testing.T) {
		path, calls := stubGH(t, map[string]resp{"repo list": {fixture: "repo_list.json"}})
		repos, err := NewGH(GHOptions{Path: path, Protocol: "ssh", Limit: 4}).ListRepos(context.Background(), "smallrye")
		if err != nil || len(repos) != 4 {
			t.Fatalf("got %d, %v", len(repos), err)
		}
		if got := calls(); len(got) != 1 || !strings.Contains(got[0], "--limit 5 ") {
			t.Fatalf("calls = %q", got)
		}
	})
	t.Run("more than limit: error and trimmed", func(t *testing.T) {
		path, _ := stubGH(t, map[string]resp{"repo list": {fixture: "repo_list.json"}})
		repos, err := NewGH(GHOptions{Path: path, Protocol: "ssh", Limit: 3}).ListRepos(context.Background(), "smallrye")
		if !errors.Is(err, ErrPossiblyTruncated) {
			t.Fatalf("err = %v", err)
		}
		if len(repos) != 3 {
			t.Fatalf("partial result must be trimmed to the limit, got %d", len(repos))
		}
	})
}

func TestGHFailureSurfacesStderr(t *testing.T) {
	cases := map[string]string{
		"not authenticated": "To get started with GitHub CLI, please run:  gh auth login",
		"org not found":     "GraphQL: Could not resolve to an Organization with the login of 'nope'.",
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			path, _ := stubGH(t, map[string]resp{"repo list": {stderr: msg, code: 1}})
			_, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).ListRepos(context.Background(), "smallrye")
			if err == nil || !strings.Contains(err.Error(), msg) {
				t.Fatalf("err = %v, want stderr %q", err, msg)
			}
			if strings.HasSuffix(err.Error(), ": ") || !strings.Contains(err.Error(), "repo list smallrye") {
				t.Fatalf("error should name the command: %v", err)
			}
		})
	}
}

func TestGHMalformedJSON(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{
		"repo list": {fixture: "malformed.json"},
		"repo view": {fixture: "malformed.json"},
		"pr view":   {fixture: "malformed.json"},
	})
	gh := NewGH(GHOptions{Path: path, Protocol: "ssh"})
	ctx := context.Background()
	if _, err := gh.ListRepos(ctx, "o"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("ListRepos err = %v", err)
	}
	if _, err := gh.Repo(ctx, "o", "r"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("Repo err = %v", err)
	}
	if _, err := gh.PR(ctx, "o", "r", 1); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("PR err = %v", err)
	}
}

func TestGHBinaryMissing(t *testing.T) {
	for name, p := range map[string]string{
		"absolute path": filepath.Join(t.TempDir(), "no-such-gh"),
		"not on PATH":   "sheaf-definitely-no-such-gh",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			_, err := NewGH(GHOptions{Path: p, Protocol: "ssh"}).ListRepos(context.Background(), "o")
			if !errors.Is(err, ErrGHNotFound) {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(err.Error(), "install") || !strings.Contains(err.Error(), "https://cli.github.com") {
				t.Fatalf("message should suggest installing gh: %v", err)
			}
		})
	}
}

func TestGHPR(t *testing.T) {
	const prCross = "pr view 42 --repo jponge/boiler-vroom --json number,headRefName,headRepository,headRepositoryOwner,isCrossRepository,url"
	path, calls := stubGH(t, map[string]resp{"pr view": {fixture: "pr_view_cross.json"}})
	p, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).PR(context.Background(), "jponge", "boiler-vroom", 42)
	if err != nil {
		t.Fatal(err)
	}
	want := PR{Number: 42, HeadBranch: "feat/cli/rework", HeadOwner: "alice", HeadRepo: "boiler-vroom-fork",
		IsCrossRepository: true, URL: "https://github.com/jponge/boiler-vroom/pull/42"}
	if p != want {
		t.Fatalf("got %+v\nwant %+v", p, want)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{prCross}) {
		t.Fatalf("calls = %q", got)
	}
}

func TestGHPRSameRepoAndDeletedFork(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"pr view": {fixture: "pr_view_same.json"}})
	p, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).PR(context.Background(), "jponge", "boiler-vroom", 7)
	if err != nil || p.IsCrossRepository || p.HeadOwner != "jponge" || p.HeadRepo != "boiler-vroom" {
		t.Fatalf("same repo: %+v, %v", p, err)
	}
	path, _ = stubGH(t, map[string]resp{"pr view": {fixture: "pr_view_deleted_fork.json"}})
	p, err = NewGH(GHOptions{Path: path, Protocol: "ssh"}).PR(context.Background(), "jponge", "boiler-vroom", 9)
	if err != nil || !p.IsCrossRepository || p.HeadOwner != "" || p.HeadRepo != "" || p.HeadBranch != "gone" {
		t.Fatalf("deleted fork: %+v, %v", p, err)
	}
}

func TestGHFailureWithEmptyStderrHasNoDanglingColon(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo list": {code: 1}})
	_, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).ListRepos(context.Background(), "smallrye")
	if err == nil || strings.HasSuffix(strings.TrimSpace(err.Error()), ":") {
		t.Fatalf("err = %q", err)
	}
}

func TestGHNotFoundMapped(t *testing.T) {
	gql := func(what, detail string) string {
		return "GraphQL: Could not resolve to a " + what + " " + detail
	}
	repoMsg := gql("Repository", "with the name 'o/n'. (repository)")
	prMsg := gql("PullRequest", "with the number of 42. (repository.pullRequest)")
	ownerMsg := gql("RepositoryOwner", "with the login of 'nope'. (repositoryOwner)")
	path, _ := stubGH(t, map[string]resp{
		"repo view": {stderr: repoMsg, code: 1},
		"pr view":   {stderr: prMsg, code: 1},
		"repo list": {stderr: ownerMsg, code: 1},
	})
	gh := NewGH(GHOptions{Path: path, Protocol: "ssh"})
	ctx := context.Background()
	_, err1 := gh.Repo(ctx, "o", "n")
	_, err2 := gh.PR(ctx, "o", "n", 42)
	_, err3 := gh.ListRepos(ctx, "nope")
	for i, c := range []struct {
		err error
		msg string
	}{{err1, repoMsg}, {err2, prMsg}, {err3, ownerMsg}} {
		if !errors.Is(c.err, ErrNotFound) || !strings.Contains(c.err.Error(), c.msg) {
			t.Errorf("case %d: err = %v", i, c.err)
		}
	}
	// other failures are not "not found"
	path, _ = stubGH(t, map[string]resp{"repo view": {stderr: "HTTP 502", code: 1}})
	if _, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).Repo(ctx, "o", "n"); errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGHInputValidation(t *testing.T) {
	path, calls := stubGH(t, map[string]resp{"repo view": {fixture: "repo_view.json"}, "repo list": {fixture: "empty_list.json"}, "pr view": {fixture: "pr_view_same.json"}})
	gh := NewGH(GHOptions{Path: path, Protocol: "ssh"})
	ctx := context.Background()
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v", what, err)
		}
	}
	for _, o := range []string{"", "-x", "a/b", "a b", "--json", "a\tb"} {
		_, err := gh.ListRepos(ctx, o)
		check("owner "+o, err)
		_, err = gh.Repo(ctx, o, "r")
		check("repo owner "+o, err)
		_, err = gh.PR(ctx, o, "r", 1)
		check("pr owner "+o, err)
	}
	for _, n := range []string{"", ".", "..", "a/b", "a b", "a\nb"} {
		_, err := gh.Repo(ctx, "o", n)
		check("name "+n, err)
		_, err = gh.PR(ctx, "o", n, 1)
		check("pr name "+n, err)
	}
	for _, n := range []int{0, -3} {
		_, err := gh.PR(ctx, "o", "r", n)
		check("pr number", err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("gh must not be invoked on invalid input: %q", got)
	}
	// valid edge cases
	for _, c := range [][2]string{{"a-b1", "my.repo_x-1"}, {"smallrye", ".github"}, {"o", "-r"}, {"o", ".hidden"}, {"name_shortcode", "r"}, {"o", "r;x"}} {
		if _, err := gh.Repo(ctx, c[0], c[1]); err != nil {
			t.Errorf("%v rejected: %v", c, err)
		}
		if _, err := gh.PR(ctx, c[0], c[1], 1); err != nil {
			t.Errorf("PR %v rejected: %v", c, err)
		}
	}
	if _, err := gh.ListRepos(ctx, "name_shortcode"); err != nil {
		t.Errorf("owner with underscore rejected: %v", err)
	}
}

func TestGHLimitOverflowIsCapped(t *testing.T) {
	path, calls := stubGH(t, map[string]resp{"repo list": {fixture: "empty_list.json"}})
	if _, err := NewGH(GHOptions{Path: path, Protocol: "ssh", Limit: math.MaxInt}).ListRepos(context.Background(), "o"); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) != 1 || strings.Contains(got[0], "--limit -") {
		t.Fatalf("calls = %q", got)
	}
}

func TestGHContextDeadline(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo list": {sleep: true}})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).ListRepos(ctx, "smallrye")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("took %v", el)
	}
}

func TestGHChildEnvironment(t *testing.T) {
	t.Setenv("GH_FORCE_TTY", "100")
	t.Setenv("GH_PAGER", "less")
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	script := "#!/bin/sh\nenv > " + shq(out) + "\necho '[]'\n"
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).ListRepos(context.Background(), "o"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	env := string(b)
	for _, want := range []string{"GH_PROMPT_DISABLED=1", "NO_COLOR=1", "GH_PAGER=cat", "CLICOLOR_FORCE=0"} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("missing %s in child env", want)
		}
	}
	if strings.Contains(env, "GH_FORCE_TTY") || strings.Contains(env, "GH_PAGER=less") {
		t.Errorf("unexpected env:\n%s", env)
	}
}

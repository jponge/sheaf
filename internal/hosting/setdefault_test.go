package hosting

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// unauthenticatedStderr is what gh 2.46.0 prints on stderr, with exit code
// 4, for `gh repo set-default cli/cli` when no credentials are configured
// (recorded 2026-10-16 with an empty GH_CONFIG_DIR, in a clone whose origin
// is https://github.com/cli/cli.git; nothing on stdout). `gh help
// exit-codes`: "If a command encounters an authentication issue, the exit
// code will be 4".
const unauthenticatedStderr = "To get started with GitHub CLI, please run:  gh auth login\n" +
	"Alternatively, populate the GH_TOKEN environment variable with a GitHub API authentication token."

// badCredentialsStderr is what gh 2.46.0 prints, with exit code 1, for the
// same command with GH_TOKEN set to an invalid token (recorded 2026-10-16).
const badCredentialsStderr = "HTTP 401: Bad credentials (https://api.github.com/graphql)\n" +
	"Try authenticating with:  gh auth login"

// noGitHubRemoteStderr is what gh 2.46.0 prints, with exit code 1, in a
// clone whose only remote is a file:// URL (recorded 2026-10-16).
const noGitHubRemoteStderr = "none of the git remotes configured for this repository point to a known GitHub host. " +
	"To tell gh about a new GitHub host, please use `gh auth login`"

func TestGHSetDefaultRepoRunsInTheCheckout(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere/.git") // inherited from a git hook: must not redirect gh's git calls
	// real_set_default_ok.txt: gh's confirmation, recorded by the user on
	// 2026-10-16 (stream unknown; printed on stdout here and ignored).
	path, calls := stubGH(t, map[string]resp{"repo set-default": {cwd: true, fixture: "real_set_default_ok.txt"}})
	dir := t.TempDir()
	if err := NewGH(GHOptions{Path: path}).SetDefaultRepo(context.Background(), dir, "Up", "x"); err != nil {
		t.Fatal(err)
	}
	phys, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"repo set-default Up/x", "cwd=" + phys + " GIT_DIR=<unset>"}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %q\nwant    %q", got, want)
	}
}

func TestGHSetDefaultRepoUnauthenticated(t *testing.T) {
	for name, r := range map[string]resp{
		"no credentials (exit 4)": {stderr: unauthenticatedStderr, code: 4},
		"bad credentials":         {stderr: badCredentialsStderr, code: 1},
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := stubGH(t, map[string]resp{"repo set-default": r})
			err := NewGH(GHOptions{Path: path}).SetDefaultRepo(context.Background(), t.TempDir(), "o", "r")
			if !errors.Is(err, ErrGHNotAuthenticated) {
				t.Fatalf("err = %v, want ErrGHNotAuthenticated", err)
			}
			if !strings.Contains(err.Error(), "gh auth login") {
				t.Fatalf("err = %v, want gh's message kept", err)
			}
		})
	}
}

// Other authentication failures of the other commands are classified the
// same way (exit code 4).
func TestGHUnauthenticatedMappedForEveryCommand(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo list": {stderr: unauthenticatedStderr, code: 4}})
	_, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).ListRepos(context.Background(), "o")
	if !errors.Is(err, ErrGHNotAuthenticated) {
		t.Fatalf("err = %v, want ErrGHNotAuthenticated", err)
	}
}

func TestGHSetDefaultRepoOtherFailure(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo set-default": {stderr: noGitHubRemoteStderr, code: 1}})
	err := NewGH(GHOptions{Path: path}).SetDefaultRepo(context.Background(), t.TempDir(), "o", "r")
	if err == nil || errors.Is(err, ErrGHNotAuthenticated) || !strings.Contains(err.Error(), "known GitHub host") {
		t.Fatalf("err = %v", err)
	}
}

func TestGHSetDefaultRepoGHMissing(t *testing.T) {
	err := NewGH(GHOptions{Path: filepath.Join(t.TempDir(), "no-gh")}).SetDefaultRepo(context.Background(), t.TempDir(), "o", "r")
	if !errors.Is(err, ErrGHNotFound) {
		t.Fatalf("err = %v, want ErrGHNotFound", err)
	}
}

// gh resolves the repository from the working directory: an empty or
// relative directory would act on sheaf's own working directory.
func TestGHSetDefaultRepoValidatesInput(t *testing.T) {
	path, calls := stubGH(t, map[string]resp{"repo set-default": {}})
	gh := NewGH(GHOptions{Path: path})
	abs := t.TempDir()
	for _, c := range []struct{ dir, owner, name string }{
		{"", "o", "r"},
		{"rel/dir", "o", "r"},
		{abs, "-o", "r"},
		{abs, "o/x", "r"},
		{abs, "o", ""},
		{abs, "o", "a b"},
	} {
		if err := gh.SetDefaultRepo(context.Background(), c.dir, c.owner, c.name); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("SetDefaultRepo(%q, %q, %q) = %v, want ErrInvalidInput", c.dir, c.owner, c.name, err)
		}
	}
	if got := calls(); got != nil {
		t.Fatalf("gh called: %q", got)
	}
}

// A checkout directory that vanished is that repository's problem, not a
// missing gh (which would stop sheaf from running gh for the whole command).
func TestGHSetDefaultRepoMissingDirIsNotGHMissing(t *testing.T) {
	path, calls := stubGH(t, map[string]resp{"repo set-default": {}})
	err := NewGH(GHOptions{Path: path}).SetDefaultRepo(context.Background(), filepath.Join(t.TempDir(), "gone"), "o", "r")
	if err == nil || errors.Is(err, ErrGHNotFound) || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("err = %v, want a failure naming the directory, not ErrGHNotFound", err)
	}
	if got := calls(); got != nil {
		t.Fatalf("gh called: %q", got)
	}
}

// A GraphQL "not found" answer means gh is authenticated: it wins over the
// exit code.
func TestGHNotFoundWinsOverAuthExitCode(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo view": {stderr: "GraphQL: Could not resolve to a Repository with the name 'o/r'. (repository)", code: 4}})
	_, err := NewGH(GHOptions{Path: path, Protocol: "ssh"}).Repo(context.Background(), "o", "r")
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrGHNotAuthenticated) {
		t.Fatalf("err = %v, want ErrNotFound only", err)
	}
}

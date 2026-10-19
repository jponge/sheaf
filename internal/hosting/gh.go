package hosting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jponge/sheaf/internal/gitx"
)

// DefaultLimit is the default maximum number of repositories requested from
// `gh repo list`. gh's own default is 30, which would silently truncate.
const DefaultLimit = 1000

var (
	// ErrGHNotFound means the gh executable could not be located.
	ErrGHNotFound = errors.New("gh executable not found")
	// ErrGHNotAuthenticated means gh has no credentials (exit code 4, see
	// `gh help exit-codes`) or the service rejected them.
	ErrGHNotAuthenticated = errors.New("gh is not authenticated")
	// ErrPossiblyTruncated is returned by ListRepos, together with the result
	// trimmed to Limit, when gh returned more than Limit repositories (Limit+1 is
	// requested to make this exact).
	ErrPossiblyTruncated = errors.New("repository list possibly truncated")
)

const (
	repoFields = "name,owner,url,sshUrl,defaultBranchRef,isArchived,isFork,description"
	prFields   = "number,headRefName,headRepository,headRepositoryOwner,isCrossRepository,url"
)

// GHOptions configures the gh adapter.
type GHOptions struct {
	// Path of the gh executable; empty means "gh" looked up on PATH.
	Path string
	// Protocol selects CloneURL: "ssh" or "https". Empty means: ask
	// `gh config get git_protocol` once (ssh -> SSH, anything else or failure -> HTTPS).
	Protocol string
	// Limit is the maximum number of repos requested by ListRepos; 0 means DefaultLimit.
	Limit int
}

// GH implements Hosting on top of the gh CLI.
type GH struct {
	opts GHOptions

	mu       sync.Mutex
	resolved bool
	proto    string
}

// NewGH returns a gh-backed Hosting.
func NewGH(opts GHOptions) *GH {
	if opts.Path == "" {
		opts.Path = "gh"
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	if opts.Limit > math.MaxInt32 { // keeps Limit+1 from overflowing
		opts.Limit = math.MaxInt32
	}
	return &GH{opts: opts}
}

// childEnv returns the environment for gh: the caller's, minus variables that
// would make gh page or colorize its output, plus non-interactive settings.
func childEnv() []string {
	drop := map[string]bool{"GH_FORCE_TTY": true, "GH_PAGER": true, "NO_COLOR": true,
		"CLICOLOR_FORCE": true, "GH_PROMPT_DISABLED": true}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	return append(env, "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "GH_PAGER=cat", "CLICOLOR_FORCE=0")
}

// notFoundMarkers are substrings of the stderr gh prints for a missing
// repository, pull request or owner. Verified against real gh output on
// 2026-10-14 (the gh version on the recording machine is unknown):
//
//	repo view: GraphQL: Could not resolve to a Repository with the name 'o/n'. (repository)
//	pr view:   GraphQL: Could not resolve to a PullRequest with the number of 999999. (repository.pullRequest)
//	repo list: the owner handle "x" was not recognized as either a GitHub user or an organization
var notFoundMarkers = []string{
	"Could not resolve to a",
	"was not recognized as either a GitHub user or an organization",
}

func isNotFound(stderr string) bool {
	for _, m := range notFoundMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// ghExitAuth is gh's exit code for authentication issues (`gh help exit-codes`).
const ghExitAuth = 4

// badCredentialsMarker is a substring of gh's stderr when the service
// rejects the token (exit code 1). Verified with gh 2.46.0 on 2026-10-16:
//
//	HTTP 401: Bad credentials (https://api.github.com/graphql)
//	Try authenticating with:  gh auth login
const badCredentialsMarker = "HTTP 401: Bad credentials"

func isUnauthenticated(err error, stderr string) bool {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == ghExitAuth {
		return true
	}
	return strings.Contains(stderr, badCredentialsMarker)
}

// isChdirError reports whether err is the failure to enter the working
// directory of the command (os.StartProcess reports it as a *fs.PathError
// with Op "chdir"), not a missing executable.
func isChdirError(err error) bool {
	pe, ok := errors.AsType[*fs.PathError](err)
	return ok && pe.Op == "chdir"
}

func (g *GH) run(ctx context.Context, args ...string) ([]byte, error) {
	return g.runIn(ctx, "", args...)
}

// runIn runs gh in dir (empty: the current directory), without the
// variables that redirect git to another repository when dir is set.
func (g *GH) runIn(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.opts.Path, args...)
	cmd.Env = childEnv()
	if dir != "" {
		cmd.Dir = dir
		cmd.Env = gitx.StripRepoEnv(cmd.Env)
	}
	// Bound the wait for orphaned grandchildren holding the output pipes after a kill.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), cerr)
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) && !isChdirError(err) {
			return nil, fmt.Errorf("%w (%s): install the GitHub CLI from https://cli.github.com/ and run `gh auth login`: %w",
				ErrGHNotFound, g.opts.Path, err)
		}
		msg := strings.TrimSpace(stderr.String())
		wrapped := err
		if isNotFound(msg) {
			wrapped = fmt.Errorf("%w: %w", ErrNotFound, err)
		} else if isUnauthenticated(err, msg) {
			wrapped = fmt.Errorf("%w: %w", ErrGHNotAuthenticated, err)
		}
		if msg == "" {
			return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), wrapped)
		}
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), wrapped, msg)
	}
	return stdout.Bytes(), nil
}

func ghHost() string {
	if h := os.Getenv("GH_HOST"); h != "" {
		return h
	}
	return "github.com"
}

func (g *GH) protocol(ctx context.Context) (string, error) {
	switch p := strings.ToLower(g.opts.Protocol); p {
	case "ssh", "https":
		return p, nil
	case "":
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.resolved {
			return g.proto, nil
		}
		// The protocol chosen at `gh auth login` is stored per host; -h falls back to the global setting.
		out, err := g.run(ctx, "config", "get", "-h", ghHost(), "git_protocol")
		if err != nil && (ctx.Err() != nil || errors.Is(err, ErrGHNotFound)) {
			return "", err
		}
		g.proto = "https"
		if err == nil && strings.TrimSpace(string(out)) == "ssh" {
			g.proto = "ssh"
		}
		// A non-zero exit (key unset on some gh versions) defaults to https and is cached.
		g.resolved = true
		return g.proto, nil
	default:
		return "", fmt.Errorf("%w: clone protocol %q: want ssh or https", ErrInvalidInput, g.opts.Protocol)
	}
}

type ghRepo struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	URL              string `json:"url"`
	SSHURL           string `json:"sshUrl"`
	DefaultBranchRef *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
	IsArchived  bool   `json:"isArchived"`
	IsFork      bool   `json:"isFork"`
	Description string `json:"description"`
}

func (r ghRepo) toRepo(proto string) Repo {
	out := Repo{
		Owner:       r.Owner.Login,
		Name:        r.Name,
		HTTPSURL:    r.URL + ".git", // gh exposes the web URL; the HTTPS clone URL is that plus ".git"
		SSHURL:      r.SSHURL,
		Archived:    r.IsArchived,
		Fork:        r.IsFork,
		Description: r.Description,
	}
	if r.DefaultBranchRef != nil {
		out.DefaultBranch = r.DefaultBranchRef.Name
	}
	out.CloneURL = out.HTTPSURL
	if proto == "ssh" {
		out.CloneURL = out.SSHURL
	}
	return out
}

// ListRepos lists the repositories owned by owner (a user or an organization).
func (g *GH) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	if err := ValidateOwner(owner); err != nil {
		return nil, err
	}
	proto, err := g.protocol(ctx)
	if err != nil {
		return nil, err
	}
	// Ask for one more than the limit so that truncation is detectable.
	out, err := g.run(ctx, "repo", "list", owner, "--limit", strconv.Itoa(g.opts.Limit+1), "--json", repoFields)
	if err != nil {
		return nil, err
	}
	var raw []ghRepo
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh repo list output for %q: %w", owner, err)
	}
	repos := make([]Repo, 0, len(raw))
	for _, r := range raw {
		repos = append(repos, r.toRepo(proto))
	}
	if len(repos) > g.opts.Limit {
		return repos[:g.opts.Limit], fmt.Errorf("%w: %q has more than %d repositories", ErrPossiblyTruncated, owner, g.opts.Limit)
	}
	return repos, nil
}

// Repo fetches one repository.
func (g *GH) Repo(ctx context.Context, owner, name string) (Repo, error) {
	if err := ValidateRepo(owner, name); err != nil {
		return Repo{}, err
	}
	proto, err := g.protocol(ctx)
	if err != nil {
		return Repo{}, err
	}
	out, err := g.run(ctx, "repo", "view", owner+"/"+name, "--json", repoFields)
	if err != nil {
		return Repo{}, err
	}
	var raw ghRepo
	if err := json.Unmarshal(out, &raw); err != nil {
		return Repo{}, fmt.Errorf("parse gh repo view output for %s/%s: %w", owner, name, err)
	}
	return raw.toRepo(proto), nil
}

// PR fetches the head information of a pull request.
func (g *GH) PR(ctx context.Context, owner, name string, number int) (PR, error) {
	if err := ValidatePR(owner, name, number); err != nil {
		return PR{}, err
	}
	out, err := g.run(ctx, "pr", "view", strconv.Itoa(number), "--repo", owner+"/"+name, "--json", prFields)
	if err != nil {
		return PR{}, err
	}
	var raw struct {
		Number         int    `json:"number"`
		HeadRefName    string `json:"headRefName"`
		HeadRepository *struct {
			Name string `json:"name"`
		} `json:"headRepository"`
		HeadRepositoryOwner *struct {
			Login string `json:"login"`
		} `json:"headRepositoryOwner"`
		IsCrossRepository bool   `json:"isCrossRepository"`
		URL               string `json:"url"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return PR{}, fmt.Errorf("parse gh pr view output for %s/%s#%d: %w", owner, name, number, err)
	}
	p := PR{Number: raw.Number, HeadBranch: raw.HeadRefName, IsCrossRepository: raw.IsCrossRepository, URL: raw.URL}
	if raw.HeadRepository != nil {
		p.HeadRepo = raw.HeadRepository.Name
	}
	if raw.HeadRepositoryOwner != nil {
		p.HeadOwner = raw.HeadRepositoryOwner.Login
	}
	return p, nil
}

const forkFields = "name,owner,parent,url,sshUrl,isFork"

// CurrentUser returns the login of the authenticated user
// (`gh api user --jq .login`).
func (g *GH) CurrentUser(ctx context.Context) (string, error) {
	out, err := g.run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(string(out))
	if login == "" {
		return "", errors.New("gh api user: empty login")
	}
	if err := ValidateOwner(login); err != nil {
		return "", fmt.Errorf("gh api user: %w", err)
	}
	return login, nil
}

// ListForks lists the forks owned by owner (`gh repo list <owner> --fork`).
// gh reports the parent by owner and name only. Entries without a parent
// are left out. Like ListRepos, more than Limit forks yields the trimmed
// list together with ErrPossiblyTruncated.
func (g *GH) ListForks(ctx context.Context, owner string) ([]Fork, error) {
	if err := ValidateOwner(owner); err != nil {
		return nil, err
	}
	proto, err := g.protocol(ctx)
	if err != nil {
		return nil, err
	}
	out, err := g.run(ctx, "repo", "list", owner, "--fork", "--limit", strconv.Itoa(g.opts.Limit+1), "--json", forkFields)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ghRepo
		Parent *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"parent"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse gh repo list --fork output for %q: %w", owner, err)
	}
	truncated := len(raw) > g.opts.Limit
	if truncated {
		raw = raw[:g.opts.Limit]
	}
	forks := make([]Fork, 0, len(raw))
	for _, r := range raw {
		if r.Parent == nil || r.Parent.Name == "" || r.Parent.Owner.Login == "" {
			continue
		}
		forks = append(forks, Fork{Repo: r.toRepo(proto), ParentOwner: r.Parent.Owner.Login, ParentName: r.Parent.Name})
	}
	if truncated {
		return forks, fmt.Errorf("%w: %q has more than %d forks", ErrPossiblyTruncated, owner, g.opts.Limit)
	}
	return forks, nil
}

// SetDefaultRepo runs `gh repo set-default <owner>/<name>` in dir, which
// must be an absolute path: gh resolves the local repository from its
// working directory and requires owner/name to match one of its remotes
// (gh stores the choice in the repository's git configuration). Variables
// that would redirect gh's git calls to another repository (GIT_DIR, ...)
// are not passed on.
func (g *GH) SetDefaultRepo(ctx context.Context, dir, owner, name string) error {
	if err := ValidateRepo(owner, name); err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: directory %q must be an absolute path", ErrInvalidInput, dir)
	}
	_, err := g.runIn(ctx, dir, "repo", "set-default", owner+"/"+name)
	return err
}

// Browse runs `gh repo view <owner>/<name> --web` for the home page, `gh pr
// view <n> --repo <owner>/<name> --web` for pull request n and `gh browse
// --repo <owner>/<name> --branch <branch>` for the tree of a branch; gh
// starts the browser. Owner, name and branch are validated first (see
// ValidatePage).
//
// Read in gh's sources (pkg/cmd/repo/view/view.go, pkg/cmd/browse/browse.go,
// v2.46.0 and v2.102.0): `repo view --web` queries the repository first
// (an unknown one fails) and opens https://<host>/<owner>/<name>; with
// --branch it would escape '/' in the branch as %2F (url.QueryEscape),
// while `browse --branch` keeps it (tree/<branch>), hence browse for
// branches.
func (g *GH) Browse(ctx context.Context, owner, name string, page Page) error {
	if err := ValidatePage(owner, name, page); err != nil {
		return err
	}
	slug := owner + "/" + name
	args := []string{"repo", "view", slug, "--web"}
	switch {
	case page.PR > 0:
		args = []string{"pr", "view", strconv.Itoa(page.PR), "--repo", slug, "--web"}
	case page.Branch != "":
		args = []string{"browse", "--repo", slug, "--branch", page.Branch}
	}
	_, err := g.run(ctx, args...)
	return err
}

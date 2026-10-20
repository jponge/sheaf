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
	"regexp"
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
	// ErrGHCredentialsRejected means the service rejected gh's credentials
	// (HTTP 401, e.g. an expired GH_TOKEN); it wraps ErrGHNotAuthenticated.
	ErrGHCredentialsRejected = fmt.Errorf("gh's credentials were rejected: %w", ErrGHNotAuthenticated)
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

// notFoundAnswers match the whole stderr gh prints for a missing
// repository, pull request or owner, and nothing else: a not-found message
// joined to another error (e.g. SAML enforcement, a rate limit) is not
// taken for a missing repository. Verified against real gh output on
// 2026-10-14 (the gh version on the recording machine is unknown):
//
//	repo view: GraphQL: Could not resolve to a Repository with the name 'o/n'. (repository)
//	pr view:   GraphQL: Could not resolve to a PullRequest with the number of 999999. (repository.pullRequest)
//	repo list: the owner handle "x" was not recognized as either a GitHub user or an organization
//
// and, from gh's sources (pkg/cmd/pr/shared/finder.go, v2.46.0 and
// v2.102.0), for `gh pr view <branch>` without a pull request of that head:
//
//	no pull requests found for branch "feat"
//
// The "with the login of" form of the GraphQL message (for an owner) is
// not recorded: it follows the same pattern.
var notFoundAnswers = []*regexp.Regexp{
	regexp.MustCompile(`^(?:GraphQL: )?Could not resolve to an? [A-Za-z]+ with the (?:name|login of) '[^'\n]*'\.(?: \([A-Za-z.]+\))?$`),
	regexp.MustCompile(`^(?:GraphQL: )?Could not resolve to an? [A-Za-z]+ with the number of [0-9]+\.(?: \([A-Za-z.]+\))?$`),
	regexp.MustCompile(`^the owner handle "[^"\n]*" was not recognized as either a GitHub user or an organization$`),
	regexp.MustCompile(`^no pull requests found for branch ".*"$`),
}

// isNotFound reports whether stderr, trimmed, is one of notFoundAnswers.
func isNotFound(stderr string) bool {
	for _, re := range notFoundAnswers {
		if re.MatchString(stderr) {
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

func isUnauthenticated(err error) bool {
	ee, ok := errors.AsType[*exec.ExitError](err)
	return ok && ee.ExitCode() == ghExitAuth
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
	cmd := g.command(ctx, dir, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := g.failure(ctx, args, cmd.Run(), stderr.String()); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// command returns the command running gh with args in dir (see runIn).
func (g *GH) command(ctx context.Context, dir string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.opts.Path, args...)
	cmd.Env = childEnv()
	if dir != "" {
		cmd.Dir = dir
		cmd.Env = gitx.StripRepoEnv(cmd.Env)
	}
	// Bound the wait for orphaned grandchildren holding the output pipes after a kill.
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// GHError is the failure of a gh command that ran (see GH.failure).
type GHError struct {
	Args   []string // the arguments of gh
	Stderr string   // what gh printed on standard error, trimmed
	// Err is the error of the run (an *exec.ExitError), wrapped in
	// ErrNotFound, ErrGHCredentialsRejected or ErrGHNotAuthenticated when
	// gh's answer says so.
	Err error
}

// Error returns one actionable line for the common failures: for
// ErrGHCredentialsRejected, what to check; for ErrGHNotAuthenticated, how
// to log in; for ErrNotFound, what is missing (see notFoundLine). Other failures name the command and keep the error
// of the run and gh's standard error.
func (e *GHError) Error() string {
	switch {
	case errors.Is(e.Err, ErrNotFound):
		return notFoundLine(e.Stderr)
	case errors.Is(e.Err, ErrGHCredentialsRejected):
		return "gh's credentials were rejected (" + badCredentialsMarker + "): check GH_TOKEN or GITHUB_TOKEN, or run `gh auth login`"
	case errors.Is(e.Err, ErrGHNotAuthenticated):
		return ErrGHNotAuthenticated.Error() + ": run `gh auth login`"
	}
	cmd := "gh " + strings.Join(e.Args, " ")
	if e.Stderr == "" {
		return cmd + ": " + e.Err.Error()
	}
	return cmd + ": " + e.Err.Error() + ": " + e.Stderr
}

func (e *GHError) Unwrap() error { return e.Err }

// notFoundLine returns what is missing according to stderr, gh's answer
// recognized by isNotFound: no such owner, repository (which may also be
// one the account cannot see) or pull request; else stderr (gh's "no pull
// requests found for branch ...").
func notFoundLine(stderr string) string {
	switch {
	case strings.Contains(stderr, "Could not resolve to a RepositoryOwner"),
		strings.Contains(stderr, "was not recognized as either a GitHub user or an organization"):
		return "no such user or organization on GitHub"
	case strings.Contains(stderr, "Could not resolve to a Repository with"):
		return "no such repository on GitHub (or not visible to your gh account)"
	case strings.Contains(stderr, "Could not resolve to a PullRequest"):
		return "no such pull request on GitHub"
	}
	return stderr
}

// failure returns the error of the gh command args whose run ended with
// err (nil on success: failure then returns nil) and printed stderr.
func (g *GH) failure(ctx context.Context, args []string, err error, stderr string) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("gh %s: %w", strings.Join(args, " "), cerr)
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) && !isChdirError(err) {
		return fmt.Errorf("%w (%s): install the GitHub CLI from https://cli.github.com/ and run `gh auth login`: %w",
			ErrGHNotFound, g.opts.Path, err)
	}
	msg := strings.TrimSpace(stderr)
	wrapped := err
	if isNotFound(msg) {
		wrapped = fmt.Errorf("%w: %w", ErrNotFound, err)
	} else if strings.Contains(msg, badCredentialsMarker) {
		wrapped = fmt.Errorf("%w: %w", ErrGHCredentialsRejected, err)
	} else if isUnauthenticated(err) {
		wrapped = fmt.Errorf("%w: %w", ErrGHNotAuthenticated, err)
	}
	return &GHError{Args: args, Stderr: msg, Err: wrapped}
}

// runBrowser runs gh like run for a command that starts a web browser.
// gh's browser launcher passes gh's standard output and error on to the
// browser (or to xdg-open, $BROWSER...), which may outlive gh: with pipes,
// the wait for their end would last until the browser exits (WaitDelay
// then reports a failure). Standard output goes to the null device and
// standard error to a temporary file read once gh has exited, so the
// wait ends with gh. The line in which gh announces the page ("Opening
// ... in your browser.") is left out of an error.
func (g *GH) runBrowser(ctx context.Context, args ...string) error {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	errf, err := os.CreateTemp("", "sheaf-gh-*.stderr")
	if err != nil {
		return err
	}
	defer os.Remove(errf.Name())
	defer errf.Close()
	cmd := g.command(ctx, "", args)
	cmd.Stdout, cmd.Stderr = null, errf
	runErr := cmd.Run()
	stderr, err := os.ReadFile(errf.Name())
	if err != nil && runErr == nil {
		return err
	}
	var kept []string
	for _, l := range strings.Split(string(stderr), "\n") {
		if !strings.HasPrefix(l, "Opening ") || !strings.HasSuffix(strings.TrimSpace(l), " in your browser.") {
			kept = append(kept, l)
		}
	}
	return g.failure(ctx, args, runErr, strings.Join(kept, "\n"))
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
// view <n> --repo <owner>/<name> --web` for pull request n, `gh pr view
// <head> --repo <owner>/<name> --web` for the pull request of a head (gh
// matches "<branch>" with the head of a pull request from a branch of the
// repository and "<owner>:<branch>" with one from a fork: HeadLabel in
// api/queries_pr.go) and `gh browse
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
	case page.Head != "":
		args = []string{"pr", "view", page.Head, "--repo", slug, "--web"}
	case page.Branch != "":
		args = []string{"browse", "--repo", slug, "--branch", page.Branch}
	}
	return g.runBrowser(ctx, args...)
}

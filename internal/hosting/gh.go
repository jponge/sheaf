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
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultLimit is the default maximum number of repositories requested from
// `gh repo list`. gh's own default is 30, which would silently truncate.
const DefaultLimit = 1000

var (
	// ErrGHNotFound means the gh executable could not be located.
	ErrGHNotFound = errors.New("gh executable not found")
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

func (g *GH) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.opts.Path, args...)
	cmd.Env = childEnv()
	// Bound the wait for orphaned grandchildren holding the output pipes after a kill.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), cerr)
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w (%s): install the GitHub CLI from https://cli.github.com/ and run `gh auth login`: %w",
				ErrGHNotFound, g.opts.Path, err)
		}
		msg := strings.TrimSpace(stderr.String())
		wrapped := err
		if isNotFound(msg) {
			wrapped = fmt.Errorf("%w: %w", ErrNotFound, err)
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
	if err := validateOwner(owner); err != nil {
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
	if err := validateRepo(owner, name); err != nil {
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
	if err := validatePR(owner, name, number); err != nil {
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

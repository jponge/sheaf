package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
)

// CheckLevel is the severity of a doctor Check.
type CheckLevel int

const (
	CheckOK   CheckLevel = iota + 1 // all good
	CheckInfo                       // worth knowing, nothing to fix
	CheckWarn                       // should be fixed; sheaf still works
	CheckFail                       // sheaf cannot work properly
)

func (l CheckLevel) String() string {
	switch l {
	case CheckOK:
		return "ok"
	case CheckInfo:
		return "info"
	case CheckWarn:
		return "warn"
	case CheckFail:
		return "fail"
	}
	return fmt.Sprintf("CheckLevel(%d)", int(l))
}

// MarshalText encodes the level as "ok", "info", "warn" or "fail".
func (l CheckLevel) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// Check is one doctor finding. Check names: git, gh, bundle, repo-dir, host,
// primary, origin-url, upstream-url, tracking, worktree, lock, adoptable,
// unknown-dir.
type Check struct {
	Level   CheckLevel `json:"level"`
	Check   string     `json:"check"`
	Repo    string     `json:"repo,omitempty"`
	Message string     `json:"message"`
	Hint    string     `json:"hint,omitempty"`
}

// DoctorOptions configures Doctor.
type DoctorOptions struct {
	// Root is the bundle root; empty when no bundle was found (RootErr then
	// says why).
	Root    string
	RootErr error
	// GitVersion returns the installed git version; nil means
	// gitx.CheckVersion.
	GitVersion func(context.Context) (gitx.GitVersion, error)
	// GH checks that gh is installed (an error matching
	// hosting.ErrGHNotFound otherwise) and authenticated; nil skips the
	// check.
	GH func(context.Context) error
	// StaleLockAge is the age from which a lock file is reported as stale;
	// zero means DefaultStaleLockAge.
	StaleLockAge time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// ErrGHProbeTimeout is wrapped by a DoctorOptions.GH probe when gh did not
// answer in time (not an authentication failure).
var ErrGHProbeTimeout = errors.New("gh did not answer")

// DefaultStaleLockAge is the default DoctorOptions.StaleLockAge.
const DefaultStaleLockAge = 10 * time.Minute

// Doctor checks the environment (git version, gh) and the bundle: sheaf.yaml
// is valid, and for each repository its directory exists, its primary
// checkout is <repo>/<normalized default branch> and a work tree of its own,
// origin matches the URL of sheaf.yaml (for a repository with a fork: origin
// matches the fork's URL, upstream the repository's, and the local default
// branch tracks upstream/<default branch>), its worktrees are present and inside
// the repository directory, and no lock file is left in its git directory.
// Directories of the bundle root that are not repositories of the bundle are
// reported, with `sheaf adopt` for those holding a checkout. It only reads:
// nothing is fetched or changed. Checks are returned in a stable order:
// environment, bundle, repositories by name, unregistered directories.
func Doctor(ctx context.Context, opts DoctorOptions) []Check {
	var cs []Check
	add := func(l CheckLevel, check, repo, msg, hint string) {
		cs = append(cs, Check{Level: l, Check: check, Repo: repo, Message: msg, Hint: hint})
	}
	gv := opts.GitVersion
	if gv == nil {
		gv = gitx.CheckVersion
	}
	switch v, err := gv(ctx); {
	case errors.Is(err, gitx.ErrGitTooOld) || err == nil && !v.AtLeast(gitx.MinGitVersion):
		add(CheckFail, "git", "", fmt.Sprintf("git %v is too old: sheaf needs %v or later", v, gitx.MinGitVersion), "upgrade git")
	case err != nil:
		add(CheckFail, "git", "", "git is not usable: "+err.Error(), "install git and make sure it is on PATH")
	default:
		add(CheckOK, "git", "", fmt.Sprintf("git %v (%v or later required)", v, gitx.MinGitVersion), "")
	}
	if opts.GH != nil {
		switch err := opts.GH(ctx); {
		case err == nil:
			add(CheckOK, "gh", "", "gh is installed and authenticated", "")
		case errors.Is(err, hosting.ErrGHNotFound):
			add(CheckWarn, "gh", "", "gh not found: add, sync and wt pr need it for GitHub repositories",
				"install the GitHub CLI from https://cli.github.com/ and run `gh auth login`")
		case errors.Is(err, ErrGHProbeTimeout):
			add(CheckWarn, "gh", "", firstLineOf(err.Error()), "check gh and the network, then run doctor again")
		default:
			add(CheckWarn, "gh", "", "gh is not authenticated: "+firstLineOf(err.Error()), "run `gh auth login`")
		}
	}
	if opts.Root == "" {
		msg := "no bundle found"
		if opts.RootErr != nil {
			msg = opts.RootErr.Error()
		}
		add(CheckFail, "bundle", "", msg, "run sheaf in a bundle, pass --bundle, or create one with `sheaf create` or `sheaf init`")
		return cs
	}
	b, err := bundle.Load(opts.Root)
	if err != nil {
		add(CheckFail, "bundle", "", fmt.Sprintf("%s is invalid: %v", filepath.Join(opts.Root, bundle.FileName), err), "fix the file")
		return cs
	}
	add(CheckOK, "bundle", "", fmt.Sprintf("bundle %s with %d repositories", opts.Root, len(b.Repositories)), "")
	repos := slices.Clone(b.Repositories)
	slices.SortFunc(repos, func(x, y bundle.Repository) int { return strings.Compare(x.Name, y.Name) })
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	age := opts.StaleLockAge
	if age <= 0 {
		age = DefaultStaleLockAge
	}
	for _, r := range repos {
		if ctx.Err() != nil {
			add(CheckFail, "bundle", "", "interrupted", "")
			return cs
		}
		cs = append(cs, doctorRepo(ctx, opts.Root, r, now(), age)...)
	}
	cs = append(cs, doctorUnregistered(ctx, opts.Root, b)...)
	return cs
}

func doctorRepo(ctx context.Context, root string, r bundle.Repository, now time.Time, staleAge time.Duration) []Check {
	var cs []Check
	add := func(l CheckLevel, check, msg, hint string) {
		cs = append(cs, Check{Level: l, Check: check, Repo: r.Name, Message: msg, Hint: hint})
	}
	repoDir := bundle.RepoDir(root, r.Name)
	if fi, err := os.Stat(repoDir); err != nil || !fi.IsDir() {
		msg := fmt.Sprintf("directory %s is missing", repoDir)
		if err == nil {
			msg = fmt.Sprintf("%s is not a directory", repoDir)
		} else if !errors.Is(err, os.ErrNotExist) {
			msg = err.Error()
		}
		add(CheckFail, "repo-dir", msg, fmt.Sprintf("restore it, or drop the repository with `sheaf remove %s`", r.Name))
		return cs
	}
	if !r.IsGitHub() {
		add(CheckInfo, "host", fmt.Sprintf("hosted on %s: GitHub features (sync, wt pr) do not apply", r.HostName()), "")
	}
	primary, common, prob := coCheckPrimary(ctx, root, r)
	if prob != nil {
		hint := ""
		if found, _ := primaries(ctx, repoDir); len(found) > 0 && !slices.Contains(found, primary) {
			hint = fmt.Sprintf("found a checkout at %s: sheaf expects the primary checkout at %s (named after the default branch %s); rename it", strings.Join(found, ", "), primary, r.DefaultBranch)
		}
		add(CheckFail, "primary", fmt.Sprintf("%s: %v", prob.Path, prob.Err), hint)
		return cs
	}
	switch br, err := gitx.CurrentBranch(ctx, primary); {
	case err != nil && !errors.Is(err, gitx.ErrDetachedHead):
		add(CheckWarn, "primary", fmt.Sprintf("%s: cannot read the current branch: %v", primary, err), "")
	case err != nil:
		add(CheckInfo, "primary", fmt.Sprintf("primary checkout %s has a detached HEAD (default branch %s)", primary, r.DefaultBranch), "")
	case br != r.DefaultBranch:
		add(CheckInfo, "primary", fmt.Sprintf("primary checkout %s is on %s, not on the default branch %s", primary, br, r.DefaultBranch), "")
	default:
		add(CheckOK, "primary", fmt.Sprintf("primary checkout %s on %s", primary, br), "")
	}
	// The configured URL, not `git remote get-url`, which applies
	// url.<base>.insteadOf rewrites: sheaf.yaml records the URL as cloned.
	checkRemote := func(remote, want, what, field string) {
		u, err := gitx.Run(ctx, primary, "config", "--get", "remote."+remote+".url")
		u = strings.TrimSpace(u)
		switch {
		case err != nil:
			add(CheckWarn, remote+"-url", "no "+remote+" remote"+what+": "+firstLineOf(err.Error()), fmt.Sprintf("git -C %s remote add %s %s", primary, remote, want))
		case u != want:
			add(CheckWarn, remote+"-url", fmt.Sprintf("%s%s is %s but %s says %s", remote, what, u, bundle.FileName, want),
				fmt.Sprintf("fix %s with `git -C %s remote set-url %s %s`, or the %s in %s", remote, primary, remote, want, field, bundle.FileName))
		}
	}
	rs := RemotesOf(r)
	if !rs.Forked() {
		checkRemote(rs.Reference, r.URL, "", "url")
	} else {
		checkRemote(rs.Fork, r.Fork.URL, " (your fork)", "fork url")
		checkRemote(rs.Reference, r.URL, " (the repository)", "url")
		checkDefaultTracking(ctx, primary, r, rs, func(msg, hint string) { add(CheckWarn, "tracking", msg, hint) })
	}
	if infos, _, _, err := listRepo(ctx, root, r); err != nil {
		add(CheckWarn, "worktree", "cannot list worktrees: "+err.Error(), "")
	} else {
		for _, in := range infos {
			switch {
			case in.Primary:
			case in.Prunable:
				add(CheckWarn, "worktree", fmt.Sprintf("worktree %s [%s] is missing (%s)", in.Path, branchOrDetached(in), in.PrunableReason),
					fmt.Sprintf("git -C %s worktree prune", primary))
			case in.Outside:
				add(CheckWarn, "worktree", fmt.Sprintf("worktree %s [%s] is outside %s", in.Path, branchOrDetached(in), repoDir),
					fmt.Sprintf("move it next to the primary checkout with `git -C %s worktree move %s <%s/dir>`", primary, in.Path, repoDir))
			case in.Locked:
				msg := fmt.Sprintf("worktree %s is locked", in.Path)
				if in.LockReason != "" {
					msg += " (" + in.LockReason + ")"
				}
				add(CheckInfo, "worktree", msg, "")
			}
		}
	}
	locks, err := gitx.LockFiles(common)
	if err != nil {
		add(CheckWarn, "lock", "cannot look for lock files: "+err.Error(), "")
	}
	for _, l := range locks {
		a := now.Sub(l.ModTime).Round(time.Second)
		if a >= staleAge {
			add(CheckWarn, "lock", fmt.Sprintf("stale lock file %s (%v old)", l.Path, a),
				"if no git process is running in this repository, delete it")
		} else {
			add(CheckInfo, "lock", fmt.Sprintf("lock file %s (%v old): git may be running", l.Path, a), "")
		}
	}
	return cs
}

// checkDefaultTracking reports (warn) a local default branch of a forked
// repository that does not track <reference>/<default>: update would then
// follow the fork instead of the repository.
func checkDefaultTracking(ctx context.Context, primary string, r bundle.Repository, rs gitx.Remotes, warn func(msg, hint string)) {
	want := rs.Reference + "/" + r.DefaultBranch
	out, err := gitx.Run(ctx, primary, "for-each-ref", "--format=%(upstream:short)", "refs/heads/"+r.DefaultBranch)
	if err != nil || !branchExistsIn(ctx, primary, r.DefaultBranch) {
		return // no local default branch: nothing tracks anything
	}
	switch up := strings.TrimSpace(out); up {
	case want:
	case "":
		warn(fmt.Sprintf("%s has no upstream; it should track %s", r.DefaultBranch, want),
			fmt.Sprintf("git -C %s branch --set-upstream-to=%s %s", primary, want, r.DefaultBranch))
	default:
		warn(fmt.Sprintf("%s tracks %s, not %s: update follows your fork instead of the repository", r.DefaultBranch, up, want),
			fmt.Sprintf("git -C %s branch --set-upstream-to=%s %s", primary, want, r.DefaultBranch))
	}
}

func branchExistsIn(ctx context.Context, dir, branch string) bool {
	ok, err := refExists(ctx, dir, "refs/heads/"+branch)
	return err == nil && ok
}

func branchOrDetached(in WorktreeInfo) string {
	if in.Branch == "" && in.Tag != "" {
		return "tag " + in.Tag
	}
	if in.Branch == "" {
		return "detached"
	}
	return in.Branch
}

// doctorUnregistered reports the non-hidden directories of the bundle root
// that are not repositories of the bundle.
func doctorUnregistered(ctx context.Context, root string, b *bundle.Bundle) []Check {
	var cs []Check
	cands, err := Candidates(ctx, root, b)
	if err != nil {
		return []Check{{Level: CheckWarn, Check: "unknown-dir", Message: "cannot list " + root + ": " + err.Error()}}
	}
	adoptable := map[string]bool{}
	for _, c := range cands {
		adoptable[c.Name] = true
		cs = append(cs, Check{Level: CheckWarn, Check: "adoptable", Message: fmt.Sprintf("%s holds a checkout (%s) that is not in %s", c.Path, c.Kind, bundle.FileName),
			Hint: "sheaf adopt " + c.Name})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return append(cs, Check{Level: CheckWarn, Check: "unknown-dir", Message: "cannot list " + root + ": " + err.Error()})
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || strings.HasPrefix(n, ".") || adoptable[n] || registered(b, n) {
			continue
		}
		cs = append(cs, Check{Level: CheckInfo, Check: "unknown-dir", Message: fmt.Sprintf("%s is not a repository of the bundle", filepath.Join(root, n))})
	}
	return cs
}

// DoctorFailed reports whether a check failed.
func DoctorFailed(checks []Check) bool {
	return slices.ContainsFunc(checks, func(c Check) bool { return c.Level == CheckFail })
}

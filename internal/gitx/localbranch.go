package gitx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// LocalBranch describes a local branch and its upstream configuration.
type LocalBranch struct {
	Name string // short name, e.g. "feat/x"
	Head string // object name of the tip
	// Remote and Merge are branch.<name>.remote and branch.<name>.merge, as
	// configured (empty when unset).
	Remote, Merge string
	// Upstream is the full name of the ref the upstream maps to, e.g.
	// "refs/remotes/origin/feat/x" (or "refs/heads/x" for a local upstream),
	// as given by %(upstream). It is empty when no upstream is configured
	// and when the configured merge ref is not mapped by a fetch refspec of
	// the remote (e.g. refs/pull/<n>/head): git then has no remote-tracking
	// ref for it.
	Upstream string
	// Gone is true when Upstream is set but its ref does not exist (the
	// remote branch was deleted and pruned): "[gone]" in %(upstream:track).
	Gone bool
}

// PullRequest returns the number of the pull request whose head the branch
// tracks, when branch.<name>.merge is refs/pull/<n>/head or
// refs/pull/<n>/merge (as set up by `sheaf wt pr` for pull requests from
// forks).
func (b LocalBranch) PullRequest() (int, bool) {
	return PullRequestRef(b.Merge)
}

// PullRequestRef parses refs/pull/<n>/head and refs/pull/<n>/merge (n >= 1).
func PullRequestRef(ref string) (int, bool) {
	rest, ok := strings.CutPrefix(ref, "refs/pull/")
	if !ok {
		return 0, false
	}
	num, kind, ok := strings.Cut(rest, "/")
	if !ok || kind != "head" && kind != "merge" || num == "" || num[0] < '1' || num[0] > '9' {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, false
	}
	return n, true
}

// PullHeadRef is the ref where sheaf records the head of pull request n of
// a repository whenever it fetches refs/pull/<n>/head from it:
// refs/sheaf/pull/<n>. The commits reachable from it are published (see
// DeletionRisksOptions.PublishedRefs) as of that fetch.
func PullHeadRef(n int) string { return "refs/sheaf/pull/" + strconv.Itoa(n) }

// PullRequestBranches returns, sorted and without duplicates, the numbers of
// the pull requests whose head a local branch of the repository containing
// dir tracks from remote (branch.<name>.remote = remote and
// branch.<name>.merge = refs/pull/<n>/head, as `sheaf wt pr` sets up).
func PullRequestBranches(ctx context.Context, dir, remote string) ([]int, error) {
	cfg, err := branchConfig(ctx, dir)
	if err != nil {
		return nil, err
	}
	var ns []int
	for _, c := range cfg {
		if n, ok := PullRequestRef(c.merge); ok && c.remote == remote && c.merge == "refs/pull/"+strconv.Itoa(n)+"/head" && !slices.Contains(ns, n) {
			ns = append(ns, n)
		}
	}
	slices.Sort(ns)
	return ns, nil
}

// LocalBranches returns the local branches of the repository containing dir,
// sorted by name, with their upstream configuration.
func LocalBranches(ctx context.Context, dir string) ([]LocalBranch, error) {
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(upstream)%00%(upstream:track)", "refs/heads")
	if err != nil {
		return nil, err
	}
	cfg, err := branchConfig(ctx, dir)
	if err != nil {
		return nil, err
	}
	var bs []LocalBranch
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x00")
		if len(f) != 4 {
			return nil, fmt.Errorf("gitx: unexpected for-each-ref output %q", line)
		}
		name := strings.TrimPrefix(f[0], "refs/heads/")
		c := cfg[name]
		bs = append(bs, LocalBranch{Name: name, Head: f[1], Remote: c.remote, Merge: c.merge, Upstream: f[2], Gone: f[2] != "" && f[3] == "[gone]"})
	}
	return bs, nil
}

type branchCfg struct{ remote, merge string }

// branchConfig reads branch.<name>.remote and branch.<name>.merge for every
// branch. Branch names may contain dots: the variable name is the part
// between "branch." and the last dot.
func branchConfig(ctx context.Context, dir string) (map[string]branchCfg, error) {
	out, err := Run(ctx, dir, "config", "-z", "--get-regexp", `^branch\..*\.(remote|merge)$`)
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return map[string]branchCfg{}, nil // no such variable
		}
		return nil, err
	}
	m := map[string]branchCfg{}
	for entry := range strings.SplitSeq(out, "\x00") {
		key, val, _ := strings.Cut(entry, "\n")
		rest, ok := strings.CutPrefix(key, "branch.")
		if !ok {
			continue
		}
		name, v, found := strings.CutLast(rest, ".")
		if !found {
			continue
		}
		c := m[name]
		switch v {
		case "remote":
			c.remote = val
		case "merge":
			c.merge = val
		}
		m[name] = c
	}
	return m, nil
}

// FetchRefspecs runs `git fetch --quiet <remote> <refspecs...>` in dir, for
// fetches of explicit refs (e.g. refs/pull/<n>/head). It does not prune, and
// a refspec without a leading '+' never rewrites its destination other than
// by a fast-forward. Like FetchRemote, it keeps the caller's terminal under
// WithInteractive and must be serialized with other fetches into the same
// repository when it updates refs; a refspec without destination only writes
// FETCH_HEAD, which is per worktree.
func FetchRefspecs(ctx context.Context, dir, remote string, refspecs ...string) error {
	if len(refspecs) == 0 {
		return errors.New("gitx: fetch: no refspec")
	}
	_, err := run(ctx, netOpts(ctx), dir, append([]string{"fetch", "--quiet", "--no-tags", "--end-of-options", remote}, refspecs...)...)
	return err
}

// LockFile is a *.lock file found in a git directory.
type LockFile struct {
	Path    string
	ModTime time.Time
}

// rootLocks are the lock files git creates directly in a git directory.
var rootLocks = []string{"index.lock", "HEAD.lock", "ORIG_HEAD.lock", "FETCH_HEAD.lock", "config.lock", "packed-refs.lock", "shallow.lock"}

// LockFiles returns the lock files git may leave behind in the common git
// directory commonDir and in its per-worktree directories
// (worktrees/<id>/): index.lock, HEAD.lock, ORIG_HEAD.lock, FETCH_HEAD.lock,
// config.lock, packed-refs.lock, shallow.lock, and every *.lock under refs/
// and logs/. A lock is held while a git process runs and is left behind when
// one is killed (SIGKILL, crash); see StaleLockPath. The result is sorted by
// path. Symbolic links are not followed.
func LockFiles(commonDir string) ([]LockFile, error) {
	if _, err := os.Stat(commonDir); err != nil {
		return nil, err
	}
	dirs := []string{commonDir}
	wts, err := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range wts {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(commonDir, "worktrees", e.Name()))
		}
	}
	var out []LockFile
	add := func(p string, fi fs.FileInfo) {
		if fi.Mode().IsRegular() {
			out = append(out, LockFile{Path: p, ModTime: fi.ModTime()})
		}
	}
	for _, d := range dirs {
		for _, n := range rootLocks {
			p := filepath.Join(d, n)
			if fi, err := os.Lstat(p); err == nil {
				add(p, fi)
			}
		}
		for _, sub := range []string{"refs", "logs"} {
			err := filepath.WalkDir(filepath.Join(d, sub), func(p string, e fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
					fi, err := e.Info()
					if err != nil {
						if errors.Is(err, fs.ErrNotExist) {
							return nil // released meanwhile
						}
						return err
					}
					add(p, fi)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(out, func(a, b LockFile) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// BranchUse tells why a branch is in use, and where.
type BranchUse struct {
	// Path is the work tree (as recorded by git; it may be missing).
	Path string
	// How is "checked out", "rebasing" or "bisecting".
	How string
}

// BranchesInUse returns the local branches of the repository containing dir
// that must not be deleted or moved: checked out in any of its work trees
// (as listed by `git worktree list`, which also works with the reftable
// backend, whose HEAD files hold a placeholder), or being rebased
// (rebase-merge/head-name, rebase-apply/head-name) or bisected
// (BISECT_START) there while HEAD is detached, read from the git directory
// of every work tree, including those whose directory is missing. Checked-out
// uses come first, in `git worktree list` order.
func BranchesInUse(ctx context.Context, dir string) (map[string][]BranchUse, error) {
	common, err := CommonDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	wts, err := WorktreeList(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]BranchUse{}
	add := func(branch string, u BranchUse) {
		if branch != "" {
			out[branch] = append(out[branch], u)
		}
	}
	for _, wt := range wts {
		if !wt.Bare && wt.Branch != "" {
			add(wt.Branch, BranchUse{Path: filepath.Clean(wt.Path), How: "checked out"})
		}
	}
	type gd struct{ gitDir, path string }
	var dirs []gd
	if len(wts) > 0 && !wts[0].Bare {
		dirs = append(dirs, gd{common, filepath.Clean(wts[0].Path)})
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		g := filepath.Join(common, "worktrees", e.Name())
		dirs = append(dirs, gd{g, worktreePathOf(g)})
	}
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	for _, d := range dirs {
		for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
			if ref, ok := strings.CutPrefix(read(filepath.Join(d.gitDir, f)), "refs/heads/"); ok {
				add(ref, BranchUse{Path: d.path, How: "rebasing"})
			}
		}
		// BISECT_START holds the branch (short name) or commit bisect
		// started from.
		if start := read(filepath.Join(d.gitDir, "BISECT_START")); start != "" {
			if ok, err := refExists(ctx, dir, "refs/heads/"+start); err != nil {
				return nil, err
			} else if ok {
				add(start, BranchUse{Path: d.path, How: "bisecting"})
			}
		}
	}
	return out, nil
}

// worktreePathOf returns the work tree of the per-worktree git directory g
// (<common>/worktrees/<id>), from its gitdir file, which holds the path of
// <work tree>/.git: absolute, or relative to g (worktree.useRelativePaths,
// git 2.48+). It returns "" when the file cannot be read.
func worktreePathOf(g string) string {
	b, err := os.ReadFile(filepath.Join(g, "gitdir"))
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(b))
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g, p)
	}
	return filepath.Dir(filepath.Clean(p))
}

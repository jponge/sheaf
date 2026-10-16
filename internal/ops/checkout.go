package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// Phases of the checkout commands (status, fetch, update, run).
const (
	PhaseStatus = "status"
	PhaseUpdate = "update"
	PhaseRun    = "run"
	// PhaseWaitFastForward is reported (KindProgress, with Repo and
	// Worktree) for each fast-forward still running when the context of
	// Update is cancelled: it runs to completion unless forced (see
	// UpdateOptions.Force).
	PhaseWaitFastForward = "wait-fast-forward"
)

// Checkout is one work tree of a bundle repository: its primary checkout or
// one of its linked worktrees, as listed by git.
type Checkout struct {
	Repo string // repository name in sheaf.yaml
	Dir  string // base name of Path, e.g. "main" or "feat-x"
	Path string // absolute path
	// Primary is true for the primary checkout <repo>/<default branch>/.
	Primary  bool
	Branch   string // checked-out branch, empty when Detached
	Detached bool
	// Outside is true for a linked worktree that is not a sibling of the
	// primary (not directly in <bundle>/<repo>/); it is processed anyway.
	Outside bool
	// Tag is the tag a worktree created by `wt add --tag` is pinned to
	// (Detached is then true); see WorktreeInfo.Tag.
	Tag string
	// TagMovedTo is HEAD when a worktree pinned at Tag is no longer at the
	// commit it was created at (see gitx.Worktree.TagMoved); empty otherwise.
	TagMovedTo string
}

// Name returns "<repo>/<dir>", the way checkouts are shown to users.
func (c Checkout) Name() string { return c.Repo + "/" + c.Dir }

// Problem is a checkout that cannot be processed, typically because its
// directory is missing; it is reported instead of failing the whole command.
type Problem struct {
	Repo string
	Path string
	Err  error
}

func (p Problem) Error() string { return fmt.Sprintf("%s: %s: %v", p.Repo, p.Path, p.Err) }

func (p Problem) Unwrap() error { return p.Err }

// CheckoutFilter selects checkouts.
type CheckoutFilter struct {
	// Repos holds repository names or path.Match patterns (e.g. "smallrye-*");
	// empty selects every repository. A pattern that is malformed or matches
	// no repository is an error, so typos do not silently select nothing.
	Repos []string
	// PrimaryOnly drops the linked worktrees.
	PrimaryOnly bool
	// Paths, when not empty, keeps only the checkouts (and problems) whose
	// path is listed, as reported by ListCheckouts. A path matching neither
	// a checkout nor a problem of the selected repositories is an error.
	Paths []string
}

// coRepoCheckouts are the checkouts of one repository whose primary exists.
type coRepoCheckouts struct {
	name      string
	remotes   gitx.Remotes
	primary   string // path of the primary checkout
	checkouts []Checkout
}

// coSelectRepos returns the repositories of b selected by patterns, sorted by
// name.
func coSelectRepos(b *bundle.Bundle, patterns []string) ([]bundle.Repository, error) {
	repos := slices.Clone(b.Repositories)
	slices.SortFunc(repos, func(x, y bundle.Repository) int { return strings.Compare(x.Name, y.Name) })
	if len(patterns) == 0 {
		return repos, nil
	}
	sel := map[string]bool{}
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("invalid repository pattern %q: %w", p, err)
		}
		n := 0
		for _, r := range repos {
			if ok, _ := path.Match(p, r.Name); ok {
				sel[r.Name] = true
				n++
			}
		}
		switch {
		case n == 0 && !strings.ContainsAny(p, `*?[\`):
			return nil, repoNotFound(b, p) // a plain name
		case n == 0:
			return nil, fmt.Errorf("%q matches no repository of the bundle", p)
		}
	}
	return slices.DeleteFunc(repos, func(r bundle.Repository) bool { return !sel[r.Name] }), nil
}

// coPrimaryPath returns <root>/<repo>/<normalized default branch>.
func coPrimaryPath(root string, r bundle.Repository) (string, error) {
	return bundle.WorktreeDir(root, r.Name, r.DefaultBranch)
}

// coCheckPrimary is the check of a repository's layout shared by every
// command: it returns a problem unless the repository directory
// <root>/<repo> and the primary checkout in it are real directories (not
// symbolic links, resolving to themselves), and the primary checkout is the
// main work tree of its own repository (see coVerifyPrimary) with a .git
// directory. With these, git lists the primary checkout as the main work
// tree, which enumerateRepo also checks. It also returns the repository's
// common git directory.
func coCheckPrimary(ctx context.Context, root string, r bundle.Repository) (string, string, *Problem) {
	p, err := coPrimaryPath(root, r)
	if err != nil {
		return "", "", &Problem{Repo: r.Name, Path: filepath.Join(root, r.Name), Err: err}
	}
	if err := coCheckRealDir(filepath.Dir(p), "repository directory"); err != nil {
		return p, "", &Problem{Repo: r.Name, Path: filepath.Dir(p), Err: err}
	}
	if err := coCheckRealDir(p, "primary checkout"); err != nil {
		return p, "", &Problem{Repo: r.Name, Path: p, Err: err}
	}
	common, err := coVerifyPrimary(ctx, p)
	if err != nil {
		return p, "", &Problem{Repo: r.Name, Path: p, Err: err}
	}
	if fi, err := os.Lstat(filepath.Join(p, ".git")); err != nil || !fi.IsDir() {
		return p, "", &Problem{Repo: r.Name, Path: p, Err: errors.New("primary checkout's .git is not a directory")}
	}
	return p, common, nil
}

// coCheckRealDir returns an error, naming dir as what, unless dir is a
// directory, not a symbolic link, that resolves to itself.
func coCheckRealDir(dir, what string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New(what + " missing")
	case err != nil:
		return err
	}
	switch fault, r, err := realDirFault(dir, fi); fault {
	case dirSymlink:
		return errors.New(what + " is a symbolic link")
	case dirNotDir:
		return errors.New(what + " is not a directory")
	case dirNotSelf:
		return fmt.Errorf("%s does not resolve to itself (%s, %v)", what, r, err)
	}
	return nil
}

// dirFault is why an existing path is not a real directory (realDirFault).
type dirFault int

const (
	dirReal    dirFault = iota // a directory, not a symbolic link, resolving to itself
	dirSymlink                 // a symbolic link
	dirNotDir                  // not a directory
	dirNotSelf                 // a directory that does not resolve to itself
)

// realDirFault tells whether dir, whose os.Lstat information is fi, is a
// directory, not a symbolic link, that resolves to itself. For dirNotSelf it
// also returns what dir resolves to and the resolution error.
func realDirFault(dir string, fi fs.FileInfo) (fault dirFault, resolved string, err error) {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return dirSymlink, "", nil
	case !fi.IsDir():
		return dirNotDir, "", nil
	}
	if r, err := filepath.EvalSymlinks(dir); err != nil || r != dir {
		return dirNotSelf, r, err
	}
	return dirReal, "", nil
}

// refuseRemovalUnlessRealDir returns a "refusing to remove" error unless dir,
// whose os.Lstat information is fi, is a directory, not a symbolic link, that
// resolves to itself.
func refuseRemovalUnlessRealDir(dir string, fi fs.FileInfo) error {
	switch fault, r, err := realDirFault(dir, fi); fault {
	case dirSymlink:
		return fmt.Errorf("refusing to remove %s: it is a symbolic link", dir)
	case dirNotDir:
		return fmt.Errorf("refusing to remove %s: not a directory", dir)
	case dirNotSelf:
		return fmt.Errorf("refusing to remove %s: it does not resolve to itself (%s, %v)", dir, r, err)
	}
	return nil
}

// coSameFile reports whether a and b resolve to the same path.
func coSameFile(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// coWorkTree is what coInspect learns about a checkout directory.
type coWorkTree struct {
	top, gitDir, commonDir string
}

// coInspect checks that dir is the top level of a git work tree of its own
// and returns its git directories, with a single git invocation. git run in
// a directory without .git walks up to an enclosing repository, which must
// never be mistaken for the checkout: dir/.git must exist and the top level
// must resolve to dir, otherwise the error is "not a git work tree (inside
// repository X)".
func coInspect(ctx context.Context, dir string) (coWorkTree, error) {
	_, gitErr := os.Lstat(filepath.Join(dir, ".git"))
	out, err := gitx.Run(ctx, dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		if ctx.Err() != nil {
			return coWorkTree{}, ctx.Err()
		}
		return coWorkTree{}, fmt.Errorf("not a git work tree: %w", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		return coWorkTree{}, fmt.Errorf("unexpected git rev-parse output %q", out)
	}
	wt := coWorkTree{top: lines[0], gitDir: lines[1], commonDir: lines[2]}
	if gitErr != nil || !coSameFile(wt.top, dir) {
		return coWorkTree{}, fmt.Errorf("not a git work tree (inside repository %s)", wt.top)
	}
	return wt, nil
}

// coVerifyPrimary checks that dir is a work tree (see coInspect) and the
// main work tree of its repository, not a linked worktree. It returns the
// repository's common git directory.
func coVerifyPrimary(ctx context.Context, dir string) (string, error) {
	wt, err := coInspect(ctx, dir)
	if err != nil {
		return "", err
	}
	if !coSameFile(wt.gitDir, wt.commonDir) {
		return "", errors.New("primary is a linked worktree of another checkout (" + wt.commonDir + ")")
	}
	return wt.commonDir, nil
}

// coListRepo lists the checkouts of r in the order of enumerateRepo; linked
// worktrees that cannot be used are returned as problems.
func coListRepo(ctx context.Context, root string, r bundle.Repository, primaryOnly bool) (*coRepoCheckouts, []Problem) {
	primary, common, prob := coCheckPrimary(ctx, root, r)
	if prob != nil {
		return nil, []Problem{*prob}
	}
	entries, err := enumerateRepo(ctx, primary, common, enumPolicy{primaryOnly: primaryOnly})
	if err != nil {
		return nil, []Problem{{Repo: r.Name, Path: primary, Err: err}}
	}
	rc := &coRepoCheckouts{name: r.Name, primary: primary, remotes: RemotesOf(r)}
	rc.checkouts = append(rc.checkouts, Checkout{Repo: r.Name, Dir: filepath.Base(primary), Path: primary, Primary: true,
		Branch: entries[0].Branch, Detached: entries[0].Detached})
	var probs []Problem
	for _, wt := range entries[1:] {
		if wt.Problem != nil {
			probs = append(probs, Problem{Repo: r.Name, Path: wt.Path, Err: wt.Problem})
			continue
		}
		rc.checkouts = append(rc.checkouts, Checkout{Repo: r.Name, Dir: filepath.Base(wt.Path), Path: wt.Path, Branch: wt.Branch, Detached: wt.Detached,
			Outside: wt.Outside, Tag: wt.Tag, TagMovedTo: tagMovedTo(wt.Worktree)})
	}
	return rc, probs
}

// coEnumerate lists the selected checkouts of w per repository (repositories
// sorted by name), listing up to jobs repositories in parallel.
func coEnumerate(ctx context.Context, w *Workspace, f CheckoutFilter, jobs int) ([]*coRepoCheckouts, []Problem, error) {
	repos, err := coSelectRepos(w.Bundle(), f.Repos)
	if err != nil {
		return nil, nil, err
	}
	sets := make([]*coRepoCheckouts, len(repos))
	probs := make([][]Problem, len(repos))
	coForEach(jobs, len(repos), func(i int) {
		sets[i], probs[i] = coListRepo(ctx, w.Root, repos[i], f.PrimaryOnly)
	})
	sets = slices.DeleteFunc(sets, func(s *coRepoCheckouts) bool { return s == nil })
	all := slices.Concat(probs...)
	if len(f.Paths) == 0 {
		return sets, all, nil
	}
	return coKeepPaths(sets, all, f.Paths)
}

// coKeepPaths applies CheckoutFilter.Paths.
func coKeepPaths(sets []*coRepoCheckouts, probs []Problem, paths []string) ([]*coRepoCheckouts, []Problem, error) {
	found := map[string]bool{}
	for _, s := range sets {
		s.checkouts = slices.DeleteFunc(s.checkouts, func(c Checkout) bool { return !slices.Contains(paths, c.Path) })
		for _, c := range s.checkouts {
			found[c.Path] = true
		}
	}
	sets = slices.DeleteFunc(sets, func(s *coRepoCheckouts) bool { return len(s.checkouts) == 0 })
	probs = slices.DeleteFunc(probs, func(p Problem) bool { return !slices.Contains(paths, p.Path) })
	for _, p := range probs {
		found[p.Path] = true
	}
	for _, p := range paths {
		if !found[p] {
			return nil, nil, fmt.Errorf("%s is not a checkout of the selected repositories", p)
		}
	}
	return sets, probs, nil
}

// ListCheckouts returns the checkouts of the repositories of w selected by f:
// sorted by repository, then primary first, then linked worktrees by
// directory name (those outside the repository directory last, by path).
// Checkouts that cannot be listed (missing directories, worktrees of another
// repository, git errors) are returned as problems, sorted the same way. The
// error is only set for an invalid filter. Up to jobs repositories are listed
// in parallel.
func ListCheckouts(ctx context.Context, w *Workspace, f CheckoutFilter, jobs int) ([]Checkout, []Problem, error) {
	sets, probs, err := coEnumerate(ctx, w, f, jobs)
	if err != nil {
		return nil, nil, err
	}
	var cs []Checkout
	for _, s := range sets {
		cs = append(cs, s.checkouts...)
	}
	return cs, probs, nil
}

// coForEach calls fn(0..n-1) with at most jobs (minimum 1) calls running at a
// time and returns when all have returned.
func coForEach(jobs, n int, fn func(i int)) {
	if jobs < 1 {
		jobs = 1
	}
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i)
		})
	}
	wg.Wait()
}

// coFetchLocks serializes fetches per git common directory: worktrees share
// the remote-tracking refs, and concurrent fetches into one repository fail
// on ref locks (see gitx.FetchRemote).
var coFetchLocks = struct {
	sync.Mutex
	m map[string]*sync.Mutex
}{m: map[string]*sync.Mutex{}}

// coFetchHook, when set by a test, runs while a repository fetch holds its
// per-repository lock.
var coFetchHook func(commonDir string)

// RemotesOf returns the remotes of a repository of the bundle: origin
// alone, or upstream (the repository) and origin (the user's fork) when a
// fork is recorded. In the documentation of this package, "origin" stands
// for these remotes: fetches fetch all of them (reference first), the
// default branch and merged checks use the reference remote
// (<reference>/<default branch>), existing remote branches are looked up in
// the fork first, and pull request refs are fetched from the reference.
func RemotesOf(r bundle.Repository) gitx.Remotes {
	if r.Forked() {
		return gitx.ForkRemotes
	}
	return gitx.OriginRemotes
}

// fetchRemotes fetches each remote of rs into the repository of dir
// (reference first), stopping at the first failure. Callers serialize it
// per repository (see coFetchRepo).
func fetchRemotes(ctx context.Context, dir string, rs gitx.Remotes) error {
	if !rs.Forked() {
		return gitx.FetchRemote(ctx, dir, rs.Reference)
	}
	for _, remote := range rs.All() {
		if err := gitx.FetchRemote(ctx, dir, remote); err != nil {
			return &remoteFetchError{Remote: remote, Err: err}
		}
	}
	return nil
}

// remoteFetchError is the failure of fetchRemotes for one remote of a
// repository with a fork (the remotes after it were not fetched).
type remoteFetchError struct {
	Remote string
	Err    error
}

func (e *remoteFetchError) Error() string { return "fetching " + e.Remote + ": " + e.Err.Error() }
func (e *remoteFetchError) Unwrap() error { return e.Err }

// onlyForkFetchFailed reports whether err is the failure of the fork fetch
// alone (the reference was fetched).
func onlyForkFetchFailed(err error, rs gitx.Remotes) bool {
	var rf *remoteFetchError
	return rs.Forked() && errors.As(err, &rf) && rf.Remote == rs.Fork
}

// coFetchRepo fetches the remotes rs into the repository of dir (see
// fetchRemotes), serialized with the other fetches into the same
// repository. Under a context made by gitx.WithInteractive, git keeps the
// terminal.
func coFetchRepo(ctx context.Context, dir string, rs gitx.Remotes) error {
	cd, err := gitx.CommonDir(ctx, dir)
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(cd); err == nil {
		cd = r
	}
	coFetchLocks.Lock()
	l := coFetchLocks.m[cd]
	if l == nil {
		l = &sync.Mutex{}
		coFetchLocks.m[cd] = l
	}
	coFetchLocks.Unlock()
	l.Lock()
	defer l.Unlock()
	if coFetchHook != nil {
		coFetchHook(cd)
	}
	return fetchRemotes(ctx, dir, rs)
}

// FetchOptions configures FetchRepos.
type FetchOptions struct {
	Repos []string // see CheckoutFilter.Repos
	Jobs  int
}

// FetchRepos runs one `git fetch --prune origin` per selected repository (in
// its primary checkout; worktrees share the remote-tracking refs), up to
// Jobs repositories in parallel; for a repository with a fork, upstream and
// then origin. Only these remotes are fetched, like update, which
// fast-forwards to their remote-tracking refs: fetching every remote would
// add network round trips and credential prompts that update does not use.
// A missing primary checkout fails that repository. The error is only set for
// an invalid filter.
func FetchRepos(ctx context.Context, w *Workspace, opts FetchOptions, rep Reporter) (Summary, error) {
	ctx = withCommandCache(ctx)
	repos, err := coSelectRepos(w.Bundle(), opts.Repos)
	if err != nil {
		return Summary{}, err
	}
	tasks := make([]Task, 0, len(repos))
	for _, r := range repos {
		tasks = append(tasks, Task{Repo: r.Name, Phase: PhaseFetch, Run: func(ctx context.Context, _ func(Event)) (string, error) {
			primary, _, prob := coCheckPrimary(ctx, w.Root, r)
			if prob != nil {
				return "", fmt.Errorf("%s: %w", prob.Path, prob.Err)
			}
			if err := coFetchRepo(ctx, primary, RemotesOf(r)); err != nil {
				return "", err
			}
			return "fetched " + RemotesOf(r).String(), nil
		}})
	}
	return Execute(ctx, opts.Jobs, tasks, rep), nil
}

// tagMovedTo returns HEAD of a worktree pinned at a tag whose HEAD moved,
// "" otherwise.
func tagMovedTo(wt gitx.Worktree) string {
	if wt.Tag != "" && wt.TagMoved {
		return wt.Head
	}
	return ""
}

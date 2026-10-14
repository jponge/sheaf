package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// PhaseWorktree is the phase of worktree add/remove events.
const PhaseWorktree = "worktree"

// RepoContext is a repository of the bundle and, when it was inferred from
// the working directory, the directory name of the checkout the working
// directory is in (empty when it is directly in <root>/<repo>).
type RepoContext struct {
	Name     string
	Worktree string
}

// lookupRepo finds a repository by directory name: exact match first, then
// a case-insensitive one (unique: bundle validation rejects names differing
// only by case).
func lookupRepo(b *bundle.Bundle, name string) (bundle.Repository, bool) {
	if r, ok := b.Repository(name); ok {
		return *r, true
	}
	for _, r := range b.Repositories {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return bundle.Repository{}, false
}

// repoList describes the repositories of b for error messages.
func repoList(b *bundle.Bundle) string {
	if len(b.Repositories) == 0 {
		return "the bundle has no repositories"
	}
	var n []string
	for _, r := range b.Repositories {
		n = append(n, r.Name)
	}
	slices.Sort(n)
	return "repositories: " + strings.Join(n, ", ")
}

// ResolveRepo resolves the optional repository argument of a command. A
// non-empty arg must name a repository of the bundle (exactly, or
// case-insensitively). An empty arg is inferred from cwd when it is inside
// <root>/<repo> of a registered repository; RepoContext.Worktree is then the
// first path component below it, if any. Errors list the repositories.
func ResolveRepo(w *Workspace, arg, cwd string) (RepoContext, error) {
	b := w.Bundle()
	if arg != "" {
		r, ok := lookupRepo(b, arg)
		if !ok {
			return RepoContext{}, fmt.Errorf("no repository named %q in the bundle (%s)", arg, repoList(b))
		}
		return RepoContext{Name: r.Name}, nil
	}
	if cwd != "" {
		rc := resolveExisting(cwd)
		if rel, err := filepath.Rel(w.Root, rc); err == nil && rel != "." && within(w.Root, rc) {
			parts := strings.Split(rel, string(filepath.Separator))
			if r, ok := lookupRepo(b, parts[0]); ok {
				ctx := RepoContext{Name: r.Name}
				if len(parts) > 1 {
					ctx.Worktree = parts[1]
				}
				return ctx, nil
			}
		}
	}
	return RepoContext{}, fmt.Errorf("no repository given and %s is not inside a repository of the bundle %s (%s)", cwd, w.Root, repoList(b))
}

// WorktreeInfo describes one checkout of a repository: its primary checkout
// or a linked worktree.
type WorktreeInfo struct {
	Repo string `json:"repo"`
	// Dir is the directory name under <root>/<repo>; empty when Outside.
	Dir string `json:"dir"`
	// Branch is empty when Detached.
	Branch         string `json:"branch"`
	Path           string `json:"path"`
	Head           string `json:"head"`
	Primary        bool   `json:"primary"`
	Detached       bool   `json:"detached"`
	Locked         bool   `json:"locked"`
	LockReason     string `json:"lock_reason,omitempty"`
	Prunable       bool   `json:"prunable"`
	PrunableReason string `json:"prunable_reason,omitempty"`
	// Outside is true for a worktree that is not directly under
	// <root>/<repo> (created by hand elsewhere).
	Outside bool `json:"outside"`
	// OffDefault is true for a primary checkout not on the default branch.
	OffDefault bool `json:"off_default"`
}

// ListWorktrees lists the checkouts of repo (resolved like ResolveRepo with
// a non-empty argument), or of every repository when repo is empty, ordered
// by repository, primary first, then by directory name (worktrees outside
// the repository directory last, by path). A repository that cannot be
// listed (directory or primary checkout missing, git failure) does not stop
// the others: the listing of the others is returned with one RepoError per
// failed repository. The error is reserved for an unknown repo and
// cancellation.
func ListWorktrees(ctx context.Context, w *Workspace, repo string) ([]WorktreeInfo, []RepoError, error) {
	b := w.Bundle()
	var repos []bundle.Repository
	if repo != "" {
		r, ok := lookupRepo(b, repo)
		if !ok {
			return nil, nil, fmt.Errorf("no repository named %q in the bundle (%s)", repo, repoList(b))
		}
		repos = []bundle.Repository{r}
	} else {
		repos = slices.Clone(b.Repositories)
		slices.SortFunc(repos, func(x, y bundle.Repository) int { return strings.Compare(x.Name, y.Name) })
	}
	var (
		out   []WorktreeInfo
		fails []RepoError
	)
	for _, r := range repos {
		infos, _, _, err := listRepo(ctx, w.Root, r)
		if err != nil {
			if ctx.Err() != nil {
				return out, fails, ctx.Err()
			}
			fails = append(fails, RepoError{Repo: r.Name, Err: err})
			continue
		}
		out = append(out, infos...)
	}
	return out, fails, nil
}

// listRepo returns the sorted checkouts of r, its repository directory and
// its primary checkout. The primary checkout must be
// <root>/<repo>/<normalized default branch> (the layout invariant).
func listRepo(ctx context.Context, root string, r bundle.Repository) (infos []WorktreeInfo, repoDir, primary string, err error) {
	repoDir, err = repoDirOf(root, r.Name)
	if err != nil {
		return nil, "", "", err
	}
	if repoDir == "" {
		return nil, "", "", fmt.Errorf("directory %s does not exist", bundle.RepoDir(root, r.Name))
	}
	primary, err = bundle.WorktreeDir(root, r.Name, r.DefaultBranch)
	if err != nil {
		return nil, "", "", err
	}
	if !isPrimaryCheckout(ctx, primary) {
		return nil, "", "", fmt.Errorf("no primary checkout at %s", primary)
	}
	wts, err := gitx.WorktreeList(ctx, primary)
	if err != nil {
		return nil, "", "", err
	}
	for i, wt := range wts {
		if wt.Bare {
			continue
		}
		p := filepath.Clean(wt.Path)
		info := WorktreeInfo{
			Repo: r.Name, Branch: wt.Branch, Path: p, Head: wt.Head, Primary: i == 0,
			Detached: wt.Detached, Locked: wt.Locked, LockReason: wt.LockReason,
			Prunable: wt.Prunable, PrunableReason: wt.PrunableReason,
		}
		info.OffDefault = info.Primary && info.Branch != r.DefaultBranch
		if rp := resolveExisting(p); filepath.Dir(rp) == repoDir && filepath.Dir(p) == repoDir {
			info.Dir = filepath.Base(p)
		} else {
			info.Outside = true
		}
		infos = append(infos, info)
	}
	slices.SortStableFunc(infos, func(a, b WorktreeInfo) int {
		switch {
		case a.Primary != b.Primary:
			if a.Primary {
				return -1
			}
			return 1
		case a.Outside != b.Outside:
			if b.Outside {
				return -1
			}
			return 1
		case a.Dir != b.Dir:
			return strings.Compare(a.Dir, b.Dir)
		}
		return strings.Compare(a.Path, b.Path)
	})
	return infos, repoDir, primary, nil
}

// errNoWorktree is wrapped by findWorktree when nothing matches.
var errNoWorktree = errors.New("unknown worktree")

// findWorktree picks the checkout named by tgt. With TargetAny, the
// checkout whose branch is tgt.Name and the one whose directory name is
// tgt.Name must be the same if both exist (otherwise the name is ambiguous
// and --branch or --dir must select); failing an exact match, the single
// checkout whose branch or directory name equals it case-insensitively is
// used. TargetBranch and TargetDir only consider branches, respectively
// directory names.
func findWorktree(infos []WorktreeInfo, repo string, tgt WorktreeTarget) (WorktreeInfo, error) {
	name := tgt.Name
	useBranch, useDir := tgt.Kind != TargetDir, tgt.Kind != TargetBranch
	byBranch, byDir := -1, -1
	for i, in := range infos {
		if useBranch && byBranch < 0 && in.Branch != "" && in.Branch == name {
			byBranch = i
		}
		if useDir && byDir < 0 && !in.Outside && in.Dir == name {
			byDir = i
		}
	}
	switch {
	case byBranch >= 0 && byDir >= 0 && byBranch != byDir:
		return WorktreeInfo{}, fmt.Errorf("%q is ambiguous in %s: it is the branch of %s and the directory of %s; select one with --branch %s or --dir %s",
			name, repo, describe(infos[byBranch]), describe(infos[byDir]), name, name)
	case byBranch >= 0:
		return infos[byBranch], nil
	case byDir >= 0:
		return infos[byDir], nil
	}
	found := -1
	for i, in := range infos {
		if useBranch && in.Branch != "" && strings.EqualFold(in.Branch, name) || useDir && !in.Outside && strings.EqualFold(in.Dir, name) {
			if found >= 0 {
				return WorktreeInfo{}, fmt.Errorf("%q is ambiguous in %s: it matches %s and %s; select one with --branch or --dir", name, repo, describe(infos[found]), describe(in))
			}
			found = i
		}
	}
	if found >= 0 {
		return infos[found], nil
	}
	var all []string
	for _, in := range infos {
		all = append(all, describe(in))
	}
	what := "worktree"
	switch tgt.Kind {
	case TargetDir:
		what = "worktree with directory"
	case TargetBranch:
		what = "worktree with branch"
	}
	return WorktreeInfo{}, fmt.Errorf("%w: no %s %q in %s (worktrees: %s)", errNoWorktree, what, name, repo, strings.Join(all, ", "))
}

// describe names a checkout in messages: "dir [branch]".
func describe(in WorktreeInfo) string {
	name := in.Dir
	if in.Outside {
		name = in.Path
	}
	if in.Branch != "" {
		return name + " [" + in.Branch + "]"
	}
	return name + " [detached]"
}

// TargetKind tells how a WorktreeTarget names a checkout.
type TargetKind int

const (
	// TargetAny matches a branch or a directory name.
	TargetAny TargetKind = iota
	// TargetDir matches a directory name under <root>/<repo> only.
	TargetDir
	// TargetBranch matches a branch name only.
	TargetBranch
)

// WorktreeTarget names a checkout of a repository.
type WorktreeTarget struct {
	Name string
	Kind TargetKind
}

// Test seams (nil in production).
var (
	testHookAfterAddCheck       func()
	testHookAfterWorktreeRemove func()
)

// RepoError is the failure of one repository in a multi-repository listing.
type RepoError struct {
	Repo string
	Err  error
}

func (e *RepoError) Error() string { return e.Repo + ": " + e.Err.Error() }
func (e *RepoError) Unwrap() error { return e.Err }

// WorktreeAddOptions configures AddWorktree.
type WorktreeAddOptions struct {
	// Base is the start point of a new branch; default "origin/<default
	// branch>" (default branch from sheaf.yaml). It is ignored (with a
	// warning) when the branch exists locally or on origin.
	Base string
	// NoFetch skips the fetch of origin.
	NoFetch bool
}

// WorktreeAddResult is the outcome of AddWorktree.
type WorktreeAddResult struct {
	Repo   string
	Branch string
	Dir    string
	Path   string
	Mode   gitx.WorktreeAddMode
	// Upstream is the upstream of the checked-out branch, empty if none.
	Upstream string
	// Base is the start point used, set for gitx.WorktreeNewBranch only.
	Base string
	// Warnings are also emitted as events.
	Warnings []string
}

// AddWorktree creates a worktree for branch at
// <root>/<repo>/<NormalizeBranch(branch)>, from the primary checkout of repo
// (a repository name, resolved like ResolveRepo with a non-empty argument).
//
// It validates the branch name (git check-ref-format --branch), fetches
// origin unless opts.NoFetch (a failed fetch is an error), and refuses a
// branch already checked out in a checkout of the repository, a directory
// name used by another branch, a detached worktree or a non-worktree entry
// (case-insensitively). Then gitx.WorktreeAdd checks out the local branch,
// or creates one tracking origin/<branch>, or creates a new branch from the
// base, in that order of preference. A local branch tracking something else
// than origin/<branch>, or without upstream although origin/<branch>
// exists, yields a warning.
func AddWorktree(ctx context.Context, w *Workspace, repo, branch string, opts WorktreeAddOptions, emit func(Event)) (WorktreeAddResult, error) {
	b := w.Bundle()
	r, ok := lookupRepo(b, repo)
	if !ok {
		return WorktreeAddResult{}, fmt.Errorf("no repository named %q in the bundle (%s)", repo, repoList(b))
	}
	report := func(phase, msg string) {
		emit(Event{Repo: r.Name, Phase: phase, Kind: KindProgress, Message: msg})
	}
	primary, err := bundle.WorktreeDir(w.Root, r.Name, r.DefaultBranch)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	if !isPrimaryCheckout(ctx, primary) {
		return WorktreeAddResult{}, fmt.Errorf("%s: no primary checkout at %s", r.Name, primary)
	}
	if err := checkBranchName(ctx, primary, branch); err != nil {
		return WorktreeAddResult{}, err
	}
	if err := checkNotRefLike(ctx, primary, branch); err != nil {
		return WorktreeAddResult{}, err
	}
	dir, err := bundle.NormalizeBranch(branch)
	if err != nil {
		return WorktreeAddResult{}, err
	}
	// Cheap checks before the fetch; repeated under the lock below.
	infos, repoDir, _, err := listRepo(ctx, w.Root, r)
	if err != nil {
		return WorktreeAddResult{}, fmt.Errorf("%s: %w", r.Name, err)
	}
	if err := checkDefaultBranch(infos, r, branch); err != nil {
		return WorktreeAddResult{}, err
	}
	if err := checkAddTarget(infos, repoDir, branch, dir); err != nil {
		return WorktreeAddResult{}, err
	}
	if !opts.NoFetch {
		report(PhaseFetch, "fetching origin")
		if err := gitx.Fetch(ctx, primary); err != nil {
			if ctx.Err() != nil {
				return WorktreeAddResult{}, ctx.Err()
			}
			return WorktreeAddResult{}, fmt.Errorf("fetching %s failed: %w\npass --no-fetch to create the worktree from the local state of the repository", r.Name, err)
		}
	}
	// Serialize the check and the creation with other sheaf processes and
	// tasks, so two adds whose directories collide cannot both succeed.
	unlock, err := lockBundle(ctx, w.Root, func() {
		report(PhaseWorktree, "waiting for "+lockFileName+" (another sheaf process is working on the bundle)")
	})
	if err != nil {
		if ctx.Err() != nil {
			return WorktreeAddResult{}, ctx.Err()
		}
		return WorktreeAddResult{}, fmt.Errorf("locking %s: %w", bundle.FileName, err)
	}
	defer unlock()
	if infos, repoDir, _, err = listRepo(ctx, w.Root, r); err != nil {
		return WorktreeAddResult{}, fmt.Errorf("%s: %w", r.Name, err)
	}
	if err := checkAddTarget(infos, repoDir, branch, dir); err != nil {
		return WorktreeAddResult{}, err
	}
	if testHookAfterAddCheck != nil {
		testHookAfterAddCheck()
	}
	base := opts.Base
	if base == "" {
		base = "origin/" + r.DefaultBranch
	}
	path := filepath.Join(repoDir, dir)
	ev := func(msg string) {
		emit(Event{Repo: r.Name, Worktree: dir, Phase: PhaseWorktree, Kind: KindProgress, Message: msg})
	}
	ev("creating worktree " + path)
	res, err := gitx.WorktreeAdd(ctx, primary, path, branch, gitx.WorktreeAddOpts{Base: base})
	if err != nil {
		if ctx.Err() != nil {
			return WorktreeAddResult{}, ctx.Err()
		}
		return WorktreeAddResult{}, fmt.Errorf("creating worktree %s: %w", path, err)
	}
	out := WorktreeAddResult{Repo: r.Name, Branch: branch, Dir: dir, Path: path, Mode: res.Mode, Upstream: res.Upstream}
	remoteRef := "origin/" + branch
	switch res.Mode {
	case gitx.WorktreeLocalBranch:
		msg := "checked out existing local branch " + branch
		if res.Upstream != "" {
			msg += " (upstream " + res.Upstream + ")"
		}
		ev(msg)
		if res.Upstream != "" && res.Upstream != remoteRef {
			out.Warnings = append(out.Warnings, fmt.Sprintf("local branch %s tracks %s, not %s", branch, res.Upstream, remoteRef))
		} else if res.Upstream == "" {
			if ok, err := refExists(ctx, primary, "refs/remotes/"+remoteRef); err == nil && ok {
				out.Warnings = append(out.Warnings, fmt.Sprintf("local branch %s has no upstream although %s exists (git branch --set-upstream-to=%s %s)", branch, remoteRef, remoteRef, branch))
			}
		}
	case gitx.WorktreeRemoteBranch:
		ev("created local branch " + branch + " tracking " + remoteRef)
	case gitx.WorktreeNewBranch:
		out.Base = base
		ev("created new branch " + branch + " from " + base + " (no upstream)")
	}
	if opts.Base != "" && res.Mode != gitx.WorktreeNewBranch {
		out.Warnings = append(out.Warnings, fmt.Sprintf("--base %s ignored: branch %s already exists", opts.Base, branch))
	}
	for _, wmsg := range out.Warnings {
		ev("warning: " + wmsg)
	}
	return out, nil
}

// checkBranchName accepts names that `git check-ref-format --branch` accepts
// unchanged (it expands @{-N}, which is refused here).
func checkBranchName(ctx context.Context, dir, branch string) error {
	out, err := gitx.Run(ctx, dir, "check-ref-format", "--branch", branch)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%q is not a valid branch name", branch)
	}
	if strings.TrimSpace(out) != branch {
		return fmt.Errorf("%q is not a valid branch name (git expands it to %q)", branch, strings.TrimSpace(out))
	}
	return nil
}

// refLikePrefixes are branch name prefixes that make the branch easy to
// confuse with other refs (e.g. a branch "remotes/origin/x" or "origin/x"
// shadowing refs/remotes/origin/x in revision lookups).
var refLikePrefixes = []string{"refs/", "heads/", "remotes/", "tags/"}

// checkNotRefLike refuses branch names starting with refLikePrefixes or
// with "<remote>/" for a configured remote.
func checkNotRefLike(ctx context.Context, dir, branch string) error {
	for _, p := range refLikePrefixes {
		if strings.HasPrefix(branch, p) {
			return fmt.Errorf("branch name %q is not allowed: it starts with %q and would be confused with other refs", branch, p)
		}
	}
	out, err := gitx.Run(ctx, dir, "remote")
	if err != nil {
		return err
	}
	for _, rem := range strings.Fields(out) {
		if branch == rem {
			return fmt.Errorf("branch name %q is not allowed: it is the name of a remote", branch)
		}
		if strings.HasPrefix(branch, rem+"/") {
			return fmt.Errorf("branch name %q is not allowed: it starts with %q and would be confused with the remote-tracking branch refs/remotes/%s", branch, rem+"/", branch)
		}
	}
	return nil
}

// checkDefaultBranch refuses adding a worktree for the default branch: its
// checkout is the primary checkout.
func checkDefaultBranch(infos []WorktreeInfo, r bundle.Repository, branch string) error {
	if branch != r.DefaultBranch {
		return nil
	}
	for _, in := range infos {
		if !in.Primary {
			continue
		}
		msg := fmt.Sprintf("%s is the default branch of %s: its checkout is the primary checkout %s", branch, r.Name, in.Path)
		if in.OffDefault {
			on := "on " + in.Branch
			if in.Branch == "" {
				on = "on a detached HEAD"
			}
			msg += fmt.Sprintf(", which is currently %s (switch it back with `git -C %s switch %s`)", on, in.Path, branch)
		}
		return errors.New(msg)
	}
	return nil
}

// checkAddTarget refuses an add whose branch is already checked out, or
// whose directory name dir is taken (case-insensitively) by another branch,
// a detached worktree or an entry that is not a worktree.
func checkAddTarget(infos []WorktreeInfo, repoDir, branch, dir string) error {
	existing := map[string]string{}
	known := map[string]bool{}
	for _, in := range infos {
		if in.Branch == branch {
			return fmt.Errorf("branch %q is already checked out at %s", branch, in.Path)
		}
		if in.Outside {
			continue
		}
		known[in.Dir] = true
		if in.Branch == "" {
			if strings.EqualFold(in.Dir, dir) {
				return fmt.Errorf("directory %s is used by a worktree with a detached HEAD", filepath.Join(repoDir, in.Dir))
			}
			continue
		}
		existing[in.Dir] = in.Branch
	}
	if err := bundle.CheckWorktreeName(existing, branch); err != nil {
		return err
	}
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !known[e.Name()] && strings.EqualFold(e.Name(), dir) {
			return fmt.Errorf("%s already exists and is not a worktree; move it away first", filepath.Join(repoDir, e.Name()))
		}
	}
	return nil
}

// refExists reports whether the fully qualified ref exists.
func refExists(ctx context.Context, dir, ref string) (bool, error) {
	_, err := gitx.Run(ctx, dir, "show-ref", "--verify", "--quiet", ref)
	if err == nil {
		return true, nil
	}
	var ge *gitx.GitError
	if errors.As(err, &ge) && ge.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// WorktreeRemoveOptions configures RemoveWorktree.
type WorktreeRemoveOptions struct {
	// Force overrides blocking risks and an unmerged branch (not the
	// problems listed in WorktreeRemovePlan.Hard).
	Force bool
	// DeleteBranch also deletes the worktree's local branch.
	DeleteBranch bool
	// ForceDeleteBranch deletes the branch even when it is not merged
	// (implies DeleteBranch).
	ForceDeleteBranch bool
	// Offline skips the fetches.
	Offline bool
	// WorkingDir is the caller's working directory; empty means os.Getwd.
	WorkingDir string
	// Confirm is called with the plan once RemoveWorktree may proceed;
	// returning false (or a nil Confirm) aborts with ErrNotConfirmed.
	Confirm func(WorktreeRemovePlan) bool
}

// WorktreeRemovePlan describes a worktree removal.
type WorktreeRemovePlan struct {
	Repo     string
	Worktree WorktreeInfo
	// Risks lists all the risks of the worktree, including repository-wide
	// and informational ones (which do not block: the repository and its
	// branches survive the removal).
	Risks []gitx.Risk
	// Hard lists problems that Force does not override.
	Hard []string
	// Unmerged explains why the branch cannot be deleted without Force
	// (DeleteBranch only), empty otherwise.
	Unmerged string
	// MergedInto is the ref the branch is merged into (DeleteBranch only).
	MergedInto string
	// ContainedIn names a ref containing the branch tip when the branch is
	// not merged but no commit would be lost (DeleteBranch only).
	ContainedIn string
	// TagOnlyCommits counts the commits of the branch reachable only from
	// tags besides it (set with Unmerged).
	TagOnlyCommits int
	// BranchKept explains why the branch will be kept although its deletion
	// was requested (e.g. it is checked out in another worktree).
	BranchKept string
	// UnmergedCommits counts the commits of the branch reachable from no
	// other branch nor remote-tracking ref (set with Unmerged).
	UnmergedCommits int

	branchOID string
}

// Blocking returns the risks that prevent the removal without Force.
func (p WorktreeRemovePlan) Blocking() []gitx.Risk {
	var out []gitx.Risk
	for _, r := range p.Risks {
		if !r.Informational() && !r.Kind.RepositoryWide() {
			out = append(out, r)
		}
	}
	return out
}

// NonBlocking returns the risks shown for information only.
func (p WorktreeRemovePlan) NonBlocking() []gitx.Risk {
	var out []gitx.Risk
	for _, r := range p.Risks {
		if r.Informational() || r.Kind.RepositoryWide() {
			out = append(out, r)
		}
	}
	return out
}

// Refused reports whether RemoveWorktree refuses the plan with opts:
// hard problems always, blocking risks without Force, an unmerged branch
// without ForceDeleteBranch.
func (p WorktreeRemovePlan) Refused(opts WorktreeRemoveOptions) bool {
	return len(p.Hard) > 0 || !opts.Force && len(p.Blocking()) > 0 || p.Unmerged != "" && !opts.ForceDeleteBranch
}

// WorktreeRefusedError is returned when RemoveWorktree refuses the plan.
type WorktreeRefusedError struct {
	Plan              WorktreeRemovePlan
	Force             bool
	ForceDeleteBranch bool
}

func (e *WorktreeRefusedError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "refusing to remove worktree %s of %s:", describe(e.Plan.Worktree), e.Plan.Repo)
	for _, h := range e.Plan.Hard {
		sb.WriteString("\n  " + h)
	}
	blocking := !e.Force && len(e.Plan.Blocking()) > 0
	if blocking {
		for _, r := range e.Plan.Blocking() {
			sb.WriteString("\n  " + r.String())
		}
	}
	if e.Plan.Unmerged != "" && !e.ForceDeleteBranch {
		sb.WriteString("\n  " + e.Plan.Unmerged + " (use -D to delete anyway)")
	}
	if len(e.Plan.Hard) > 0 {
		sb.WriteString("\nsome problems cannot be overridden; fix them first")
	}
	if blocking {
		sb.WriteString("\npass --force to remove the worktree anyway (the changes listed above will be lost)")
	}
	return sb.String()
}

// RemoveWorktree removes the linked worktree of repo named by target (its
// branch or its directory name, see findWorktree), and with
// opts.DeleteBranch its local branch.
//
// The primary checkout is always refused (use Remove). Unless opts.Offline,
// origin is fetched first, and every nested repository with a remote before
// it is assessed; a failed fetch of origin is a hard problem. The worktree
// must be a real directory <root>/<repo>/<dir> (not a symbolic link), not
// locked, present, and must not contain the working directory. Its
// work-tree-local blocking risks (gitx.SafeToRemoveWorktree) refuse the
// removal unless opts.Force; repository-wide risks are informational. With
// DeleteBranch the branch must be merged into its upstream or into
// origin/<default branch> (unless opts.Force), and is deleted only if it
// still points at the commit checked.
//
// The worktree is removed with `git worktree remove`, with --force only
// when opts.Force overrode blocking risks; RemoveWorktree then checks that
// the directory is gone.
func RemoveWorktree(ctx context.Context, w *Workspace, repo string, tgt WorktreeTarget, opts WorktreeRemoveOptions, emit func(Event)) (string, error) {
	b := w.Bundle()
	r, ok := lookupRepo(b, repo)
	if !ok {
		return "", fmt.Errorf("no repository named %q in the bundle (%s)", repo, repoList(b))
	}
	infos, repoDir, primary, err := listRepo(ctx, w.Root, r)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.Name, err)
	}
	wt, err := findWorktree(infos, r.Name, tgt)
	if err != nil {
		return "", err
	}
	if wt.Primary {
		return "", fmt.Errorf("%s (%s) is the primary checkout of %s and cannot be removed as a worktree; use `sheaf remove %s` to remove the repository", describe(wt), wt.Path, r.Name, r.Name)
	}
	deleteBr := opts.DeleteBranch || opts.ForceDeleteBranch
	wd := opts.WorkingDir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	ev := func(phase, msg string) {
		emit(Event{Repo: r.Name, Worktree: wt.Dir, Phase: phase, Kind: KindProgress, Message: msg})
	}
	fetchMsg := ""
	var fetchNested func(context.Context, string) error
	if opts.Offline {
		ev(PhaseFetch, "offline: not fetching; the remote state may be stale")
	} else {
		ev(PhaseFetch, "fetching origin")
		if err := gitx.Fetch(ctx, primary); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			fetchMsg = fetchProblem(err)
		}
		fetched := map[string]error{}
		fetchNested = func(ctx context.Context, dir string) error {
			if err, ok := fetched[dir]; ok {
				return err
			}
			ev(PhaseFetch, "fetching nested repository "+dir)
			err := gitx.FetchAll(ctx, dir)
			if ctx.Err() == nil {
				fetched[dir] = err
			}
			return err
		}
	}
	mkPlan := func() (WorktreeRemovePlan, error) {
		return planWorktreeRemoval(ctx, r, repoDir, primary, wt, deleteBr, wd, fetchMsg, fetchNested)
	}
	ev(PhaseCheck, "checking for data at risk")
	plan, err := mkPlan()
	if err != nil {
		return "", err
	}
	if plan.Refused(opts) {
		return "", &WorktreeRefusedError{Plan: plan, Force: opts.Force, ForceDeleteBranch: opts.ForceDeleteBranch}
	}
	if opts.Confirm == nil || !opts.Confirm(plan) {
		return "", fmt.Errorf("%w: worktree %s", ErrNotConfirmed, wt.Path)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Serialize the final check and the removal with other sheaf processes
	// and tasks (e.g. a concurrent add of the same branch).
	unlock, err := lockBundle(ctx, w.Root, func() {
		ev(PhaseWorktree, "waiting for "+lockFileName+" (another sheaf process is working on the bundle)")
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("locking %s: %w", bundle.FileName, err)
	}
	defer unlock()
	again, err := mkPlan()
	if err != nil {
		return "", err
	}
	if !reflect.DeepEqual(worktreeBlockingView(again), worktreeBlockingView(plan)) {
		return "", fmt.Errorf("worktree %s changed while waiting for confirmation; nothing was removed, run the command again", describe(wt))
	}
	if err := checkContained(repoDir, wt); err != nil {
		return "", err
	}
	// git refuses worktrees with submodules without --force: nested
	// repositories were assessed above (clean ones are informational,
	// blocking ones were overridden with Force), so --force is safe there.
	force := opts.Force && len(plan.Blocking()) > 0 || hasNested(plan.Risks)
	ev(PhaseRemove, "removing worktree "+describe(wt)+" at "+wt.Path)
	if err := gitx.WorktreeRemove(ctx, primary, wt.Path, force); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("removing worktree %s: %w", wt.Path, err)
	}
	if _, err := os.Lstat(wt.Path); !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("git worktree remove succeeded but %s still exists", wt.Path)
	}
	if testHookAfterWorktreeRemove != nil {
		testHookAfterWorktreeRemove()
	}
	msg := "removed worktree " + describe(wt) + " at " + wt.Path
	if !deleteBr {
		return msg, nil
	}
	ev(PhaseRemove, "deleting branch "+wt.Branch)
	if reason := deleteBranchIfUnchanged(ctx, primary, wt.Branch, plan.branchOID); reason != "" {
		ev(PhaseRemove, "warning: kept branch "+wt.Branch+": "+reason)
		return msg + "; kept branch " + wt.Branch + ": " + reason, nil
	}
	if plan.MergedInto != "" {
		return msg + fmt.Sprintf("; deleted branch %s (merged into %s)", wt.Branch, plan.MergedInto), nil
	}
	if plan.ContainedIn != "" {
		return msg + fmt.Sprintf("; deleted branch %s (contained in %s)", wt.Branch, plan.ContainedIn), nil
	}
	return msg + fmt.Sprintf("; deleted unmerged branch %s (%s lost)", wt.Branch, commits(plan.UnmergedCommits)), nil
}

func hasNested(risks []gitx.Risk) bool {
	for _, r := range risks {
		if r.Kind == gitx.RiskNestedRepository || r.Kind == gitx.RiskNestedRepositoryClean {
			return true
		}
	}
	return false
}

func commits(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}

// checkContained verifies that wt is the real directory <repoDir>/<dir>.
func checkContained(repoDir string, wt WorktreeInfo) error {
	if wt.Outside || wt.Dir == "" {
		return fmt.Errorf("refusing to remove %s: worktree outside %s; sheaf only removes worktrees located in it", wt.Path, repoDir)
	}
	expected := filepath.Join(repoDir, wt.Dir)
	if filepath.Clean(wt.Path) != expected {
		return fmt.Errorf("refusing to remove %s: not %s", wt.Path, expected)
	}
	fi, err := os.Lstat(expected)
	if err != nil {
		return fmt.Errorf("refusing to remove %s: %w", expected, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove %s: it is a symbolic link", expected)
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to remove %s: not a directory", expected)
	}
	if rp, err := filepath.EvalSymlinks(expected); err != nil || rp != expected {
		return fmt.Errorf("refusing to remove %s: it does not resolve to itself (%s, %v)", expected, rp, err)
	}
	return nil
}

func planWorktreeRemoval(ctx context.Context, r bundle.Repository, repoDir, primary string, wt WorktreeInfo, deleteBr bool, wd, fetchMsg string,
	fetchNested func(context.Context, string) error) (WorktreeRemovePlan, error) {
	plan := WorktreeRemovePlan{Repo: r.Name, Worktree: wt}
	inspectable := true
	if wt.Prunable {
		plan.Hard = append(plan.Hard, "its directory is missing; clean it up with `git worktree prune`")
		inspectable = false
	} else if err := checkContained(repoDir, wt); err != nil {
		plan.Hard = append(plan.Hard, err.Error())
		inspectable = false
	}
	if wt.Locked {
		p := "locked"
		if wt.LockReason != "" {
			p += " (" + wt.LockReason + ")"
		}
		plan.Hard = append(plan.Hard, p+"; unlock it with `git worktree unlock "+wt.Path+"`")
	}
	if wd != "" {
		if rwd := resolveExisting(wd); within(resolveExisting(wt.Path), rwd) {
			plan.Hard = append(plan.Hard, "it contains the current working directory "+rwd+"; change to another directory first")
		}
	}
	if fetchMsg != "" {
		plan.Hard = append(plan.Hard, fetchMsg)
	}
	if deleteBr && wt.Branch == "" {
		plan.Hard = append(plan.Hard, "detached HEAD: there is no branch to delete")
	}
	if inspectable {
		risks, err := gitx.DeletionRisksWithOptions(ctx, wt.Path, gitx.DeletionRisksOptions{BeforeNested: fetchNested, DeletedRoot: wt.Path})
		if err != nil {
			return plan, err
		}
		plan.Risks = risks
	}
	if deleteBr && wt.Branch != "" {
		if err := checkMerged(ctx, primary, r, wt.Branch, &plan); err != nil {
			return plan, err
		}
		// Normally impossible (git worktree add -f): the branch survives.
		wts, err := gitx.WorktreeList(ctx, primary)
		if err != nil {
			return plan, err
		}
		for _, o := range wts {
			if o.Branch == wt.Branch && filepath.Clean(o.Path) != wt.Path {
				plan.BranchKept = fmt.Sprintf("branch %s is checked out at %s and will be kept", wt.Branch, filepath.Clean(o.Path))
				break
			}
		}
	}
	return plan, nil
}

// checkMerged fills plan.branchOID and either plan.MergedInto or
// plan.Unmerged with plan.UnmergedCommits: the branch is merged when its tip
// is an ancestor of (or equal to) its upstream or origin/<default branch>.
// An upstream counts only when it is a remote-tracking ref or another local
// branch: a branch configured as its own upstream (remote ".") does not.
func checkMerged(ctx context.Context, primary string, r bundle.Repository, branch string, plan *WorktreeRemovePlan) error {
	own := "refs/heads/" + branch
	oid, err := gitx.Run(ctx, primary, "rev-parse", "--verify", "--end-of-options", own)
	if err != nil {
		return err
	}
	plan.branchOID = strings.TrimSpace(oid)
	up, err := gitx.Run(ctx, primary, "for-each-ref", "--format=%(upstream)", own)
	if err != nil {
		return err
	}
	var targets []string
	if u := strings.TrimSpace(up); strings.HasPrefix(u, "refs/remotes/") || strings.HasPrefix(u, "refs/heads/") && u != own {
		targets = append(targets, u)
	}
	if d := "refs/remotes/origin/" + r.DefaultBranch; !slices.Contains(targets, d) {
		targets = append(targets, d)
	}
	var names []string
	for _, t := range targets {
		short := strings.TrimPrefix(strings.TrimPrefix(t, "refs/remotes/"), "refs/heads/")
		names = append(names, short)
		if ok, err := refExists(ctx, primary, t); err != nil || !ok {
			continue
		}
		_, err := gitx.Run(ctx, primary, "merge-base", "--is-ancestor", plan.branchOID, t)
		if err == nil {
			plan.MergedInto = short
			return nil
		}
		var ge *gitx.GitError
		if !errors.As(err, &ge) || ge.ExitCode != 1 {
			return err
		}
	}
	// Commits lost with the branch: those on no other branch, no
	// remote-tracking ref and no tag. (Branch names cannot contain glob
	// characters, so --exclude matches this branch only.)
	lost, err := revCount(ctx, primary, plan.branchOID, "--not", "--exclude="+branch, "--branches", "--remotes", "--tags")
	if err != nil {
		return err
	}
	if lost == 0 {
		// Every commit is contained in another ref: nothing is lost. Name
		// a ref containing the tip (branches, then remote-tracking refs,
		// then tags, as for-each-ref sorts them).
		out, err := gitx.Run(ctx, primary, "for-each-ref", "--format=%(refname)", "--contains", plan.branchOID, "refs/heads", "refs/remotes", "refs/tags")
		if err != nil {
			return err
		}
		for _, ref := range strings.Fields(out) {
			if ref != own {
				plan.ContainedIn = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/remotes/"), "refs/tags/")
				return nil
			}
		}
		return fmt.Errorf("branch %s: no commit would be lost but no ref contains its tip", branch)
	}
	notTags, err := revCount(ctx, primary, plan.branchOID, "--not", "--exclude="+branch, "--branches", "--remotes")
	if err != nil {
		return err
	}
	plan.UnmergedCommits, plan.TagOnlyCommits = lost, notTags-lost
	plan.Unmerged = fmt.Sprintf("branch %s is not merged into %s; its %s would be lost", branch, strings.Join(names, " nor "), commits(lost))
	if plan.TagOnlyCommits > 0 {
		plan.Unmerged += fmt.Sprintf(" (%d more reachable only from tags)", plan.TagOnlyCommits)
	}
	return nil
}

// revCount runs `git rev-list --count args...`.
func revCount(ctx context.Context, dir string, args ...string) (int, error) {
	out, err := gitx.Run(ctx, dir, append([]string{"rev-list", "--count"}, args...)...)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, fmt.Errorf("rev-list --count: unexpected output %q", out)
	}
	return n, nil
}

// deleteBranchIfUnchanged deletes refs/heads/branch if it is checked out in
// no worktree and still points at oid, then its configuration section (best
// effort). It returns why the branch was kept, or "" once deleted.
func deleteBranchIfUnchanged(ctx context.Context, dir, branch, oid string) string {
	wts, err := gitx.WorktreeList(ctx, dir)
	if err != nil {
		return "cannot list worktrees: " + err.Error()
	}
	for _, wt := range wts {
		if wt.Branch == branch {
			return "it is checked out at " + wt.Path
		}
	}
	if _, err := gitx.Run(ctx, dir, "update-ref", "-d", "refs/heads/"+branch, oid); err != nil {
		return "it changed since it was checked (" + firstLineOf(err.Error()) + ")"
	}
	_, _ = gitx.Run(ctx, dir, "config", "--remove-section", "branch."+branch)
	return ""
}

func firstLineOf(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// worktreeBlockingView keeps what decides a removal.
func worktreeBlockingView(p WorktreeRemovePlan) WorktreeRemovePlan {
	p.Risks = p.Blocking()
	return p
}

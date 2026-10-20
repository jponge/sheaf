package gitx

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// RiskKind is a category of data that deleting a work tree or repository
// would lose.
type RiskKind int

// Risk kinds.
const (
	// Work-tree-local kinds (lost when the work tree is deleted).

	RiskStaged     RiskKind = iota + 1 // entries with staged changes
	RiskUnstaged                       // entries with unstaged changes (incl. submodules with new commits or modified content)
	RiskConflicted                     // unmerged entries
	RiskUntracked                      // untracked entries (incl. submodules with only untracked content)
	// Repository-wide kinds (lost when the repository, i.e. its last work
	// tree, is deleted; irrelevant to removing a linked worktree).

	RiskStash                 // stash entries
	RiskUnpushedBranchCommits // commits on local branches not on any trusted remote-tracking ref (see DeletionRisksOptions) nor DeletionRisksOptions.PublishedRefs
	// RiskDetachedCommits covers commits reachable from this work tree's detached
	// HEAD but from no branch, tag or remote-tracking ref (work-tree-local).
	RiskDetachedCommits
	// RiskTagOnlyCommits covers commits reachable only from tags (neither from a
	// local branch nor from a remote-tracking ref); repository-wide and
	// informational (see Risk.Informational).
	RiskTagOnlyCommits
	// RiskIgnoredFiles covers ignored entries; informational (usually build
	// output), see Risk.Informational.
	RiskIgnoredFiles
	// RiskHiddenModifications covers tracked files flagged skip-worktree or
	// assume-unchanged (`git update-index`) whose work tree content differs
	// from the index. Status does not show them; work-tree-local, blocking.
	// Flagged files absent from the work tree (e.g. outside a sparse
	// checkout) do not count; a symlink entry checked out as a regular file
	// (core.symlinks=false) holding its target does not either.
	RiskHiddenModifications
	// RiskUnverifiable covers skip-worktree or assume-unchanged files whose
	// content could not be compared with the index (e.g. a required clean
	// filter such as Git LFS is not installed); Risk.Detail says why.
	// Work-tree-local, blocking: the files may hold modifications.
	RiskUnverifiable
	// RiskNestedRepository covers other repositories inside the work tree
	// (submodules, embedded gitlinks, independent clones, linked worktrees of
	// other repositories) that have data at risk themselves, have no remote,
	// or could not be inspected. Linked worktrees of the same repository do
	// not count. Work-tree-local, blocking; Risk.Detail lists their paths
	// with the reasons.
	RiskNestedRepository
	// RiskNestedRepositoryClean covers nested repositories with nothing to lose
	// (clean, with a remote, everything pushed); informational, work-tree-
	// local. Risk.Detail lists their paths.
	RiskNestedRepositoryClean
	// RiskForeignWorktree covers work trees of other repositories found inside the
	// work tree, at any nesting level, whose .git file names a git directory
	// outside the deleted tree (or cannot be read): deleting them would lose
	// their changes unchecked and leave their repository's administrative
	// files dangling. Reported only with
	// DeletionRisksOptions.ForeignWorktrees (they are nested repositories
	// otherwise); work-tree-local, blocking. Risk.Detail lists their paths
	// with the git directories.
	RiskForeignWorktree
)

func (k RiskKind) String() string {
	switch k {
	case RiskStaged:
		return "staged changes"
	case RiskUnstaged:
		return "unstaged changes"
	case RiskConflicted:
		return "conflicts"
	case RiskUntracked:
		return "untracked files"
	case RiskStash:
		return "stashes"
	case RiskUnpushedBranchCommits:
		return "unpushed commits on local branches"
	case RiskDetachedCommits:
		return "commits only on detached HEAD"
	case RiskTagOnlyCommits:
		return "commits only reachable from local tags"
	case RiskIgnoredFiles:
		return "ignored files"
	case RiskHiddenModifications:
		return "hidden modifications (skip-worktree or assume-unchanged files)"
	case RiskUnverifiable:
		return "files that could not be verified"
	case RiskNestedRepository:
		return "nested repositories"
	case RiskNestedRepositoryClean:
		return "clean, fully pushed nested repositories"
	case RiskForeignWorktree:
		return "worktrees of other repositories"
	}
	return fmt.Sprintf("RiskKind(%d)", int(k))
}

// phrase is String for n entries: the singular form when n is 1.
func (k RiskKind) phrase(n int) string {
	if n != 1 {
		return k.String()
	}
	switch k {
	case RiskStaged:
		return "staged change"
	case RiskUnstaged:
		return "unstaged change"
	case RiskConflicted:
		return "conflict"
	case RiskUntracked:
		return "untracked file"
	case RiskStash:
		return "stash"
	case RiskUnpushedBranchCommits:
		return "unpushed commit on local branches"
	case RiskDetachedCommits:
		return "commit only on detached HEAD"
	case RiskTagOnlyCommits:
		return "commit only reachable from local tags"
	case RiskIgnoredFiles:
		return "ignored file"
	case RiskHiddenModifications:
		return "hidden modification (skip-worktree or assume-unchanged file)"
	case RiskUnverifiable:
		return "file that could not be verified"
	case RiskNestedRepository:
		return "nested repository"
	case RiskNestedRepositoryClean:
		return "clean, fully pushed nested repository"
	case RiskForeignWorktree:
		return "worktree of another repository"
	}
	return k.String()
}

// Risk is one kind of data at risk, with the number of entries or commits.
type Risk struct {
	Kind  RiskKind
	Count int
	// Detail is an optional human-readable explanation: the reason for
	// RiskUnverifiable, the branches carrying the commits for
	// RiskUnpushedBranchCommits, the repositories for the nested kinds;
	// empty for the other kinds.
	Detail string
}

func (r Risk) String() string {
	if r.Detail != "" {
		return fmt.Sprintf("%d %s: %s", r.Count, r.Kind.phrase(r.Count), r.Detail)
	}
	return fmt.Sprintf("%d %s", r.Count, r.Kind.phrase(r.Count))
}

// Informational reports whether the risk should be shown but not block a
// deletion: ignored files (usually build output) and tag-only commits. Tags
// are fetched from origin by default, so a commit reachable only from a tag
// is almost always a release tag that exists on origin, and blocking on it
// would make every fresh clone of such a repository undeletable; telling
// those apart from truly local tags would require asking the remote. The
// cost is that commits only reachable from a tag created locally and never
// pushed do not block.
func (r Risk) Informational() bool {
	return r.Kind == RiskIgnoredFiles || r.Kind == RiskTagOnlyCommits || r.Kind == RiskNestedRepositoryClean
}

// RepositoryWide reports whether the kind concerns the whole repository
// (stashes, unpushed local branches, tag-only commits: lost only when the
// repository, i.e. its last work tree, is deleted) rather than one work tree
// (staged, unstaged, conflicted, untracked, ignored, detached-HEAD commits,
// hidden modifications: lost when that work tree is deleted).
func (k RiskKind) RepositoryWide() bool {
	switch k {
	case RiskStash, RiskUnpushedBranchCommits, RiskTagOnlyCommits:
		return true
	}
	return false
}

// SafeToDelete reports whether risks contains only informational entries.
// Use it before deleting a whole repository (with the risks of each of its
// work trees).
func SafeToDelete(risks []Risk) bool {
	for _, r := range risks {
		if !r.Informational() {
			return false
		}
	}
	return true
}

// SafeToRemoveWorktree reports whether risks contains no blocking
// work-tree-local entry, i.e. whether removing that linked worktree (while
// the repository and its other work trees remain) loses nothing.
// Repository-wide risks are ignored: they survive the removal.
func SafeToRemoveWorktree(risks []Risk) bool {
	for _, r := range risks {
		if !r.Informational() && !r.Kind.RepositoryWide() {
			return false
		}
	}
	return true
}

// DeletionRisks lists what deleting the work tree containing dir, or the
// whole repository, would lose. Only kinds with a non-zero count are
// returned, in RiskKind order. The kinds are documented as work-tree-local
// or repository-wide: removing a linked worktree only needs the former, while
// removing a repository needs every kind, checked in each of its work trees
// (each may have its own detached HEAD and changes).
//
// Commit-based kinds compare with remote-tracking refs as of the last fetch
// (DeletionRisks: those `git fetch --all` writes and prunes, see
// DeletionRisksOptions.TrustFetchAllRemotes); commits on a branch that was
// pushed but whose remote-tracking ref was pruned count as unpushed. Status configuration that hides changes is
// overridden (see Status), and files whose changes are hidden by the
// skip-worktree or assume-unchanged bits are checked by comparing their
// content with the index: the id `git hash-object` computes after clean
// filters and line-ending conversion, or failing that the id of the raw bytes
// (so a CRLF blob that git does not convert again is not a modification). A
// file whose filters cannot run (e.g. a required Git LFS filter that is not
// installed) and whose raw content differs from the index cannot be judged:
// such files are reported as RiskUnverifiable, with the reason in
// Risk.Detail, instead of failing the call.
//
// Nested repositories (submodules, embedded gitlinks, independent clones or
// other repositories' worktrees anywhere in the work tree, see
// nestedRepositories) are inspected recursively, up to maxNestingDepth
// levels: one with data at risk, without any remote, or that cannot be
// inspected is a blocking RiskNestedRepository; a clean, fully pushed one an
// informational RiskNestedRepositoryClean. In a nested repository, only the
// refs written and pruned by the fetches of the remotes `git fetch --all`
// fetches (see BeforeNested) whose URL points outside the deleted tree
// count as pushed, with or without BeforeNested.
//
// Known gaps (not detected): commits only reachable from reflogs, notes or
// other refs (refs/pull/*, refs/replace/*, ...); detached HEADs of other
// worktrees (call DeletionRisks in each); content of .git itself (hooks,
// config, info/exclude); unpushed Git LFS objects; mode-only changes (e.g.
// the executable bit) of skip-worktree or assume-unchanged files. Tags
// fetched from the remote count as local tags, which is why
// RiskTagOnlyCommits is informational.
func DeletionRisks(ctx context.Context, dir string) ([]Risk, error) {
	return DeletionRisksWithOptions(ctx, dir, DeletionRisksOptions{TrustFetchAllRemotes: true})
}

// DeletionRisksOptions configures DeletionRisksWithOptions.
type DeletionRisksOptions struct {
	// BeforeNested, when set, is called with the directory of each nested
	// repository that has at least one remote (its top level, or the
	// repository itself when bare), before that repository is assessed, at
	// every nesting level. Callers use it to fetch (FetchAll: the remotes
	// whose refs count, see DeletionRisks) so that
	// commits whose remote branch was deleted are seen as unpushed:
	// DeletionRisks itself never accesses the network. An error makes the
	// nested repository blocking ("fetch failed, remote state unknown"),
	// except a cancellation of ctx, which aborts DeletionRisksWithOptions.
	BeforeNested func(ctx context.Context, dir string) error
	// DeletedRoot is the directory whose deletion is being assessed
	// (default: the top level of dir). A nested repository whose remotes all
	// point inside it counts as having no remote: deleting the tree deletes
	// those remotes too.
	DeletedRoot string
	// UntrustedRemotes are remotes whose remote-tracking refs do not count
	// as pushed for dir's own repository (not for nested repositories), e.g.
	// a fork that appears to be gone: commits reachable only from them are
	// reported as unpushed or detached.
	UntrustedRemotes []string
	// TrustedRemotes are the remotes whose remote-tracking refs count as
	// pushed for dir's own repository (not for nested repositories): only
	// the refs their fetches write and prune (see TrustedRemoteRefs),
	// except those of UntrustedRemotes. Typically the remotes the caller
	// fetched: the refs of a remote that was not fetched may be stale (a
	// branch deleted there, a remote that no longer exists). Fail safe:
	// with neither TrustedRemotes nor TrustFetchAllRemotes, no
	// remote-tracking ref counts.
	TrustedRemotes []string
	// TrustFetchAllRemotes adds to TrustedRemotes the remotes `git fetch
	// --all` fetches (with a URL, without remote.<name>.skipFetchAll), the
	// rule for nested repositories.
	TrustFetchAllRemotes bool
	// PublishedRefs are refs (full names) whose commits count as pushed for
	// dir's own repository, like remote-tracking refs (not for nested
	// repositories), e.g. recorded pull request heads (see PullHeadRef).
	// Refs that do not exist are ignored.
	PublishedRefs []string
	// ForeignWorktrees reports the nested work trees whose .git file names a
	// git directory outside DeletedRoot (or cannot be read) as
	// RiskForeignWorktree instead of nested repositories, at every nesting
	// level. Meant for deleting a whole repository: a submodule's git
	// directory, for instance, lies outside a linked worktree.
	ForeignWorktrees bool
}

// nestOpts carries the options and the recursion state.
type nestOpts struct {
	before  func(ctx context.Context, dir string) error
	root    string // resolved DeletedRoot
	visited map[string]bool
	// foreign is set with ForeignWorktrees; the foreign work trees found at
	// any level are collected in it ("path (git directory ...)", relative
	// to top) and reported at the top level.
	foreign *[]string
	top     string
}

// DeletionRisksWithOptions is DeletionRisks with options.
func DeletionRisksWithOptions(ctx context.Context, dir string, opts DeletionRisksOptions) ([]Risk, error) {
	top, err := TopLevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	no := &nestOpts{before: opts.BeforeNested, root: resolvePath(top), visited: map[string]bool{resolvePath(top): true}}
	pushed, err := pushedArgs(ctx, dir, opts)
	if err != nil {
		return nil, err
	}
	if opts.ForeignWorktrees {
		no.foreign, no.top = new([]string), top
	}
	if opts.DeletedRoot != "" {
		no.root = resolvePath(opts.DeletedRoot)
	}
	return deletionRisks(ctx, dir, top, maxNestingDepth, no, pushed)
}

// pushedRevs are rev-list arguments for the commits that count as pushed:
// lead goes before the revisions, revs after --not (see pushedArgs).
type pushedRevs struct {
	lead, revs []string
}

// maxNestingDepth bounds the recursion into nested repositories.
const maxNestingDepth = 4

// deletionRisks is DeletionRisks with the top level of the work tree
// containing dir (as reported by TopLevel), the remaining nesting depth,
// the resolved top levels already inspected (cycle safety) and the commits
// that count as pushed in it.
func deletionRisks(ctx context.Context, dir, top string, depth int, no *nestOpts, pushed pushedRevs) ([]Risk, error) {
	st, err := status(ctx, dir, "--ignored=traditional")
	if err != nil {
		return nil, err
	}
	revArgs := func(args ...string) []string {
		return append(append(slices.Clone(pushed.lead), args...), pushed.revs...)
	}
	branchCommits, err := revCount(ctx, dir, revArgs("--branches", "--not")...)
	if err != nil {
		return nil, err
	}
	detachedCommits := 0
	if st.Detached && !st.Unborn {
		if detachedCommits, err = revCount(ctx, dir, revArgs("HEAD", "--not", "--branches", "--tags")...); err != nil {
			return nil, err
		}
	}
	tagCommits, err := revCount(ctx, dir, revArgs("--tags", "--not", "--branches")...)
	if err != nil {
		return nil, err
	}
	hidden, unverifiable, reason, err := hiddenModifications(ctx, top)
	if err != nil {
		return nil, err
	}
	blocking, clean, err := assessNested(ctx, top, depth, no)
	if err != nil {
		return nil, err
	}
	var risks []Risk
	for _, r := range []Risk{
		{Kind: RiskStaged, Count: st.Staged},
		{Kind: RiskUnstaged, Count: st.Unstaged},
		{Kind: RiskConflicted, Count: st.Conflicted},
		{Kind: RiskUntracked, Count: st.Untracked},
		{Kind: RiskStash, Count: st.Stashes},
		{Kind: RiskUnpushedBranchCommits, Count: branchCommits},
		{Kind: RiskDetachedCommits, Count: detachedCommits},
		{Kind: RiskTagOnlyCommits, Count: tagCommits},
		{Kind: RiskIgnoredFiles, Count: st.ignored},
		{Kind: RiskHiddenModifications, Count: hidden},
		{Kind: RiskUnverifiable, Count: unverifiable, Detail: reason},
		{Kind: RiskNestedRepository, Count: len(blocking), Detail: summarizeList(blocking, "; ")},
		{Kind: RiskNestedRepositoryClean, Count: len(clean), Detail: summarizeList(clean, ", ")},
	} {
		if r.Count > 0 {
			risks = append(risks, r)
		}
	}
	if depth == maxNestingDepth && no.foreign != nil && len(*no.foreign) > 0 {
		risks = append(risks, Risk{Kind: RiskForeignWorktree, Count: len(*no.foreign), Detail: summarizeList(*no.foreign, "; ")})
	}
	return risks, nil
}

// pushedArgs returns the rev-list arguments for the commits that count as
// pushed in the repository of dir under opts: the refs written by the
// fetches of the trusted remotes (TrustedRemotes and, with
// TrustFetchAllRemotes, those `git fetch --all` fetches, except
// UntrustedRemotes; see TrustedRemoteRefs), then PublishedRefs, with
// --ignore-missing for exact refs that do not exist. The configuration is
// read (one git call) only when some remote may be trusted.
func pushedArgs(ctx context.Context, dir string, opts DeletionRisksOptions) (pushedRevs, error) {
	var p pushedRevs
	if len(opts.TrustedRemotes) > 0 || opts.TrustFetchAllRemotes {
		conf, err := readRemotes(ctx, dir)
		if err != nil {
			return p, err
		}
		trusted := slices.Clone(opts.TrustedRemotes)
		if opts.TrustFetchAllRemotes {
			trusted = append(trusted, fetchAllRemotes(conf)...)
		}
		trusted = slices.DeleteFunc(trusted, func(r string) bool { return slices.Contains(opts.UntrustedRemotes, r) })
		p.lead, p.revs = refSetOf(conf, trusted).RevListArgs()
	}
	p.revs = append(p.revs, opts.PublishedRefs...)
	if len(opts.PublishedRefs) > 0 {
		p.lead = []string{"--ignore-missing"}
	}
	return p, nil
}

// UnpushedBranches returns, sorted, the local branches of the repository
// containing dir that carry the commits DeletionRisksWithOptions counts as
// RiskUnpushedBranchCommits with the same opts (UntrustedRemotes,
// TrustedRemotes, TrustFetchAllRemotes and PublishedRefs; the other options
// do not matter). A branch carries unpushed commits exactly when its tip is
// unpushed: a pushed tip makes all its ancestors pushed.
func UnpushedBranches(ctx context.Context, dir string, opts DeletionRisksOptions) ([]string, error) {
	p, err := pushedArgs(ctx, dir, opts)
	if err != nil {
		return nil, err
	}
	// --simplify-by-decoration: only the unpushed commits some ref points at.
	args := append(append(append([]string{"rev-list"}, p.lead...), "--simplify-by-decoration", "--branches", "--not"), p.revs...)
	out, err := Run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	tips := map[string]bool{}
	for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		tips[l] = true
	}
	heads, err := Run(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads")
	if err != nil {
		return nil, err
	}
	var names []string
	for l := range strings.SplitSeq(strings.TrimSpace(heads), "\n") {
		oid, ref, ok := strings.Cut(l, " ")
		if ok && tips[oid] {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	slices.Sort(names)
	return names, nil
}

// revCount runs `git rev-list --count args...` and parses the result.
func revCount(ctx context.Context, dir string, args ...string) (int, error) {
	out, err := Run(ctx, dir, append([]string{"rev-list", "--count"}, args...)...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("gitx: rev-list --count in %s: unexpected output %q", dir, out)
	}
	return n, nil
}

// hiddenModifications counts index entries of the work tree whose top level
// is top that are flagged skip-worktree or assume-unchanged and whose file
// exists with a type or content different from the index entry. Files whose
// content could not be compared (a clean filter that fails) are counted
// separately, with the reason of the first failure.
func hiddenModifications(ctx context.Context, top string) (modified, unverifiable int, reason string, err error) {
	// "<tag> <mode> <oid> <stage>\t<path>": tag 'S' is skip-worktree, a
	// lowercase tag is assume-unchanged.
	out, err := Run(ctx, top, "ls-files", "-v", "-s", "-z")
	if err != nil {
		return 0, 0, "", err
	}
	var paths, oids []string
	for rec := range strings.SplitSeq(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || len(f[0]) != 1 {
			return 0, 0, "", fmt.Errorf("gitx: ls-files in %s: unexpected entry %q", top, rec)
		}
		tag, mode, oid, stage := f[0][0], f[1], f[2], f[3]
		if (tag != 'S' && (tag < 'a' || tag > 'z')) || stage != "0" || mode == "160000" {
			continue
		}
		full := filepath.Join(top, filepath.FromSlash(path))
		fi, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue // absent: nothing to lose
		}
		if err != nil {
			return 0, 0, "", fmt.Errorf("gitx: %w", err)
		}
		switch {
		case mode == "120000" && fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return 0, 0, "", fmt.Errorf("gitx: %w", err)
			}
			if blobID(oid, target) != oid {
				modified++
			}
		case mode == "120000" && fi.Mode().IsRegular() && fi.Size() <= maxSymlinkTarget:
			// core.symlinks=false: git checks a symlink out as a regular file
			// holding the link target.
			target, err := os.ReadFile(full)
			if err != nil {
				return 0, 0, "", fmt.Errorf("gitx: %w", err)
			}
			if blobID(oid, string(target)) != oid {
				modified++
			}
		case mode == "120000" || !fi.Mode().IsRegular():
			modified++
		default:
			paths, oids = append(paths, path), append(oids, oid)
		}
	}

	// First pass: as `git status` would hash the file, i.e. through the clean
	// filters, text conversion and ident expansion. `git hash-object` does not
	// consult the index, so it cannot apply the rule "a blob that already has
	// CRLF is not converted again" (text=auto, core.autocrlf): a clean file may
	// hash differently. Second pass, for the files that mismatched or could not
	// be hashed: the raw content; a byte-identical file is unmodified whatever
	// the filters would do.
	ids, errs, err := hashPaths(ctx, top, paths, false)
	if err != nil {
		return 0, 0, "", err
	}
	var retry []int
	for i := range paths {
		if ids[i] != oids[i] {
			retry = append(retry, i)
		}
	}
	retryPaths := make([]string, len(retry))
	for j, i := range retry {
		retryPaths[j] = paths[i]
	}
	rawIDs, _, err := hashPaths(ctx, top, retryPaths, true)
	if err != nil {
		return 0, 0, "", err
	}
	for j, i := range retry {
		switch {
		case rawIDs[j] == oids[i]:
		case errs[i] != nil:
			if unverifiable++; reason == "" {
				reason = failureReason(errs[i])
			}
		default:
			modified++
		}
	}
	return modified, unverifiable, reason, nil
}

// maxSymlinkTarget bounds the size of a regular file read as the content of a
// symlink index entry (PATH_MAX).
const maxSymlinkTarget = 4096

// failureReason condenses a failed git invocation into one line.
func failureReason(err error) string {
	var ge *GitError
	if errors.As(err, &ge) && ge.Stderr != "" {
		var lines []string
		for l := range strings.SplitSeq(ge.Stderr, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		return strings.Join(lines, "; ")
	}
	return err.Error()
}

// hashPathsBatch is the number of paths per command line for paths that
// cannot go through --stdin-paths; it keeps the command line short.
const hashPathsBatch = 256

// stdinUnsafe reports whether path cannot be fed to `git hash-object
// --stdin-paths`, which reads one path per line, strips a trailing CR and
// unquotes a path starting with a double quote.
func stdinUnsafe(path string) bool {
	return strings.ContainsAny(path, "\n\r") || strings.HasPrefix(path, `"`)
}

// hashPaths returns, for each path (relative to top), the blob id that `git
// hash-object` computes (with the filters of the path's attributes unless
// noFilters), or the error of the invocation that failed for that path alone.
// Most paths go through a single `hash-object --stdin-paths` process; the
// others through command-line batches. A failing set is bisected so that one
// failing file does not hide the ids of the others. The returned error is
// non-nil only when hashing as a whole could not run (e.g. ctx is done).
func hashPaths(ctx context.Context, top string, paths []string, noFilters bool) (ids []string, errs []error, err error) {
	h := &hasher{ctx: ctx, top: top, noFilters: noFilters, paths: paths,
		ids: make([]string, len(paths)), errs: make([]error, len(paths))}
	var viaStdin, viaArgs []int
	for i, p := range paths {
		if stdinUnsafe(p) {
			viaArgs = append(viaArgs, i)
		} else {
			viaStdin = append(viaStdin, i)
		}
	}
	if err := h.hash(viaStdin, true); err != nil {
		return nil, nil, err
	}
	for i := 0; i < len(viaArgs); i += hashPathsBatch {
		if err := h.hash(viaArgs[i:min(i+hashPathsBatch, len(viaArgs))], false); err != nil {
			return nil, nil, err
		}
	}
	return h.ids, h.errs, nil
}

type hasher struct {
	ctx       context.Context
	top       string
	noFilters bool
	paths     []string
	ids       []string
	errs      []error
}

// hash hashes paths[idx...] with one process and stores the results; if git
// exits non-zero it retries each half separately.
func (h *hasher) hash(idx []int, stdin bool) error {
	if len(idx) == 0 {
		return nil
	}
	args := []string{"hash-object"}
	if h.noFilters {
		args = append(args, "--no-filters")
	}
	var (
		out string
		err error
	)
	if stdin {
		var in strings.Builder
		for _, i := range idx {
			in.WriteString(h.paths[i])
			in.WriteByte('\n')
		}
		out, err = run(h.ctx, runOpts{stdin: strings.NewReader(in.String())}, h.top, append(args, "--stdin-paths")...)
	} else {
		args = append(args, "--")
		for _, i := range idx {
			args = append(args, h.paths[i])
		}
		out, err = Run(h.ctx, h.top, args...)
	}
	if err == nil {
		got := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(got) != len(idx) {
			return fmt.Errorf("gitx: hash-object in %s: %d ids for %d paths", h.top, len(got), len(idx))
		}
		for k, i := range idx {
			h.ids[i] = got[k]
		}
		return nil
	}
	var ge *GitError
	if h.ctx.Err() != nil || !errors.As(err, &ge) || ge.ExitCode <= 0 {
		return err
	}
	if len(idx) == 1 {
		h.errs[idx[0]] = err
		return nil
	}
	mid := len(idx) / 2
	if err := h.hash(idx[:mid], stdin); err != nil {
		return err
	}
	return h.hash(idx[mid:], stdin)
}

// blobID returns the id of a blob with content, using the hash function of
// the repository whose object id like is (SHA-256 for 64 hex digits, SHA-1
// otherwise).
func blobID(like, content string) string {
	var h hash.Hash = sha1.New()
	if len(like) == 64 {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

// nestedRepo is a repository found inside a work tree.
type nestedRepo struct {
	rel  string // relative to the outer top level
	bare bool
}

// nestedRepositories returns, sorted and relative to top (the top level of
// a work tree), the directories holding another repository: a
// .git directory or file, or a bare repository (HEAD, objects/ and refs/,
// confirmed by git). It looks everywhere a repository can hide:
//
//   - untracked and ignored entries (`git ls-files --others --directory`),
//     walked without following symbolic links (git reports a nested
//     repository there as an untracked directory, or not at all when it is
//     ignored);
//   - gitlinks (mode 160000: submodules and repositories embedded with `git
//     add`) whose directory holds a .git entry;
//   - directories holding tracked files (one Lstat each, plus a few for
//     directories that look like a bare repository), since a repository may
//     have been initialized inside tracked content.
//
// Linked worktrees of the same repository are skipped (callers handle them),
// as are repositories nested inside another reported one (they are found by
// inspecting that one).
func nestedRepositories(ctx context.Context, top string) ([]nestedRepo, error) {
	wts, err := WorktreeList(ctx, top)
	if err != nil {
		return nil, err
	}
	own := map[string]bool{}
	for _, w := range wts {
		own[resolvePath(w.Path)] = true
	}
	found := map[string]bool{} // rel -> bare
	consider := func(p string) bool {
		bare := false
		if _, err := os.Lstat(filepath.Join(p, ".git")); err != nil {
			if !looksBare(p) || !isBareRepo(ctx, p) {
				return false
			}
			bare = true
		}
		if !own[resolvePath(p)] {
			rel, _ := filepath.Rel(top, p)
			found[rel] = bare
		}
		return true
	}

	out, err := Run(ctx, top, "ls-files", "--others", "--directory", "-z")
	if err != nil {
		return nil, err
	}
	for e := range strings.SplitSeq(out, "\x00") {
		if e == "" {
			continue
		}
		root := filepath.Join(top, filepath.FromSlash(strings.TrimSuffix(e, "/")))
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // vanished meanwhile
				}
				return err
			}
			if d.IsDir() && consider(p) {
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("gitx: looking for nested repositories in %s: %w", root, err)
		}
	}

	staged, err := Run(ctx, top, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{}
	for rec := range strings.SplitSeq(staged, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		path = filepath.FromSlash(path)
		if strings.HasPrefix(meta, "160000 ") {
			consider(filepath.Join(top, path))
			continue
		}
		for d := filepath.Dir(path); d != "." && !dirs[d]; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		consider(filepath.Join(top, d))
	}

	var rels []string
	for r := range found {
		rels = append(rels, r)
	}
	slices.Sort(rels)
	// Drop repositories inside another reported one.
	var res []nestedRepo
	for _, r := range rels {
		if n := len(res); n > 0 && strings.HasPrefix(r, res[n-1].rel+string(filepath.Separator)) {
			continue
		}
		res = append(res, nestedRepo{rel: r, bare: found[r]})
	}
	return res, nil
}

// looksBare reports whether p has the layout of a bare repository.
func looksBare(p string) bool {
	if fi, err := os.Lstat(filepath.Join(p, "HEAD")); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	for _, d := range []string{"objects", "refs"} {
		if fi, err := os.Lstat(filepath.Join(p, d)); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// isBareRepo confirms with git that p is a bare repository: run from p, git
// discovers p itself as the repository only if it is a valid git directory
// (otherwise it finds the enclosing repository, which is not bare).
func isBareRepo(ctx context.Context, p string) bool {
	out, err := Run(ctx, p, "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(out) != "true" {
		return false
	}
	gd, err := Run(ctx, p, "rev-parse", "--absolute-git-dir")
	return err == nil && resolvePath(strings.TrimSpace(gd)) == resolvePath(p)
}

// assessNested inspects each nested repository of the work tree whose top
// level is top (see nestedRepositories) and returns the blocking ones ("path
// (reasons)") and the clean ones (paths).
func assessNested(ctx context.Context, top string, depth int, no *nestOpts) (blocking, clean []string, err error) {
	repos, err := nestedRepositories(ctx, top)
	if err != nil {
		return nil, nil, err
	}
	for _, nr := range repos {
		p := filepath.Join(top, nr.rel)
		rp := resolvePath(p)
		if no.visited[rp] {
			continue
		}
		no.visited[rp] = true
		if no.foreign != nil && !nr.bare {
			if fi, err := os.Lstat(filepath.Join(p, ".git")); err == nil && fi.Mode().IsRegular() {
				gd, gerr := GitFileDir(p)
				rel, _ := filepath.Rel(no.top, p)
				switch {
				case gerr != nil:
					*no.foreign = append(*no.foreign, rel+" (unreadable .git file: "+gerr.Error()+")")
					continue
				case !within(no.root, resolvePath(gd)):
					*no.foreign = append(*no.foreign, rel+" (git directory "+gd+")")
					continue
				}
			}
		}
		reasons, err := nestedReasons(ctx, p, rp, nr.bare, depth, no)
		if err != nil {
			return nil, nil, err
		}
		if len(reasons) > 0 {
			blocking = append(blocking, nr.rel+" ("+strings.Join(reasons, ", ")+")")
		} else {
			clean = append(clean, nr.rel)
		}
	}
	return blocking, clean, nil
}

// nestedReasons lists why deleting the nested repository at p would lose
// data; empty means nothing would be lost. Only ctx errors are returned as
// errors: anything that prevents the inspection is itself a reason.
func nestedReasons(ctx context.Context, p, resolved string, bare bool, depth int, no *nestOpts) ([]string, error) {
	if depth <= 0 {
		return []string{"nested too deeply to inspect"}, nil
	}
	top := ""
	if !bare {
		var err error
		top, err = TopLevel(ctx, p)
		if err != nil || resolvePath(top) != resolved {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return []string{"not a valid repository, cannot inspect"}, nil
		}
	}
	var reasons []string
	conf, err := readRemotes(ctx, p)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []string{"cannot inspect remotes: " + failureReason(err)}, nil
	}
	total, outside, trusted := remoteLocations(conf, p, no.root)
	pushed := pushedRevs{}
	pushed.lead, pushed.revs = refSetOf(conf, trusted).RevListArgs()
	switch {
	case bare && outside == 0:
		reasons = append(reasons, "bare repository, no remote outside the deleted tree")
	case total == 0:
		reasons = append(reasons, "no remote")
	case outside == 0:
		reasons = append(reasons, "its remotes are all inside the deleted tree")
	}
	if total > 0 && no.before != nil {
		if err := no.before(ctx, p); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			reasons = append(reasons, "fetch failed, remote state unknown: "+failureReason(err))
		}
	}
	if bare {
		if outside > 0 {
			n, err := revCount(ctx, p, append(append(slices.Clone(pushed.lead), "--branches", "--not"), pushed.revs...)...)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return append(reasons, "cannot inspect: "+failureReason(err)), nil
			}
			if n > 0 {
				reasons = append(reasons, fmt.Sprintf("bare repository with %d commits on no remote-tracking ref", n))
			}
		}
		return reasons, nil
	}
	risks, err := deletionRisks(ctx, p, top, depth-1, no, pushed)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return append(reasons, "cannot inspect: "+failureReason(err)), nil
	}
	for _, r := range risks {
		if !r.Informational() {
			reasons = append(reasons, r.String())
		}
	}
	return reasons, nil
}

// remoteLocations counts the remotes (URLs) of the repository at p, with
// configuration conf, and those that do not point inside root (resolved).
// Local URLs (absolute or relative paths, file://) are resolved against p;
// anything else (scp-like, ssh, https, ...) is outside. trusted names the
// remotes whose refs count as pushed: those `git fetch --all` fetches (see
// fetchAllRemotes) whose URL (the first one, which fetch uses) is outside
// root.
func remoteLocations(conf map[string]*remoteConf, p, root string) (total, outside int, trusted []string) {
	isOutside := func(u string) bool {
		local, isLocal := localRemotePath(p, u)
		if !isLocal {
			return true
		}
		rel, err := filepath.Rel(root, resolvePath(local))
		return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, c := range conf {
		for _, u := range c.urls {
			if u == "" {
				continue
			}
			total++
			if isOutside(u) {
				outside++
			}
		}
	}
	for _, name := range fetchAllRemotes(conf) {
		if u := conf[name].urls[0]; u != "" && isOutside(u) {
			trusted = append(trusted, name)
		}
	}
	return total, outside, trusted
}

// localRemotePath returns the filesystem path of a local remote URL.
func localRemotePath(base, u string) (string, bool) {
	switch {
	case strings.HasPrefix(u, "file://"):
		pu, err := url.Parse(u)
		if err != nil {
			return "", false
		}
		return pu.Path, true
	case strings.Contains(u, "://"):
		return "", false
	case filepath.IsAbs(u):
		return u, true
	}
	if i := strings.IndexByte(u, ':'); i >= 0 && !strings.Contains(u[:i], "/") {
		return "", false // scp-like host:path
	}
	return filepath.Join(base, u), true
}

// GitFileDir returns the git directory named by the .git file of dir
// ("gitdir: <path>", relative to dir unless absolute), cleaned but not
// resolved.
func GitFileDir(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", err
	}
	gd, ok := strings.CutPrefix(strings.TrimRight(string(b), "\r\n"), "gitdir: ")
	if !ok || gd == "" {
		return "", errors.New("no gitdir line")
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(dir, gd)
	}
	return filepath.Clean(gd), nil
}

// within reports whether p is root or below it (both resolved).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// summarizeList joins up to five entries for Risk.Detail.
func summarizeList(entries []string, sep string) string {
	const maxListed = 5
	if len(entries) <= maxListed {
		return strings.Join(entries, sep)
	}
	return fmt.Sprintf("%s and %d more", strings.Join(entries[:maxListed], sep), len(entries)-maxListed)
}

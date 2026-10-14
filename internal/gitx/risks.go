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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// RiskKind is a category of data that deleting a work tree or repository
// would lose.
type RiskKind int

const (
	// Work-tree-local kinds (lost when the work tree is deleted).
	RiskStaged     RiskKind = iota + 1 // entries with staged changes
	RiskUnstaged                       // entries with unstaged changes (incl. submodules with new commits or modified content)
	RiskConflicted                     // unmerged entries
	RiskUntracked                      // untracked entries (incl. submodules with only untracked content)
	// Repository-wide kinds (lost when the repository, i.e. its last work
	// tree, is deleted; irrelevant to removing a linked worktree).
	RiskStash                 // stash entries
	RiskUnpushedBranchCommits // commits on local branches not on any remote-tracking ref
	// RiskDetachedCommits: commits reachable from this work tree's detached
	// HEAD but from no branch, tag or remote-tracking ref (work-tree-local).
	RiskDetachedCommits
	// RiskTagOnlyCommits: commits reachable only from tags (neither from a
	// local branch nor from a remote-tracking ref); repository-wide and
	// informational (see Risk.Informational).
	RiskTagOnlyCommits
	// RiskIgnoredFiles: ignored entries; informational (usually build
	// output), see Risk.Informational.
	RiskIgnoredFiles
	// RiskHiddenModifications: tracked files flagged skip-worktree or
	// assume-unchanged (`git update-index`) whose work tree content differs
	// from the index. Status does not show them; work-tree-local, blocking.
	// Flagged files absent from the work tree (e.g. outside a sparse
	// checkout) do not count; a symlink entry checked out as a regular file
	// (core.symlinks=false) holding its target does not either.
	RiskHiddenModifications
	// RiskUnverifiable: skip-worktree or assume-unchanged files whose content
	// could not be compared with the index (e.g. a required clean filter such
	// as Git LFS is not installed); Risk.Detail says why. Work-tree-local,
	// blocking: the files may hold modifications.
	RiskUnverifiable
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
	}
	return fmt.Sprintf("RiskKind(%d)", int(k))
}

// Risk is one kind of data at risk, with the number of entries or commits.
type Risk struct {
	Kind  RiskKind
	Count int
	// Detail is an optional human-readable explanation (the reason for
	// RiskUnverifiable); empty for the other kinds.
	Detail string
}

func (r Risk) String() string {
	if r.Detail != "" {
		return fmt.Sprintf("%d %v: %s", r.Count, r.Kind, r.Detail)
	}
	return fmt.Sprintf("%d %v", r.Count, r.Kind)
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
	return r.Kind == RiskIgnoredFiles || r.Kind == RiskTagOnlyCommits
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
// Commit-based kinds compare with remote-tracking refs as of the last fetch;
// commits on a branch that was pushed but whose remote-tracking ref was
// pruned count as unpushed. Status configuration that hides changes is
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
// Known gaps (not detected): unpushed commits, stashes or changes inside
// submodules' own repositories (only the submodule's state as seen from the
// superproject is reported); commits only reachable from reflogs, notes or
// other refs (refs/pull/*, refs/replace/*, ...); detached HEADs of other
// worktrees (call DeletionRisks in each); content of .git itself (hooks,
// config, info/exclude); unpushed Git LFS objects; mode-only changes (e.g.
// the executable bit) of skip-worktree or assume-unchanged files. Tags
// fetched from the remote count as local tags, which is why
// RiskTagOnlyCommits is informational.
func DeletionRisks(ctx context.Context, dir string) ([]Risk, error) {
	st, err := status(ctx, dir, "--ignored=traditional")
	if err != nil {
		return nil, err
	}
	branchCommits, err := revCount(ctx, dir, "--branches", "--not", "--remotes")
	if err != nil {
		return nil, err
	}
	detachedCommits := 0
	if st.Detached && !st.Unborn {
		if detachedCommits, err = revCount(ctx, dir, "HEAD", "--not", "--branches", "--tags", "--remotes"); err != nil {
			return nil, err
		}
	}
	tagCommits, err := revCount(ctx, dir, "--tags", "--not", "--branches", "--remotes")
	if err != nil {
		return nil, err
	}
	hidden, unverifiable, reason, err := hiddenModifications(ctx, dir)
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
	} {
		if r.Count > 0 {
			risks = append(risks, r)
		}
	}
	return risks, nil
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

// hiddenModifications counts index entries of the work tree containing dir
// that are flagged skip-worktree or assume-unchanged and whose file exists
// with a type or content different from the index entry. Files whose content
// could not be compared (a clean filter that fails) are counted separately,
// with the reason of the first failure.
func hiddenModifications(ctx context.Context, dir string) (modified, unverifiable int, reason string, err error) {
	top, err := TopLevel(ctx, dir)
	if err != nil {
		return 0, 0, "", err
	}
	// "<tag> <mode> <oid> <stage>\t<path>": tag 'S' is skip-worktree, a
	// lowercase tag is assume-unchanged.
	out, err := Run(ctx, top, "ls-files", "-v", "-s", "-z")
	if err != nil {
		return 0, 0, "", err
	}
	var paths, oids []string
	for _, rec := range strings.Split(out, "\x00") {
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

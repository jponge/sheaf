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
	// checkout) do not count.
	RiskHiddenModifications
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
	}
	return fmt.Sprintf("RiskKind(%d)", int(k))
}

// Risk is one kind of data at risk, with the number of entries or commits.
type Risk struct {
	Kind  RiskKind
	Count int
}

func (r Risk) String() string { return fmt.Sprintf("%d %v", r.Count, r.Kind) }

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
// content (after clean filters, as `git hash-object` computes it) with the
// index.
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
	hidden, err := hiddenModifications(ctx, dir)
	if err != nil {
		return nil, err
	}
	var risks []Risk
	for _, r := range []Risk{
		{RiskStaged, st.Staged},
		{RiskUnstaged, st.Unstaged},
		{RiskConflicted, st.Conflicted},
		{RiskUntracked, st.Untracked},
		{RiskStash, st.Stashes},
		{RiskUnpushedBranchCommits, branchCommits},
		{RiskDetachedCommits, detachedCommits},
		{RiskTagOnlyCommits, tagCommits},
		{RiskIgnoredFiles, st.ignored},
		{RiskHiddenModifications, hidden},
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
// with a type or content different from the index entry.
func hiddenModifications(ctx context.Context, dir string) (int, error) {
	top, err := TopLevel(ctx, dir)
	if err != nil {
		return 0, err
	}
	// "<tag> <mode> <oid> <stage>\t<path>": tag 'S' is skip-worktree, a
	// lowercase tag is assume-unchanged.
	out, err := Run(ctx, top, "ls-files", "-v", "-s", "-z")
	if err != nil {
		return 0, err
	}
	count := 0
	var paths, oids []string
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 || len(f[0]) != 1 {
			return 0, fmt.Errorf("gitx: ls-files in %s: unexpected entry %q", top, rec)
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
			return 0, fmt.Errorf("gitx: %w", err)
		}
		switch {
		case mode == "120000":
			if fi.Mode()&fs.ModeSymlink == 0 {
				count++
				continue
			}
			target, err := os.Readlink(full)
			if err != nil {
				return 0, fmt.Errorf("gitx: %w", err)
			}
			if blobID(oid, target) != oid {
				count++
			}
		case !fi.Mode().IsRegular():
			count++
		default:
			paths, oids = append(paths, path), append(oids, oid)
		}
	}
	const batch = 256 // keeps the command line short
	for i := 0; i < len(paths); i += batch {
		j := min(i+batch, len(paths))
		out, err := Run(ctx, top, append([]string{"hash-object", "--"}, paths[i:j]...)...)
		if err != nil {
			return 0, err
		}
		got := strings.Fields(out)
		if len(got) != j-i {
			return 0, fmt.Errorf("gitx: hash-object in %s: %d ids for %d paths", top, len(got), j-i)
		}
		for k, id := range got {
			if id != oids[i+k] {
				count++
			}
		}
	}
	return count, nil
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

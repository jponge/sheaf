package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// TagMarkerFile is the file, in the administrative directory of a linked
// worktree (<common dir>/worktrees/<id>/), that records the tag a detached
// worktree created by WorktreeAddTag is pinned to: the tag name on the first
// line, the commit it pointed to on the second (informational).
//
// Why a marker rather than deriving the tag from the commit (`git describe
// --exact-match`, `for-each-ref --points-at`): several tags may point at the
// same commit, a tag may be moved or deleted after the worktree was created,
// and a detached worktree made by hand at a tagged commit is not meant to be
// pinned. The administrative directory belongs to the worktree: git moves it
// along with `git worktree move`/`repair` and deletes it with `git worktree
// remove`/`prune`, and reading it needs neither the network nor a git
// process. Git ignores files it does not know there.
const TagMarkerFile = "sheaf-tag"

// ErrTagNotFound is returned by WorktreeAddTag when refs/tags/<tag> does not
// exist.
var ErrTagNotFound = errors.New("gitx: tag not found")

func fetchTags(ctx context.Context, dir, remote string) (kept []string, err error) {
	// Without --quiet: the refusals are only listed in the ref update
	// summary, which fetch.output=full keeps parseable (compact shows "*"
	// instead of the local name).
	_, err = run(ctx, netOpts(ctx), dir, "-c", "fetch.output=full", "fetch", "--no-prune", "--tags", remote)
	if err == nil {
		return nil, nil
	}
	var ge *GitError
	if ctx.Err() != nil || !errors.As(err, &ge) || ge.ExitCode != 1 {
		return nil, err
	}
	if kept = keptTags(ge.Stderr); kept == nil {
		return nil, err
	}
	return kept, nil
}

// keptTags parses the stderr of a failed `git fetch` (fetch.output=full,
// LC_ALL=C) and returns the local tags git refused to clobber, or nil when
// stderr reports any other failure (or none of these). Server messages
// ("remote: ...") and ssh's "Warning: Permanently added ..." are ignored;
// error:, hint:, fatal: and any unknown line count as failures. The format
// was verified against git 2.47.3 only; an unknown format makes FetchTagsFrom
// fail rather than succeed.
func keptTags(stderr string) []string {
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 0 || strings.HasPrefix(line, "From "),
			strings.HasPrefix(line, "remote:"),                    // server messages
			strings.HasPrefix(line, "Warning: Permanently added"): // ssh known_hosts notice
		case f[0] != "!":
			if strings.HasPrefix(line, " ") { // ref updates: " * [new tag] ...", "   a..b  main -> origin/main"
				continue
			}
			return nil // error:, fatal:, hints and anything unknown
		case len(f) == 9 && f[1] == "[rejected]" && f[3] == "->" && strings.Join(f[5:], " ") == "(would clobber existing tag)":
			kept = append(kept, f[4])
		default:
			return nil
		}
	}
	slices.Sort(kept)
	return kept
}

// TagCommit returns the commit refs/tags/<tag> points to (peeling annotated
// tags), ErrTagNotFound when there is no such tag, or an error when it does
// not point to a commit.
func TagCommit(ctx context.Context, dir, tag string) (string, error) {
	ref := "refs/tags/" + tag
	ok, err := refExists(ctx, dir, ref)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTagNotFound, tag)
	}
	out, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", fmt.Errorf("gitx: tag %s does not point to a commit", tag)
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// WorktreeAddTag creates a worktree at path (relative paths are relative to
// repoDir) with a detached HEAD at the commit of refs/tags/<tag>, and records
// the tag in its TagMarkerFile. It returns that commit. path must be absent
// or an empty directory (ErrWorktreePathOccupied). It never accesses the
// network: callers wanting origin's tags should FetchTagsFrom first. If the
// marker cannot be written, the fresh worktree is removed again.
func WorktreeAddTag(ctx context.Context, repoDir, path, tag string) (string, error) {
	if tag == "" {
		return "", errors.New("gitx: worktree add: empty tag name")
	}
	if path == "" {
		return "", errors.New("gitx: worktree add: empty path")
	}
	if err := checkWorktreeTarget(repoDir, path); err != nil {
		return "", err
	}
	oid, err := TagCommit(ctx, repoDir, tag)
	if err != nil {
		return "", err
	}
	if _, err := Run(ctx, repoDir, "worktree", "add", "--quiet", "--detach", "--", path, oid); err != nil {
		return "", err
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(repoDir, abs)
	}
	if err := writeTagMarker(ctx, abs, tag, oid); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rereadTimeout)
		defer cancel()
		_ = WorktreeRemove(cctx, repoDir, abs, true) // nothing of the user's is in it yet
		return "", fmt.Errorf("gitx: recording tag %s for worktree %s: %w", tag, abs, err)
	}
	return oid, nil
}

// writeTagMarker writes the TagMarkerFile of the worktree at wtPath,
// atomically.
func writeTagMarker(ctx context.Context, wtPath, tag, oid string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rereadTimeout)
	defer cancel()
	out, err := Run(cctx, wtPath, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	admin := strings.TrimSpace(out)
	tmp, err := os.CreateTemp(admin, TagMarkerFile+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(tag + "\n" + oid + "\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), filepath.Join(admin, TagMarkerFile))
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// fillWorktreeTags sets Worktree.Tag on the detached linked worktrees of wts
// (as listed by WorktreeList, main worktree first) from their marker files.
// It reads <common dir>/worktrees/*/{gitdir,TagMarkerFile} directly and
// matches each administrative directory to its worktree by the path recorded
// in gitdir, so it also works for worktrees whose directory is missing.
// Unreadable entries are ignored: a worktree then shows as merely detached,
// which every caller treats at least as cautiously.
func fillWorktreeTags(wts []Worktree) {
	found := false
	for _, w := range wts[min(1, len(wts)):] {
		if w.Detached {
			found = true
		}
	}
	if !found {
		return
	}
	common := commonDirOfMain(wts[0])
	if common == "" {
		return
	}
	adminRoot := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(adminRoot)
	if err != nil {
		return
	}
	type pin struct{ tag, oid string }
	tags := map[string]pin{} // worktree path -> tag and recorded commit
	for _, e := range entries {
		admin := filepath.Join(adminRoot, e.Name())
		raw, err := os.ReadFile(filepath.Join(admin, TagMarkerFile))
		if err != nil {
			continue
		}
		tag, rest, _ := strings.Cut(string(raw), "\n")
		tag = strings.TrimSpace(tag)
		oid, _, _ := strings.Cut(rest, "\n")
		if tag == "" {
			continue
		}
		gd, err := os.ReadFile(filepath.Join(admin, "gitdir"))
		if err != nil {
			continue
		}
		p := strings.TrimSpace(string(gd))
		if !filepath.IsAbs(p) {
			p = filepath.Join(admin, p)
		}
		p = filepath.Dir(filepath.Clean(p)) // strip "/.git"
		pn := pin{tag, strings.TrimSpace(oid)}
		tags[p] = pn
		if r, err := filepath.EvalSymlinks(p); err == nil {
			tags[r] = pn
		}
	}
	for i := 1; i < len(wts); i++ {
		w := &wts[i]
		if !w.Detached {
			continue
		}
		p := filepath.Clean(w.Path)
		pn, ok := tags[p]
		if !ok {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				pn, ok = tags[r]
			}
		}
		if ok {
			w.Tag = pn.tag
			w.TagMoved = pn.oid != "" && w.Head != pn.oid
		}
	}
}

// commonDirOfMain returns the common git directory of the repository whose
// main worktree (or bare repository) is main, without running git: the
// bare repository itself, <main>/.git when it is a directory, or the
// directory a <main>/.git file points to. Empty when it cannot be read.
func commonDirOfMain(main Worktree) string {
	if main.Bare {
		return main.Path
	}
	dotgit := filepath.Join(main.Path, ".git")
	fi, err := os.Stat(dotgit)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		return dotgit
	}
	raw, err := os.ReadFile(dotgit)
	if err != nil {
		return ""
	}
	p, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(main.Path, p)
	}
	return p
}

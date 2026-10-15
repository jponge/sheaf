package ops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// AddTagWorktree creates a worktree with a detached HEAD at the commit of
// tag, pinned to it (see gitx.TagMarkerFile), at
// <root>/<repo>/<NormalizeBranch(tag)>: the same directory namespace as the
// branch worktrees.
//
// It validates the tag name (a valid ref component, not starting with '-'),
// fetches origin with its tags unless opts.NoFetch (gitx.FetchTags, which
// never deletes local tags; a failed fetch is an error), and refuses a tag
// that already has a worktree and a directory name taken, case-insensitively,
// by any checkout (primary, branch or tag worktree) or by another entry. The
// tag may be annotated or lightweight; it must point to a commit. opts.Base
// does not apply and is refused.
func AddTagWorktree(ctx context.Context, w *Workspace, repo, tag string, opts WorktreeAddOptions, emit func(Event)) (WorktreeAddResult, error) {
	b := w.Bundle()
	r, ok := lookupRepo(b, repo)
	if !ok {
		return WorktreeAddResult{}, fmt.Errorf("no repository named %q in the bundle (%s)", repo, repoList(b))
	}
	if opts.Base != "" {
		return WorktreeAddResult{}, errors.New("--base does not apply to a tag worktree")
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
	if err := checkTagName(ctx, primary, tag); err != nil {
		return WorktreeAddResult{}, err
	}
	dir, err := bundle.NormalizeBranch(tag)
	if err != nil {
		return WorktreeAddResult{}, fmt.Errorf("tag %q cannot be mapped to a directory name", tag)
	}
	// Cheap checks before the fetch; repeated under the lock below.
	infos, repoDir, _, err := listRepo(ctx, w.Root, r)
	if err != nil {
		return WorktreeAddResult{}, fmt.Errorf("%s: %w", r.Name, err)
	}
	if err := checkTagTarget(infos, repoDir, tag, dir); err != nil {
		return WorktreeAddResult{}, err
	}
	var warnings []string
	if !opts.NoFetch {
		report(PhaseFetch, "fetching origin and its tags")
		kept, err := gitx.FetchTags(ctx, primary)
		if err != nil {
			if ctx.Err() != nil {
				return WorktreeAddResult{}, ctx.Err()
			}
			return WorktreeAddResult{}, fmt.Errorf("fetching %s failed: %w\npass --no-fetch to create the worktree from the local tags of the repository", r.Name, err)
		}
		warnings = keptTagWarnings(kept, tag)
	}
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
	if err := checkTagTarget(infos, repoDir, tag, dir); err != nil {
		return WorktreeAddResult{}, err
	}
	path := filepath.Join(repoDir, dir)
	ev := func(msg string) {
		emit(Event{Repo: r.Name, Worktree: dir, Phase: PhaseWorktree, Kind: KindProgress, Message: msg})
	}
	ev("creating worktree " + path + " at tag " + tag)
	oid, err := gitx.WorktreeAddTag(ctx, primary, path, tag)
	if err != nil {
		if ctx.Err() != nil {
			return WorktreeAddResult{}, ctx.Err()
		}
		if errors.Is(err, gitx.ErrTagNotFound) {
			hint := "it is not on origin either"
			if opts.NoFetch {
				hint = "origin's tags were not fetched (--no-fetch)"
			}
			return WorktreeAddResult{}, fmt.Errorf("no tag %q in %s (%s)", tag, r.Name, hint)
		}
		return WorktreeAddResult{}, fmt.Errorf("creating worktree %s: %w", path, err)
	}
	ev(fmt.Sprintf("checked out tag %s (commit %s) on a detached HEAD", tag, coShortID(oid)))
	for _, wmsg := range warnings {
		ev("warning: " + wmsg)
	}
	return WorktreeAddResult{Repo: r.Name, Dir: dir, Path: path, Tag: tag, Head: oid, Warnings: warnings}, nil
}

// keptTagWarnings describes the local tags that gitx.FetchTags kept because
// origin has a different tag of the same name; the requested tag gets its
// own warning, since the worktree is then made from the local one.
func keptTagWarnings(kept []string, tag string) []string {
	var out, others []string
	for _, k := range kept {
		if k == tag {
			out = append(out, fmt.Sprintf("local tag %s differs from origin's and was kept: the worktree is at the local tag (git fetch origin +refs/tags/%s:refs/tags/%s takes origin's)", tag, tag, tag))
		} else {
			others = append(others, k)
		}
	}
	switch len(others) {
	case 0:
	case 1:
		out = append(out, fmt.Sprintf("local tag %s differs from origin's and was kept", others[0]))
	default:
		out = append(out, fmt.Sprintf("local tags %s differ from origin's and were kept", strings.Join(others, ", ")))
	}
	return out
}

// checkTagName accepts tag names that are valid ref components (git
// check-ref-format refs/tags/<tag>), do not start with '-' (git tag refuses
// those) and do not start like a ref (refs/..., tags/), which is refused with
// a hint to drop the prefix.
func checkTagName(ctx context.Context, dir, tag string) error {
	if tag == "" || strings.HasPrefix(tag, "-") {
		return fmt.Errorf("%q is not a valid tag name", tag)
	}
	if p := refLikeTagPrefix(tag); p != "" {
		return fmt.Errorf("tag %q: drop the %q prefix (--tag %s)", tag, p, strings.TrimPrefix(tag, p))
	}
	if _, err := gitx.Run(ctx, dir, "check-ref-format", "refs/tags/"+tag); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%q is not a valid tag name", tag)
	}
	return nil
}

// checkTagTarget refuses a tag worktree when the tag already has one, or
// when its directory name dir is taken (case-insensitively) by a checkout or
// by an entry that is not a worktree.
func checkTagTarget(infos []WorktreeInfo, repoDir, tag, dir string) error {
	known := map[string]bool{}
	for _, in := range infos {
		if in.Tag == tag {
			return fmt.Errorf("tag %s already has a worktree at %s", tag, in.Path)
		}
		if in.Outside {
			continue
		}
		known[in.Dir] = true
		if !strings.EqualFold(in.Dir, dir) {
			continue
		}
		if in.Primary {
			return fmt.Errorf("directory %s is the primary checkout of the repository", in.Path)
		}
		return fmt.Errorf("directory %s is used by the worktree %s", in.Path, describe(in))
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

// wouldCreateBranch reports whether name is neither a local branch nor a
// branch of origin, i.e. whether AddWorktree would create a new branch.
func wouldCreateBranch(ctx context.Context, dir, name string) (bool, error) {
	for _, ref := range []string{"refs/heads/" + name, "refs/remotes/origin/" + name} {
		ok, err := refExists(ctx, dir, ref)
		if err != nil || ok {
			return false, err
		}
	}
	return true, nil
}

// checkNotOnlyATag refuses a branch worktree for name when name is a tag but
// neither a local branch nor a branch of origin: AddWorktree would otherwise
// create a new branch named like the tag, which is rarely what was meant.
func checkNotOnlyATag(ctx context.Context, dir, name string) error {
	if newBranch, err := wouldCreateBranch(ctx, dir, name); err != nil || !newBranch {
		return err
	}
	ok, err := refExists(ctx, dir, "refs/tags/"+name)
	if err != nil || !ok {
		return err
	}
	return fmt.Errorf("%s is a tag; use --tag %s for a detached worktree at that tag", name, name)
}

// maxBaseSuggestions bounds the suggestions of baseNotFoundError.
const maxBaseSuggestions = 5

// baseNotFoundError describes a start point that does not resolve in the
// repository, with whether origin was fetched and up to maxBaseSuggestions
// remote branches, local branches and tags whose names match base
// case-insensitively (see closestRefs).
func baseNotFoundError(ctx context.Context, dir, repo, base string, noFetch bool) error {
	msg := fmt.Sprintf("base %s not found in %s (after fetching origin)", base, repo)
	if noFetch {
		msg = fmt.Sprintf("base %s not found in %s (not fetched: --no-fetch)", base, repo)
	}
	out, err := gitx.Run(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/", "refs/heads/", "refs/tags/")
	if err != nil {
		return errors.New(msg)
	}
	if s := closestRefs(base, strings.Split(out, "\n")); len(s) > 0 {
		msg += "; closest: " + strings.Join(s, ", ")
	}
	return errors.New(msg)
}

// closestRefs returns, as usable start points ("origin/<branch>", "<branch>
// (local branch)", "<tag> (tag)"), the remote branches, local branches and
// tags among refs (full ref names) whose
// short names match query (base without an origin/, tags/ or refs/...
// prefix), ignoring case, best first: equal, prefixed by the query,
// containing it, contained in it (two characters at least), sharing a prefix
// of half the query at least. Ties go to the name closest in length, then
// remote branches, local branches, tags, then by name. At most maxBaseSuggestions.
func closestRefs(query string, refs []string) []string {
	q := strings.ToLower(query)
	for _, p := range []string{"refs/remotes/origin/", "refs/heads/", "refs/tags/", "origin/", "tags/"} {
		if strings.HasPrefix(q, p) {
			q = q[len(p):]
			break
		}
	}
	if q == "" {
		return nil
	}
	type cand struct {
		show       string
		rank, dist int
		kind       int // 0 remote branch, 1 local branch, 2 tag
		name       string
	}
	var cands []cand
	for _, ref := range refs {
		var name, show string
		kind := 0
		if b, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok && b != "HEAD" {
			name, show = b, "origin/"+b
		} else if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			name, show, kind = b, b+" (local branch)", 1
		} else if t, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
			name, show, kind = t, t+" (tag)", 2
		} else {
			continue
		}
		c := strings.ToLower(name)
		rank := -1
		switch {
		case c == q:
			rank = 0
		case strings.HasPrefix(c, q):
			rank = 1
		case strings.Contains(c, q):
			rank = 2
		case len(c) >= 2 && strings.Contains(q, c):
			rank = 3
		case commonPrefixLen(c, q) >= max(2, (len(q)+1)/2):
			rank = 4
		}
		if rank < 0 {
			continue
		}
		dist := len(c) - len(q)
		if dist < 0 {
			dist = -dist
		}
		cands = append(cands, cand{show: show, rank: rank, dist: dist, kind: kind, name: name})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.dist, b.dist), cmp.Compare(a.kind, b.kind), strings.Compare(a.name, b.name))
	})
	var out []string
	for _, c := range cands[:min(len(cands), maxBaseSuggestions)] {
		out = append(out, c.show)
	}
	return out
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// refLikeTagPrefix returns the ref-like prefix of a --tag value: "refs/tags/",
// "tags/" or "refs/<kind>/"; "" when there is none.
func refLikeTagPrefix(tag string) string {
	switch {
	case strings.HasPrefix(tag, "refs/tags/"):
		return "refs/tags/"
	case strings.HasPrefix(tag, "tags/"):
		return "tags/"
	case strings.HasPrefix(tag, "refs/"):
		if kind, _, ok := strings.Cut(tag[len("refs/"):], "/"); ok {
			return "refs/" + kind + "/"
		}
		return "refs/"
	}
	return ""
}

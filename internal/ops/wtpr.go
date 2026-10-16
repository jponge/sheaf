package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
)

// PhasePR is the phase of the events of AddPRWorktree.
const PhasePR = "pr"

// PRBranch is the local branch of pull request n when it is checked out from
// refs/pull/<n>/head: "pr/<n>" (worktree directory "pr-<n>").
func PRBranch(n int) string { return "pr/" + strconv.Itoa(n) }

// PRWorktreeResult is the outcome of AddPRWorktree.
type PRWorktreeResult struct {
	Repo   string
	Number int
	Branch string
	Dir    string
	Path   string
	// FromPullRef is true when the branch was fetched from
	// refs/pull/<n>/head (pull request from a fork, or head branch deleted).
	FromPullRef bool
	// Created is false when the worktree already existed.
	Created bool
	// Message summarizes what was done.
	Message  string
	Warnings []string
}

// AddPRWorktree checks out pull request number of repo (a GitHub repository
// of the bundle) in a worktree, resolving its head with h.PR, after a fetch
// of origin. For a repository with a fork (see RemotesOf), the pull request
// is resolved against the repository (upstream): a head branch of the
// repository is tracked from upstream, a head branch of the user's fork
// (r.Fork) from origin, and pull refs are fetched from upstream.
//
// A pull request from a branch of the repository itself, whose branch still
// exists on origin, is checked out like `sheaf wt add <head branch>` (local
// branch tracking origin/<head branch>); when that branch is already checked
// out, its checkout is returned as is.
//
// Otherwise (pull request from a fork, possibly deleted, or head branch
// deleted) refs/pull/<n>/head is fetched into the local branch pr/<n> (see
// PRBranch) without forcing, the branch gets branch.pr/<n>.remote=origin and
// branch.pr/<n>.merge=refs/pull/<n>/head (so `git pull` in the worktree
// refreshes it; no fetch refspec is added to the repository), and a worktree
// is created at <repo>/pr-<n>. An existing pr/<n> that tracks something else
// is refused. When pr/<n> is already checked out, AddPRWorktree refreshes it
// instead: the worktree must have no uncommitted changes, and it is only
// fast-forwarded to the fetched head (a force-pushed pull request is
// refused). `sheaf update` skips such branches (git sees no upstream branch)
// and points to this command.
func AddPRWorktree(ctx context.Context, w *Workspace, h hosting.Hosting, repo string, number int, emit func(Event)) (PRWorktreeResult, error) {
	b := w.Bundle()
	r, err := repoNamed(b, repo)
	if err != nil {
		return PRWorktreeResult{}, err
	}
	// SSH aliases of github.com (e.g. "github_work") are accepted with the
	// heuristic add uses for them (looksLikeGitHubAlias).
	alias := !r.IsGitHub() && looksLikeGitHubAlias(r.HostName())
	if !r.IsGitHub() && !alias {
		return PRWorktreeResult{}, fmt.Errorf("%s is hosted on %s: pull requests can only be checked out for GitHub repositories", r.Name, r.HostName())
	}
	if number <= 0 {
		return PRWorktreeResult{}, fmt.Errorf("invalid pull request number %d", number)
	}
	report := func(msg string) { emit(Event{Repo: r.Name, Phase: PhasePR, Kind: KindProgress, Message: msg}) }
	owner, name, _ := strings.Cut(r.Slug, "/")
	if alias {
		report(fmt.Sprintf("treating host %s as an alias of github.com", r.HostName()))
	}
	report(fmt.Sprintf("resolving pull request #%d of %s", number, r.Slug))
	pr, err := h.PR(ctx, owner, name, number)
	if err != nil {
		return PRWorktreeResult{}, fmt.Errorf("pull request #%d of %s: %w", number, r.Slug, err)
	}
	primary, _, prob := checkPrimary(ctx, w.Root, r)
	if prob != nil {
		return PRWorktreeResult{}, prob
	}
	report("fetching " + RemotesOf(r).String())
	if err := fetchRepo(ctx, primary, RemotesOf(r)); err != nil {
		if ctx.Err() != nil {
			return PRWorktreeResult{}, ctx.Err()
		}
		return PRWorktreeResult{}, fmt.Errorf("fetching %s failed: %w", r.Name, err)
	}
	res := PRWorktreeResult{Repo: r.Name, Number: number}
	// The remote holding the head branch, when it is one of the
	// repository's: the repository itself, or the user's fork.
	rs, headRemote := RemotesOf(r), ""
	switch {
	case !pr.IsCrossRepository && pr.HeadBranch != "":
		headRemote = rs.Reference
	case rs.Forked() && pr.HeadBranch != "" && strings.EqualFold(pr.HeadOwner+"/"+pr.HeadRepo, r.Fork.Slug):
		headRemote = rs.Fork
	}
	if headRemote != "" {
		ok, err := refExists(ctx, primary, "refs/remotes/"+headRemote+"/"+pr.HeadBranch)
		if err != nil {
			return PRWorktreeResult{}, err
		}
		if ok {
			return sameRepoPR(ctx, w, r, pr.HeadBranch, headRemote, res, emit)
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("head branch %s of pull request #%d no longer exists on %s; using refs/pull/%d/head", pr.HeadBranch, number, headRemote, number))
		report("warning: " + res.Warnings[len(res.Warnings)-1])
	}
	return pullRefPR(ctx, w, r, primary, res, emit)
}

func sameRepoPR(ctx context.Context, w *Workspace, r bundle.Repository, head, headRemote string, res PRWorktreeResult, emit func(Event)) (PRWorktreeResult, error) {
	res.Branch = head
	infos, primary, err := listRepo(ctx, w.Root, r)
	if err != nil {
		return PRWorktreeResult{}, fmt.Errorf("%s: %w", r.Name, err)
	}
	remoteRef := headRemote + "/" + head
	remote, err := revParse(ctx, primary, "refs/remotes/"+remoteRef)
	if err != nil {
		return PRWorktreeResult{}, err
	}
	warn := func(msg string) {
		res.Warnings = append(res.Warnings, msg)
		emit(Event{Repo: r.Name, Phase: PhasePR, Kind: KindProgress, Message: "warning: " + msg})
	}
	for _, in := range infos {
		if in.Branch == head {
			if _, err := usableWorktree(in, r.Name); err != nil {
				return PRWorktreeResult{}, err
			}
			res.Dir, res.Path = in.Dir, in.Path
			// Checked out: never modified here (it may have local changes).
			if msg, err := relationWarning(ctx, primary, head, in.Head, remote, remoteRef); err != nil {
				return PRWorktreeResult{}, err
			} else if msg != "" {
				warn(msg + " (run `sheaf update` or merge it there)")
			}
			res.Message = fmt.Sprintf("branch %s of pull request #%d is already checked out at %s", head, res.Number, in.Path)
			return res, nil
		}
	}
	if ok, err := refExists(ctx, primary, "refs/heads/"+head); err != nil {
		return PRWorktreeResult{}, err
	} else if ok {
		// The in-use check and the ref update run under the bundle lock, like
		// the worktree operations they must not interleave with. (wt pr has
		// no query cache today, so the fresh context only matters if a
		// caller ever passes one: defence in depth. The AddWorktree call
		// below, after the lock is released, uses the outer context.)
		ctx, unlock, err := lockBundleFresh(ctx, w.Root, func() {
			emit(Event{Repo: r.Name, Phase: PhasePR, Kind: KindProgress, Message: "waiting for " + lockFileName + " (another sheaf process is working on the bundle)"})
		})
		if err != nil {
			return PRWorktreeResult{}, fmt.Errorf("locking %s: %w", bundle.FileName, err)
		}
		local, err := revParse(ctx, primary, "refs/heads/"+head)
		if err != nil {
			unlock()
			return PRWorktreeResult{}, err
		}
		inUse, err := gitx.BranchesInUse(ctx, primary)
		if err != nil {
			unlock()
			return PRWorktreeResult{}, err
		}
		if local != remote && len(inUse[head]) == 0 && isAncestor(ctx, primary, local, remote) {
			// Strictly behind and not in use: fast-forward the ref
			// (compare-and-swap) so the worktree shows the pull request.
			if _, err := gitx.Run(ctx, primary, "update-ref", "-m", "sheaf wt pr: fast-forward to "+remoteRef, "refs/heads/"+head, remote, local); err != nil {
				unlock()
				return PRWorktreeResult{}, fmt.Errorf("fast-forwarding %s to %s: %w", head, remoteRef, err)
			}
			emit(Event{Repo: r.Name, Phase: PhasePR, Kind: KindProgress, Message: fmt.Sprintf("fast-forwarded local branch %s to %s", head, remoteRef)})
		} else if msg, err := relationWarning(ctx, primary, head, local, remote, remoteRef); err != nil {
			unlock()
			return PRWorktreeResult{}, err
		} else if msg != "" {
			warn(msg)
		}
		// Released before AddWorktree, which takes the lock itself.
		unlock()
	}
	add, err := AddWorktree(ctx, w, r.Name, head, WorktreeAddOptions{NoFetch: true, remotes: []string{headRemote}}, emit)
	if err != nil {
		return PRWorktreeResult{}, err
	}
	res.Dir, res.Path, res.Created = add.Dir, add.Path, true
	res.Warnings = append(res.Warnings, add.Warnings...)
	res.Message = fmt.Sprintf("checked out branch %s of pull request #%d at %s", head, res.Number, add.Path)
	return res, nil
}

// relationWarning describes how local (the tip of branch) differs from
// remote (the tip of remoteRef), or returns "" when they are equal.
func relationWarning(ctx context.Context, dir, branch, local, remote, remoteRef string) (string, error) {
	if local == remote {
		return "", nil
	}
	ahead, err := revCount(ctx, dir, local, "--not", remote)
	if err != nil {
		return "", err
	}
	behind, err := revCount(ctx, dir, remote, "--not", local)
	if err != nil {
		return "", err
	}
	switch {
	case ahead == 0:
		return fmt.Sprintf("branch %s is %s behind %s: it does not show the latest state of the pull request", branch, commits(behind), remoteRef), nil
	case behind == 0:
		return fmt.Sprintf("branch %s is %s ahead of %s (not pushed)", branch, commits(ahead), remoteRef), nil
	}
	return fmt.Sprintf("branch %s has diverged from %s (%s only local, %s only on %s): the worktree shows the local branch; merge or rebase it",
		branch, remoteRef, commits(ahead), commits(behind), remoteRef), nil
}

func revParse(ctx context.Context, dir, ref string) (string, error) {
	out, err := gitx.Run(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func pullRefPR(ctx context.Context, w *Workspace, r bundle.Repository, primary string, res PRWorktreeResult, emit func(Event)) (PRWorktreeResult, error) {
	n := res.Number
	remote := RemotesOf(r).Reference // pull refs live on the repository
	branch, pullRef := PRBranch(n), fmt.Sprintf("refs/pull/%d/head", n)
	res.Branch, res.FromPullRef = branch, true
	exists, err := refExists(ctx, primary, "refs/heads/"+branch)
	if err != nil {
		return PRWorktreeResult{}, err
	}
	if exists {
		if merge := branchMerge(ctx, primary, branch); merge != pullRef {
			return PRWorktreeResult{}, fmt.Errorf("local branch %s exists but does not track pull request #%d (branch.%s.merge is %q, not %s); rename or delete it first", branch, n, branch, merge, pullRef)
		}
	}
	infos, _, err := listRepo(ctx, w.Root, r)
	if err != nil {
		return PRWorktreeResult{}, fmt.Errorf("%s: %w", r.Name, err)
	}
	for _, in := range infos {
		if in.Branch == branch {
			if _, err := usableWorktree(in, r.Name); err != nil {
				return PRWorktreeResult{}, err
			}
			res.Dir, res.Path = in.Dir, in.Path
			return refreshPR(ctx, r, in.Path, pullRef, res, emit)
		}
	}
	note := ""
	if !exists {
		emit(Event{Repo: r.Name, Phase: PhaseFetch, Kind: KindProgress, Message: "fetching " + pullRef + " into " + branch})
		// The head is also recorded (see gitx.PullHeadRef): remove counts
		// its commits as published.
		if err := gitx.FetchRefspecs(ctx, primary, remote, pullRef+":refs/heads/"+branch, "+"+pullRef+":"+gitx.PullHeadRef(n)); err != nil {
			if ctx.Err() != nil {
				return PRWorktreeResult{}, ctx.Err()
			}
			return PRWorktreeResult{}, fmt.Errorf("fetching %s of %s into %s failed: %w", pullRef, remote, branch, err)
		}
	} else if note, err = updatePRBranch(ctx, w.Root, r, primary, branch, pullRef, n, emit); err != nil {
		return PRWorktreeResult{}, err
	}
	oid, err := revParse(ctx, primary, "refs/heads/"+branch)
	if err != nil {
		return PRWorktreeResult{}, err
	}
	if !exists {
		for k, v := range map[string]string{"remote": remote, "merge": pullRef} {
			if _, err := gitx.Run(ctx, primary, "config", "branch."+branch+"."+k, v); err != nil {
				deleteBranchIfUnchanged(context.WithoutCancel(ctx), primary, branch, oid)
				return PRWorktreeResult{}, err
			}
		}
	}
	add, err := AddWorktree(ctx, w, r.Name, branch, WorktreeAddOptions{NoFetch: true}, emit)
	if err != nil {
		if !exists {
			// Leave nothing behind: the branch was created above.
			deleteBranchIfUnchanged(context.WithoutCancel(ctx), primary, branch, oid)
		}
		return PRWorktreeResult{}, err
	}
	res.Dir, res.Path, res.Created = add.Dir, add.Path, true
	res.Warnings = append(res.Warnings, add.Warnings...)
	res.Message = fmt.Sprintf("checked out pull request #%d (%s) as %s at %s%s", n, pullRef, branch, add.Path, note)
	return res, nil
}

// updatePRBranch brings the existing, not checked out, local branch of pull
// request n up to date with refs/pull/<n>/head before its worktree is
// created: fast-forwarded (compare-and-swap) when behind, left alone when
// ahead (the returned note says so), refused when diverged, in use, or
// changed by someone else during the fetch.
func updatePRBranch(ctx context.Context, root string, r bundle.Repository, primary, branch, pullRef string, n int, emit func(Event)) (string, error) {
	// The fetch runs before the bundle lock is taken: it may prompt for
	// credentials, which must not block the other sheaf processes. pr/<n>
	// is read before it, so that a change made meanwhile (e.g. by a
	// concurrent wt pr, whose fetch may be newer) is detected under the lock.
	before, err := revParse(ctx, primary, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	emit(Event{Repo: r.Name, Phase: PhaseFetch, Kind: KindProgress, Message: "fetching " + pullRef})
	head, err := fetchPullHead(ctx, primary, RemotesOf(r).Reference, pullRef, n)
	if err != nil {
		return "", err
	}
	// Under the bundle lock (released before AddWorktree, which takes it).
	// Fresh reads are defence in depth: wt pr has no query cache today.
	ctx, unlock, err := lockBundleFresh(ctx, root, func() {
		emit(Event{Repo: r.Name, Phase: PhasePR, Kind: KindProgress, Message: "waiting for " + lockFileName + " (another sheaf process is working on the bundle)"})
	})
	if err != nil {
		return "", fmt.Errorf("locking %s: %w", bundle.FileName, err)
	}
	defer unlock()
	inUse, err := gitx.BranchesInUse(ctx, primary)
	if err != nil {
		return "", err
	}
	if uses := inUse[branch]; len(uses) > 0 {
		return "", fmt.Errorf("branch %s is %s; finish that first", branch, describeUse(uses[0]))
	}
	local, err := revParse(ctx, primary, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	switch {
	case head == local:
		return "", nil
	case local != before:
		return "", fmt.Errorf("local branch %s changed while %s was being fetched (from %s to %s: another sheaf or git command?); nothing was changed: run this command again",
			branch, pullRef, shortID(before), shortID(local))
	case isAncestor(ctx, primary, local, head):
		// Compare-and-swap: refused if the branch moved since it was read
		// under the lock (by a git command, which does not take it).
		if _, err := gitx.Run(ctx, primary, "update-ref", "-m", "sheaf wt pr: fast-forward to "+pullRef, "refs/heads/"+branch, head, local); err != nil {
			return "", fmt.Errorf("fast-forwarding %s to %s: %w", branch, pullRef, err)
		}
		return "", nil
	case isAncestor(ctx, primary, head, local):
		ahead, err := revCount(ctx, primary, local, "--not", head)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(" (local branch is ahead of the pull request head by %s)", commits(ahead)), nil
	}
	ahead, err := revCount(ctx, primary, local, "--not", head)
	if err != nil {
		return "", err
	}
	behind, err := revCount(ctx, primary, head, "--not", local)
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("local branch %s has diverged from pull request #%d (%s was force-pushed, or %s has local commits): "+
		"%s has %s that %s not in the pull request, and the pull request has %s that %s not in %s. Nothing was changed. "+
		"To review the current head, rename the branch (git branch -m %s %s-old) and run this command again (deleting it instead, its %s would be lost)",
		branch, n, pullRef, branch, branch, commits(ahead), isAre(ahead), commits(behind), isAre(behind), branch, branch, branch, commits(ahead))
}

// fetchPullHead fetches pullRef of remote into a private temporary ref
// (refs/sheaf/tmp/pr-<n>-<pid>-<random>), reads it and deletes it, and
// returns the commit. FETCH_HEAD is not used: another fetch in the same work
// tree could overwrite it in between. The head is also recorded as
// gitx.PullHeadRef(n) (not read back: a concurrent fetch may move it).
func fetchPullHead(ctx context.Context, dir, remote, pullRef string, n int) (string, error) {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	tmp := fmt.Sprintf("refs/sheaf/tmp/pr-%d-%d-%s", n, os.Getpid(), hex.EncodeToString(rnd[:]))
	defer func() {
		// Also after a failure or a cancellation.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = gitx.Run(dctx, dir, "update-ref", "-d", tmp)
	}()
	if prFetchHook != nil {
		prFetchHook(dir)
	}
	if err := gitx.FetchRefspecs(ctx, dir, remote, "+"+pullRef+":"+tmp, "+"+pullRef+":"+gitx.PullHeadRef(n)); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("fetching %s of %s failed: %w", pullRef, remote, err)
	}
	return revParse(ctx, dir, tmp)
}

// prFetchHook, when set by a test, runs in fetchPullHead right before the
// fetch.
var prFetchHook func(dir string)

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// refreshPR fast-forwards the worktree at path, on the branch of a pull
// request, to the current refs/pull/<n>/head of origin.
func refreshPR(ctx context.Context, r bundle.Repository, path, pullRef string, res PRWorktreeResult, emit func(Event)) (PRWorktreeResult, error) {
	st, err := gitx.Status(ctx, path)
	if err != nil {
		return PRWorktreeResult{}, err
	}
	if st.Dirty() {
		return PRWorktreeResult{}, fmt.Errorf("%s has uncommitted changes: commit or stash them, then run `sheaf wt pr %s %d` again", path, r.Name, res.Number)
	}
	emit(Event{Repo: r.Name, Worktree: res.Dir, Phase: PhaseFetch, Kind: KindProgress, Message: "fetching " + pullRef})
	head, err := fetchPullHead(ctx, path, RemotesOf(r).Reference, pullRef, res.Number)
	if err != nil {
		return PRWorktreeResult{}, err
	}
	switch {
	case head == st.Head:
		res.Message = fmt.Sprintf("pull request #%d is up to date at %s", res.Number, path)
		return res, nil
	case isAncestor(ctx, path, head, st.Head):
		res.Message = fmt.Sprintf("pull request #%d is up to date at %s (local commits ahead of %s)", res.Number, path, pullRef)
		return res, nil
	case !isAncestor(ctx, path, st.Head, head):
		return PRWorktreeResult{}, fmt.Errorf("%s of pull request #%d cannot be fast-forwarded to %s (%s): it was rewritten (force-pushed?) or has local commits; nothing was changed",
			res.Branch, res.Number, pullRef, shortID(head))
	}
	if err := ctx.Err(); err != nil {
		return PRWorktreeResult{}, err
	}
	// Like gitx.FastForward, a started merge is not interrupted by ctx
	// (it would leave a half-updated work tree); it is bounded instead.
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitx.DefaultMergeTimeout)
	defer cancel()
	if _, err := gitx.Run(mctx, path, "merge", "--quiet", "--ff-only", "--no-autostash", head); err != nil {
		return PRWorktreeResult{}, fmt.Errorf("fast-forwarding %s to %s: %w", path, shortID(head), err)
	}
	res.Message = fmt.Sprintf("fast-forwarded pull request #%d at %s (%s..%s)", res.Number, path, shortID(st.Head), shortID(head))
	return res, nil
}

// isAncestor reports whether a is an ancestor of (or equal to) b; errors
// count as false.
func isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := gitx.Run(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// branchMerge returns branch.<branch>.merge, or "" when unset or unreadable.
func branchMerge(ctx context.Context, dir, branch string) string {
	out, err := gitx.Run(ctx, dir, "config", "--get", "branch."+branch+".merge")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// pullRequestReason explains why update skips a checkout on a pull request
// branch (whose upstream refs/pull/<n>/head has no remote-tracking ref), or
// returns "".
func pullRequestReason(ctx context.Context, c Checkout) string {
	if c.Branch == "" {
		return ""
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	n, ok := gitx.PullRequestRef(branchMerge(sctx, c.Path, c.Branch))
	if !ok {
		return ""
	}
	return fmt.Sprintf("pull request branch (refs/pull/%d/head, no remote-tracking branch): refresh it with `sheaf wt pr %s %d`", n, c.Repo, n)
}

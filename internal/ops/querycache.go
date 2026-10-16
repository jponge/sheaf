package ops

import (
	"context"

	"github.com/jponge/sheaf/internal/gitx"
)

// withCommandCache gives one command a git query cache (gitx.WithQueryCache):
// repeated questions about a repository (top level of a checkout, worktree
// list, ref existence) are asked once until a git invocation changes that
// repository. The cache lives in the returned context only, so it ends with
// the command; a command called by another one shares the caller's cache.
//
// Commands with a cache: status, update, fetch, wt list, path, doctor,
// prune (planning only), remove, wt rm, wt add (branch and tag). The others
// never read through a cache: adopt moves checkouts and add and sync clone
// into new directories (file system changes outside git), run executes
// arbitrary user commands, and wt pr was not measured.
//
// A command's own mutations need no care: every git invocation that is not
// a known read-only query invalidates the answers of its repository (see
// gitx.WithQueryCache). Changes made outside git or by other processes are
// not seen by cached reads, hence the fresh reads below.
//
// Fresh reads (gitx.WithFreshReads) are used for the facts a safety
// decision rests on:
//
//   - remove: everything after the confirmation (the re-check of the plan,
//     then the deletion);
//   - prune (ApplyPrune): everything, since it re-checks a plan made by an
//     earlier call (the worktree, branch and commit of each candidate);
//   - wt rm, prune's branch deletion, wt add (branch and tag): everything
//     under the bundle lock (see lockBundleFresh), i.e. the re-check of the
//     removal plan and the branch deletion, the add checks and the creation;
//   - gitx itself: the branch clean-up after a failed worktree add.
//
// Some of these are no-ops today and kept as documented defence in depth,
// not as coverage: wt pr's locked sections (wt pr has no cache), prune's
// branch deletion (ApplyPrune is already fresh) and the gitx clean-up
// (the failed add before it already invalidated the repository).
func withCommandCache(ctx context.Context) context.Context {
	return gitx.WithQueryCache(ctx)
}

// lockBundleFresh is lockBundle for code that reads git under the lock: the
// returned context makes every git query fresh (gitx.WithFreshReads), so
// what is checked under the lock is never an answer cached before it was
// taken. Callers replace their context with it.
func lockBundleFresh(ctx context.Context, root string, waiting func()) (context.Context, func(), error) {
	unlock, err := lockBundle(ctx, root, waiting)
	return gitx.WithFreshReads(ctx), unlock, err
}

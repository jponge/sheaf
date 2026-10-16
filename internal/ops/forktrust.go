package ops

import (
	"context"
	"errors"
	"strings"

	"github.com/jponge/sheaf/internal/gitx"
)

// forkDistrust names the fork remote of a forked repository whose
// remote-tracking refs are not trusted, and why: the fork appears to be
// gone (its refs are stale and hold the only remote copy of what is on them
// alone), or it was not fetched (--offline). Untrusted refs neither count as
// pushed nor as merge targets. The zero value trusts every remote.
type forkDistrust struct {
	remote string
	why    string
	gone   bool // the fork appears to be gone (not merely unfetched)
}

func goneForkDistrust(fork string) forkDistrust {
	return forkDistrust{remote: fork, why: "your fork (" + fork + ") appears to be gone", gone: true}
}

func offlineForkDistrust(fork string) forkDistrust {
	return forkDistrust{remote: fork, why: "--offline: the state of your fork (" + fork + ") is unknown"}
}

func (d forkDistrust) set() bool { return d.remote != "" }

// untrusted returns the untrusted remotes (for gitx.DeletionRisksOptions).
func (d forkDistrust) untrusted() []string {
	if !d.set() {
		return nil
	}
	return []string{d.remote}
}

// excludes returns the rev-list arguments that, placed right before
// --remotes, leave the untrusted remote's refs out.
func (d forkDistrust) excludes() []string {
	if !d.set() {
		return nil
	}
	return []string{"--exclude=" + d.remote + "/*"}
}

// covers reports whether the full ref name is an untrusted remote-tracking ref.
func (d forkDistrust) covers(ref string) bool {
	return d.set() && strings.HasPrefix(ref, "refs/remotes/"+d.remote+"/")
}

// note explains the distrust for messages ("" when trusted).
func (d forkDistrust) note() string {
	if !d.set() {
		return ""
	}
	return " (" + d.why + ": its remote-tracking refs " + d.remote + "/* are not trusted)"
}

// fetchForkRemote fetches the fork remote into the repository of dir. It
// returns gone=true when the fork appears to be gone (see
// remoteRepoGoneFor), the fetch error for any other failure, and ctx's
// error when cancelled.
func fetchForkRemote(ctx context.Context, dir, fork string) (gone bool, err error) {
	err = gitx.FetchRemote(ctx, dir, fork)
	if err == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return forkGoneError(err, fork), err
}

// forkGoneError reports whether err, from a fetch of fork, says that the
// remote repository is gone.
func forkGoneError(err error, fork string) bool {
	var ge *gitx.GitError
	return errors.As(err, &ge) && remoteRepoGoneFor(ge.Stderr, fork)
}

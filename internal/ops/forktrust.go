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
// pushed nor as merge targets. The zero value trusts every remote sheaf
// fetches (and no other, see trusted).
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

// trusted returns the remotes of rs whose remote-tracking refs count as
// pushed and as merge targets (for gitx.DeletionRisksOptions.TrustedRemotes):
// the remotes sheaf fetches (rs.All()), except the distrusted fork. The
// refs of any other remote are never trusted, with or without --offline:
// sheaf does not fetch them, so they may be stale (a branch deleted there,
// a remote that no longer exists).
func (d forkDistrust) trusted(rs gitx.Remotes) []string {
	var out []string
	for _, r := range rs.All() {
		if r != d.remote {
			out = append(out, r)
		}
	}
	return out
}

// covers reports whether the full ref name is an untrusted remote-tracking ref.
func (d forkDistrust) covers(ref string) bool {
	return d.set() && strings.HasPrefix(ref, "refs/remotes/"+d.remote+"/")
}

// untrustedRefsNote explains, for messages, that the remote-tracking refs
// containing one of commits that are not in trusted (the refs written by
// sheaf's fetches of the remotes named fetched, see
// gitx.TrustedRemoteRefs) did not count: "" when there is none, or when
// they cannot be listed. Refs covered by d are left to d.note().
func untrustedRefsNote(ctx context.Context, dir string, trusted gitx.RemoteRefSet, fetched []string, d forkDistrust, commits []string) string {
	if len(commits) == 0 {
		return "" // without --contains, every ref would be listed
	}
	args := []string{"for-each-ref", "--format=%(refname)"}
	for _, c := range commits {
		args = append(args, "--contains="+c)
	}
	out, err := gitx.Run(ctx, dir, append(args, "refs/remotes")...)
	if err != nil {
		return ""
	}
	var names []string
	for ref := range strings.FieldsSeq(out) {
		if !strings.HasSuffix(ref, "/HEAD") && !trusted.Contains(ref) && !d.covers(ref) {
			names = append(names, strings.TrimPrefix(ref, "refs/remotes/"))
		}
	}
	if len(names) == 0 {
		return ""
	}
	verb := "does"
	if len(names) > 1 {
		verb = "do"
	}
	return " (also on " + branchList(names) + ", which " + verb + " not count: only the remote-tracking refs that sheaf's fetch of " +
		strings.Join(fetched, " and ") + " updates count, others may be stale)"
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
func fetchForkRemote(ctx context.Context, net gitx.Net, dir, fork string) (gone bool, err error) {
	err = net.FetchRemote(ctx, dir, fork)
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

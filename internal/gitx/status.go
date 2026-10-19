package gitx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RepoStatus is a snapshot of a work tree's state, as reported by
// `git status --porcelain=v2 --branch --show-stash -z`.
type RepoStatus struct {
	// Branch is the short name of the checked-out branch, empty when Detached.
	Branch string
	// Detached is true when HEAD is detached.
	Detached bool
	// Unborn is true when the branch has no commit yet (Head is then empty).
	Unborn bool
	// Head is the commit id of HEAD.
	Head string
	// Upstream is the configured upstream (e.g. "origin/main"), empty if none.
	Upstream string
	// UpstreamGone is true when an upstream is configured but its
	// remote-tracking ref does not exist (deleted on the remote and pruned,
	// or never fetched). Ahead and Behind are then 0.
	UpstreamGone bool
	// Ahead and Behind count commits relative to the local remote-tracking
	// ref of Upstream; they are only as fresh as the last fetch.
	Ahead, Behind int
	// Staged counts entries with index changes, Unstaged entries with work
	// tree changes to tracked files (an entry can count in both). Renames and
	// copies count once.
	Staged, Unstaged int
	// Untracked counts untracked entries; an untracked directory containing
	// no tracked file counts once (--untracked-files=normal). A submodule
	// whose only change is untracked content inside it counts here too, not
	// in Unstaged; any other submodule change (new commits, modified tracked
	// content) counts in Unstaged.
	Untracked int
	// Conflicted counts unmerged entries.
	Conflicted int
	// Stashes is the number of stash entries of the repository (shared by
	// all its worktrees).
	Stashes int

	ignored int // ignored entries, only counted with --ignored (DeletionRisks)
}

// Dirty reports whether the work tree has staged, unstaged or conflicted
// changes. Untracked files deliberately do not make a tree dirty: they are
// never lost by a fast-forward (git refuses to overwrite them) and are
// commonly build output; they are reported through Untracked.
func (s *RepoStatus) Dirty() bool {
	return s.Staged > 0 || s.Unstaged > 0 || s.Conflicted > 0
}

// Status returns the status of the work tree containing dir. It runs git with
// --no-optional-locks, so it does not refresh the index and can run
// concurrently with other git commands on the same repository. It passes
// --untracked-files=normal and --ignore-submodules=none, so configuration
// that hides changes (status.showUntrackedFiles, diff.ignoreSubmodules,
// submodule.<name>.ignore, from any config source) does not hide them here.
func Status(ctx context.Context, dir string) (*RepoStatus, error) {
	return status(ctx, dir)
}

func status(ctx context.Context, dir string, extra ...string) (*RepoStatus, error) {
	args := append([]string{"--no-optional-locks", "status", "--porcelain=v2", "--branch", "--show-stash",
		"--untracked-files=normal", "--ignore-submodules=none", "-z"}, extra...)
	out, err := Run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	st, err := parseStatus(out)
	if err != nil {
		return nil, fmt.Errorf("gitx: status in %s: %w", dir, err)
	}
	return st, nil
}

// CommitTimes returns the committer dates of the commits oids (full commit
// ids) of the repository containing dir, by commit id, starting one git
// process at most (none without oids). It fails when one of them is not a
// commit of the repository.
func CommitTimes(ctx context.Context, dir string, oids []string) (map[string]time.Time, error) {
	times := map[string]time.Time{}
	if len(oids) == 0 {
		return times, nil
	}
	// rev-list is plumbing: unlike log, configuration such as
	// log.showSignature does not change its output.
	args := append([]string{"rev-list", "--no-walk=unsorted", "--no-commit-header", "--format=%H %ct", "--end-of-options"}, oids...)
	out, err := Run(ctx, dir, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		oid, ct, ok := strings.Cut(line, " ")
		secs, err := strconv.ParseInt(ct, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("gitx: commit times in %s: malformed line %q", dir, line)
		}
		times[oid] = time.Unix(secs, 0)
	}
	return times, nil
}

// parseStatus parses NUL-separated porcelain v2 output. Paths are never
// interpreted, only skipped, so they may contain any byte but NUL.
func parseStatus(out string) (*RepoStatus, error) {
	st := &RepoStatus{}
	abSeen := false
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue // trailing terminator
		}
		switch f[0] {
		case '#':
			key, val, _ := strings.Cut(strings.TrimPrefix(f, "# "), " ")
			switch key {
			case "branch.oid":
				if val == "(initial)" {
					st.Unborn = true
				} else {
					st.Head = val
				}
			case "branch.head":
				if val == "(detached)" {
					st.Detached = true
				} else {
					st.Branch = val
				}
			case "branch.upstream":
				st.Upstream = val
			case "branch.ab":
				a, b, ok := strings.Cut(val, " ")
				ahead, err1 := strconv.Atoi(strings.TrimPrefix(a, "+"))
				behind, err2 := strconv.Atoi(strings.TrimPrefix(b, "-"))
				if !ok || err1 != nil || err2 != nil || !strings.HasPrefix(a, "+") || !strings.HasPrefix(b, "-") {
					return nil, fmt.Errorf("malformed header %q", f)
				}
				st.Ahead, st.Behind, abSeen = ahead, behind, true
			case "stash":
				n, err := strconv.Atoi(val)
				if err != nil {
					return nil, fmt.Errorf("malformed header %q", f)
				}
				st.Stashes = n
			}
			// Unknown headers are ignored for forward compatibility.
		case '1', '2':
			if len(f) < 4 || f[1] != ' ' {
				return nil, fmt.Errorf("malformed entry %q", f)
			}
			if f[2:4] == ".M" && len(f) >= 9 && f[4:9] == " S..U" {
				// Submodule with only untracked content (see Untracked).
				st.Untracked++
				continue
			}
			if f[2] != '.' {
				st.Staged++
			}
			if f[3] != '.' {
				st.Unstaged++
			}
			if f[0] == '2' {
				// Renamed/copied: the original path is the next field.
				i++
				if i >= len(fields) || fields[i] == "" {
					return nil, fmt.Errorf("rename entry without original path %q", f)
				}
			}
		case 'u':
			st.Conflicted++
		case '?':
			st.Untracked++
		case '!':
			st.ignored++ // only with --ignored
		default:
			return nil, fmt.Errorf("unexpected entry %q", f)
		}
	}
	st.UpstreamGone = st.Upstream != "" && !abSeen
	return st, nil
}

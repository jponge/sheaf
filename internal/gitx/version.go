package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GitVersion is the numeric part of a git version.
type GitVersion struct{ Major, Minor, Patch int }

// MinGitVersion is the oldest git release gitx supports: `worktree list -z`
// needs 2.36, and `status --porcelain=v2 --show-stash` needs 2.35 (older
// releases silently omit the stash count).
var MinGitVersion = GitVersion{2, 36, 0}

// ErrGitTooOld is returned (wrapped) by CheckVersion.
var ErrGitTooOld = errors.New("gitx: git is too old")

func (v GitVersion) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// AtLeast reports whether v >= o.
func (v GitVersion) AtLeast(o GitVersion) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Patch >= o.Patch
}

// ParseVersion parses the output of `git version`, e.g. "git version 2.47.3",
// "git version 2.39.3 (Apple Git-146)" or "git version 2.45.1.windows.1".
// Only the leading digits of the first three dot-separated components are
// used; a missing patch component is 0.
func ParseVersion(out string) (GitVersion, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(out), "git version ")
	fields := strings.Fields(rest)
	if !ok || len(fields) == 0 {
		return GitVersion{}, fmt.Errorf("gitx: unrecognized git version output %q", out)
	}
	parts := strings.SplitN(fields[0], ".", 4)
	if len(parts) < 2 {
		return GitVersion{}, fmt.Errorf("gitx: unrecognized git version %q", fields[0])
	}
	var nums [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		p := parts[i]
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			if i == 2 {
				break // e.g. "2.36.rc0": no numeric patch
			}
			return GitVersion{}, fmt.Errorf("gitx: unrecognized git version %q", fields[0])
		}
		n, err := strconv.Atoi(p[:end])
		if err != nil {
			return GitVersion{}, fmt.Errorf("gitx: unrecognized git version %q: %w", fields[0], err)
		}
		nums[i] = n
	}
	return GitVersion{nums[0], nums[1], nums[2]}, nil
}

// Version returns the version of the git found on PATH. git runs in
// os.TempDir() when that is an existing directory, else in the root
// directory, so a stale TMPDIR does not make it fail.
func Version(ctx context.Context) (GitVersion, error) {
	dir := os.TempDir()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = string(filepath.Separator)
	}
	out, err := Run(ctx, dir, "version")
	if err != nil {
		return GitVersion{}, err
	}
	return ParseVersion(out)
}

// CheckVersion returns the installed git version and an error wrapping
// ErrGitTooOld when it is older than MinGitVersion.
func CheckVersion(ctx context.Context) (GitVersion, error) {
	v, err := Version(ctx)
	if err != nil {
		return GitVersion{}, err
	}
	return v, checkVersion(v)
}

func checkVersion(v GitVersion) error {
	if !v.AtLeast(MinGitVersion) {
		return fmt.Errorf("%w: found %v, need %v or later", ErrGitTooOld, v, MinGitVersion)
	}
	return nil
}

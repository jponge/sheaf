package gitx

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// remoteConf is the configuration of one remote (see readRemotes).
type remoteConf struct {
	urls         []string
	fetch        []string // fetch refspecs, as configured
	skipFetchAll bool     // remote.<name>.skipFetchAll: `git fetch --all` skips it
}

// remoteConfigArgs reads every remote.* setting at once; the query cache
// keeps its answer (see cacheableCalls).
var remoteConfigArgs = []string{"config", "-z", "--get-regexp", `^remote\.`}

// readRemotes returns the configured remotes of the repository of dir by
// name (one git call).
func readRemotes(ctx context.Context, dir string) (map[string]*remoteConf, error) {
	out, err := Run(ctx, dir, remoteConfigArgs...)
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return map[string]*remoteConf{}, nil // no remote.* setting
		}
		return nil, err
	}
	return parseRemotes(out), nil
}

// parseRemotes parses the output of remoteConfigArgs: NUL-terminated
// entries "key\nvalue", or "key" alone for a boolean set without a value.
// Git lowercases the section and the variable name, not the remote name
// (which may contain dots).
func parseRemotes(out string) map[string]*remoteConf {
	m := map[string]*remoteConf{}
	for entry := range strings.SplitSeq(out, "\x00") {
		key, val, hasVal := strings.Cut(entry, "\n")
		rest, ok := strings.CutPrefix(key, "remote.")
		i := strings.LastIndexByte(rest, '.')
		if !ok || i <= 0 {
			continue // e.g. remote.pushdefault
		}
		name, v := rest[:i], rest[i+1:]
		c := m[name]
		if c == nil {
			c = &remoteConf{}
			m[name] = c
		}
		switch v {
		case "url":
			c.urls = append(c.urls, val)
		case "fetch":
			c.fetch = append(c.fetch, val)
		case "skipfetchall":
			c.skipFetchAll = !hasVal || !configFalse(val)
		}
	}
	return m
}

// configFalse reports whether a git boolean value is false. Values git
// would reject count as true (for skipFetchAll: not fetched, not trusted).
func configFalse(v string) bool {
	switch strings.ToLower(v) {
	case "false", "no", "off", "":
		return true
	}
	n, err := strconv.Atoi(v)
	return err == nil && n == 0
}

// fetchAllRemotes returns the names of the remotes `git fetch --all`
// fetches: those with a URL and without skipFetchAll.
func fetchAllRemotes(conf map[string]*remoteConf) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(conf)) {
		if c := conf[name]; len(c.urls) > 0 && !c.skipFetchAll {
			out = append(out, name)
		}
	}
	return out
}

// refspecDst returns the destination of a fetch refspec ("" when it has
// none: such a refspec only writes FETCH_HEAD) and whether it is negative
// (^<src>: excludes sources).
func refspecDst(spec string) (dst string, negative bool) {
	if strings.HasPrefix(spec, "^") {
		return "", true
	}
	_, dst, _ = strings.Cut(strings.TrimPrefix(spec, "+"), ":")
	return dst, false
}

// RemoteRefSet is the set of remote-tracking refs that the fetches of some
// remotes write and prune (see TrustedRemoteRefs): the commits they hold
// are on those remotes, as of the last fetch. The zero value is empty.
type RemoteRefSet struct {
	globs    []string // wildcard refspec destinations under refs/remotes/
	refs     []string // exact refspec destinations under refs/remotes/
	excludes []string // namespaces and destinations of the other remotes
}

// TrustedRemoteRefs returns the refs that `git fetch --prune <remote>`
// writes and prunes, for each of remotes, in the repository of dir: the
// destinations under refs/remotes/ of its fetch refspecs (by default
// refs/remotes/<remote>/*). Excluded: refs of a remote that is not
// configured or has no URL; every ref of a remote with a negative refspec
// (its fetch neither updates nor prunes the destinations of the excluded
// sources); and the refs that also belong to any other remote (its
// refs/remotes/<name>/* namespace and its refspec destinations), e.g.
// refs/remotes/origin/x/* of a remote named origin/x. One git call (see
// readRemotes).
func TrustedRemoteRefs(ctx context.Context, dir string, remotes []string) (RemoteRefSet, error) {
	conf, err := readRemotes(ctx, dir)
	if err != nil {
		return RemoteRefSet{}, err
	}
	return refSetOf(conf, remotes), nil
}

func refSetOf(conf map[string]*remoteConf, trusted []string) RemoteRefSet {
	var s RemoteRefSet
	var exact []string
	for _, name := range slices.Sorted(maps.Keys(conf)) {
		c := conf[name]
		if slices.Contains(trusted, name) && len(c.urls) > 0 && !slices.ContainsFunc(c.fetch, func(f string) bool { _, neg := refspecDst(f); return neg }) {
			for _, f := range c.fetch {
				dst, _ := refspecDst(f)
				switch n := strings.Count(dst, "*"); {
				case !strings.HasPrefix(dst, "refs/remotes/"):
				case n == 1:
					s.globs = append(s.globs, dst)
				case n == 0:
					exact = append(exact, dst)
				}
			}
			continue
		}
		s.excludes = append(s.excludes, "refs/remotes/"+name+"/*")
		for _, f := range c.fetch {
			if dst, _ := refspecDst(f); strings.HasPrefix(dst, "refs/remotes/") {
				s.excludes = append(s.excludes, dst)
			}
		}
	}
	for _, r := range exact {
		if !slices.ContainsFunc(s.excludes, func(e string) bool { return refGlobMatch(e, r) }) {
			s.refs = append(s.refs, r)
		}
	}
	return s
}

// refGlobMatch matches a ref against a refspec-like pattern with at most
// one *, which matches any string (slashes included, as in git's --glob
// and refspecs).
func refGlobMatch(pattern, ref string) bool {
	pre, suf, ok := strings.Cut(pattern, "*")
	if !ok {
		return pattern == ref
	}
	return len(ref) >= len(pre)+len(suf) && strings.HasPrefix(ref, pre) && strings.HasSuffix(ref, suf)
}

// Contains reports whether the full ref name is in the set.
func (s RemoteRefSet) Contains(ref string) bool {
	if slices.ContainsFunc(s.excludes, func(e string) bool { return refGlobMatch(e, ref) }) {
		return false
	}
	return slices.Contains(s.refs, ref) || slices.ContainsFunc(s.globs, func(g string) bool { return refGlobMatch(g, ref) })
}

// RevListArgs returns the rev-list arguments selecting the refs of the set:
// the options to put before the revisions (--ignore-missing, for exact
// refs that do not exist) and the revisions (e.g. after --not).
func (s RemoteRefSet) RevListArgs() (lead, revs []string) {
	for _, g := range s.globs {
		// --exclude applies to the next --glob only.
		for _, e := range s.excludes {
			revs = append(revs, "--exclude="+e)
		}
		revs = append(revs, "--glob="+g)
	}
	if len(s.refs) > 0 {
		lead = []string{"--ignore-missing"}
		revs = append(revs, s.refs...)
	}
	return lead, revs
}

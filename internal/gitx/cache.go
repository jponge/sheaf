package gitx

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Query cache
//
// A command often asks git the same question about the same repository
// several times (the top level of a checkout, the worktree list, whether a
// ref exists). WithQueryCache makes the invocations under a context share
// the answers to a few such read-only queries (see classify), for the
// duration of one command: the cache lives in the context, never in a
// package variable, so it disappears with the command.
//
// Invalidation is structural: every git process gitx starts goes through
// run, which classifies its arguments. Only invocations recognised as
// read-only (an explicit allowlist) leave the cache alone; any other
// invocation (fetch, merge, worktree add/remove/repair, update-ref,
// branch, config writes, clone, remote changes, and anything unknown) is
// treated as a mutation and, once it has finished (successfully or not),
// drops the cached answers of its repository, or of every repository when
// its repository cannot be determined (e.g. a clone, which runs in the
// parent directory).
//
// Answers are grouped by repository, identified by its common git directory
// as read from the file system (the dir/.git directory, or the gitdir and
// commondir files a .git file leads to) with symbolic links resolved: no
// git process is needed, and a directory that was moved, deleted or
// replaced by another repository since an answer was cached gets another
// key (or none). A directory without a .git entry (a subdirectory, or a
// checkout that lost its .git, where git would discover an enclosing
// repository) is never cached.
//
// The cache cannot see changes made by other processes or by code that
// changes the file system directly. Nor does a mutation invalidate the
// entries of OTHER repositories it changes as a side effect: a fetch
// recursing into submodules, a merge with submodule.recurse, hooks, or a
// background `gc --auto` running `worktree prune`. Reads that must reflect
// the current state (safety re-checks before deleting something, re-reads
// under a lock) therefore run under WithFreshReads: in sheaf, every deletion
// or overwrite decision is re-checked with fresh reads or rests on reads the
// cache never covers (status, rev-list, ...).

type cacheScope struct {
	c     *queryCache // nil under WithFreshReads without a cache
	fresh bool
}

type cacheScopeKey struct{}

// WithQueryCache returns a context under which the read-only git queries of
// gitx made in a repository directory are answered at most once per
// question, until a mutating invocation in the same repository (see the
// package comment above). It is meant for the scope of one command: callers
// create it at the start of the command and drop it at the end. If ctx
// already carries a cache, or was made by WithFreshReads, ctx is returned
// unchanged: a command started by another one shares its cache, and one
// started under fresh reads stays fresh.
func WithQueryCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(cacheScopeKey{}).(cacheScope); ok {
		return ctx
	}
	return context.WithValue(ctx, cacheScopeKey{}, cacheScope{c: &queryCache{}})
}

// WithFreshReads returns a context under which no query is answered from
// the cache (nor stored in it): every query runs git. Mutations made under
// it still invalidate the cache of ctx, if any. Use it for the reads that a
// safety decision rests on: re-checks before a deletion and reads under the
// bundle lock.
func WithFreshReads(ctx context.Context) context.Context {
	sc, _ := ctx.Value(cacheScopeKey{}).(cacheScope)
	if sc.fresh {
		return ctx
	}
	sc.fresh = true
	return context.WithValue(ctx, cacheScopeKey{}, sc)
}

// queryCache holds the cached answers of one command.
type queryCache struct {
	mu      sync.Mutex
	gen     uint64            // incremented when every entry is dropped
	repoGen map[string]uint64 // per repository, incremented when its entries are dropped
	entries map[cacheKey]cacheEntry
}

type cacheKey struct{ repo, dir, args string }

type cacheEntry struct {
	out string
	err *GitError // nil on success; a copy is returned to each caller
}

// cacheTicket records the state of the cache when a query started, so that
// its answer is not stored if a mutation invalidated the repository while
// the query ran.
type cacheTicket struct {
	key          cacheKey
	gen, repoGen uint64
}

func argsKey(args []string) string { return strings.Join(args, "\x00") }

// cacheLookup is the result of queryCache.lookup.
type cacheLookup struct {
	hit bool
	// out and err are the cached answer when hit.
	out string
	err error
	// ticket stores the answer once obtained, valid iff storable (never on
	// a hit).
	ticket   cacheTicket
	storable bool
}

// lookup returns the cached answer to args in dir, or a ticket to store the
// answer once obtained.
func (c *queryCache) lookup(dir string, args []string) cacheLookup {
	repo, ok := repoKey(dir)
	if !ok {
		return cacheLookup{}
	}
	k := cacheKey{repo: repo, dir: filepath.Clean(dir), args: argsKey(args)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[k]; ok {
		if e.err != nil {
			ge := *e.err
			ge.Args = slices.Clone(ge.Args)
			return cacheLookup{hit: true, out: e.out, err: &ge}
		}
		return cacheLookup{hit: true, out: e.out}
	}
	return cacheLookup{ticket: cacheTicket{key: k, gen: c.gen, repoGen: c.repoGen[repo]}, storable: true}
}

// store records an answer unless the repository was invalidated since the
// ticket was issued.
func (c *queryCache) store(t cacheTicket, out string, ge *GitError) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != t.gen || c.repoGen[t.key.repo] != t.repoGen {
		return
	}
	if c.entries == nil {
		c.entries = map[cacheKey]cacheEntry{}
	}
	e := cacheEntry{out: out}
	if ge != nil {
		cp := *ge
		cp.Args = slices.Clone(ge.Args)
		e.err = &cp
	}
	c.entries[t.key] = e
}

// invalidate drops the answers of the repositories of dir as derived before
// and after a mutation (before, after), or every answer when either cannot
// be determined.
func (c *queryCache) invalidate(before, after string, okBefore, okAfter bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !okBefore || !okAfter {
		c.gen++
		c.entries = nil
		return
	}
	if c.repoGen == nil {
		c.repoGen = map[string]uint64{}
	}
	for _, repo := range []string{before, after} {
		c.repoGen[repo]++
		for k := range c.entries {
			if k.repo == repo {
				delete(c.entries, k)
			}
		}
	}
}

// repoKey returns the resolved common git directory of the repository whose
// work tree has its top level at dir, read from the file system: dir/.git
// is that directory or a file "gitdir: <path>" naming the per-worktree git
// directory, whose commondir file (if any) names the common one. Relative
// paths are resolved against the physical (symlink-free) directories, as
// git does. ok is false when dir has no .git entry or the chain cannot be
// read.
func repoKey(dir string) (string, bool) {
	dotgit := filepath.Join(dir, ".git")
	fi, err := os.Lstat(dotgit)
	if err != nil {
		return "", false
	}
	gitDir := dotgit
	switch {
	case fi.IsDir():
	case fi.Mode().IsRegular():
		raw, err := os.ReadFile(dotgit)
		if err != nil {
			return "", false
		}
		p, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
		if !ok {
			return "", false
		}
		if p = strings.TrimSpace(p); !filepath.IsAbs(p) {
			// Like git: relative to the physical directory, not to a
			// lexical path through a symbolic link.
			phys, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", false
			}
			p = filepath.Join(phys, p)
		}
		gitDir = p
	default:
		return "", false
	}
	// Relative paths in commondir are resolved against the physical git
	// directory too.
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", false
	}
	common := gitDir
	if raw, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		p := strings.TrimSpace(string(raw))
		if !filepath.IsAbs(p) {
			p = filepath.Join(gitDir, p)
		}
		common = p
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return "", false
	}
	r, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", false
	}
	return r, true
}

type callKind int

const (
	// callMutating: may change the repository (or is not known not to).
	callMutating callKind = iota
	// callReadOnly: never changes anything git reports; not cached.
	callReadOnly
	// callCacheable: read-only and answered from the query cache.
	callCacheable
)

// cacheableCalls are the exact argument lists (show-ref: with one operand)
// whose answers the query cache keeps. They were chosen from measurements
// of repeated questions within one command (ops TestGitCallsPerCommand).
var cacheableCalls = [][]string{
	topLevelArgs,
	commonDirArgs,
	inspectArgs,
	{"worktree", "list", "--porcelain", "-z"},
}

var (
	topLevelArgs  = []string{"rev-parse", "--show-toplevel"}
	commonDirArgs = []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}
	// inspectArgs asks for the top level, the git directory and the common
	// git directory of a work tree at once.
	inspectArgs = []string{"rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir"}
)

type derivedAnswer struct {
	args []string
	out  string
}

// derivedAnswers returns the answers of other cacheable queries contained in
// the successful output of args: the three-line answer to inspectArgs holds
// those of `rev-parse --show-toplevel` and `rev-parse --path-format=absolute
// --git-common-dir` (each prints one absolute path and a newline).
func derivedAnswers(args []string, out string) []derivedAnswer {
	if !slices.Equal(args, inspectArgs) {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 || !filepath.IsAbs(lines[0]) || !filepath.IsAbs(lines[2]) {
		return nil
	}
	return []derivedAnswer{
		{args: topLevelArgs, out: lines[0] + "\n"},
		{args: commonDirArgs, out: lines[2] + "\n"},
	}
}

// readOnlyCommands never write to the repository, whatever their options.
var readOnlyCommands = map[string]bool{
	"rev-parse": true, "rev-list": true, "for-each-ref": true, "show-ref": true, "ls-files": true,
	"merge-base": true, "check-ref-format": true, "cat-file": true, "ls-remote": true, "version": true,
}

// classify tells how an invocation relates to the query cache. Unknown
// invocations are mutations: misclassifying a reader only costs a cache
// flush, misclassifying a writer would serve stale answers.
func classify(args []string) callKind {
	for _, c := range cacheableCalls {
		if slices.Equal(args, c) {
			return callCacheable
		}
	}
	if len(args) == 4 && args[0] == "show-ref" && args[1] == "--verify" && args[2] == "--quiet" && !strings.HasPrefix(args[3], "-") {
		return callCacheable
	}
	noLocks := false
	for len(args) > 0 {
		switch {
		case args[0] == "--no-optional-locks":
			noLocks, args = true, args[1:]
			continue
		case args[0] == "-c" && len(args) > 1:
			args = args[2:]
			continue
		}
		break
	}
	if len(args) == 0 {
		return callMutating
	}
	cmd, rest := args[0], args[1:]
	switch {
	case readOnlyCommands[cmd]:
		return callReadOnly
	case cmd == "status" && noLocks:
		return callReadOnly
	case cmd == "hash-object" && !slices.Contains(rest, "-w"):
		return callReadOnly
	case cmd == "worktree" && len(rest) > 0 && rest[0] == "list":
		return callReadOnly
	case cmd == "remote" && (len(rest) == 0 || rest[0] == "get-url"):
		return callReadOnly
	case cmd == "symbolic-ref":
		operands := 0
		for _, a := range rest {
			switch {
			case a == "-q" || a == "--quiet" || a == "--short" || a == "--no-recurse":
			case strings.HasPrefix(a, "-"):
				return callMutating // -d/--delete, -m <reason>, unknown
			default:
				operands++
			}
		}
		if operands == 1 {
			return callReadOnly
		}
	case cmd == "config":
		get := false
		for _, a := range rest {
			switch a {
			case "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--list", "-l":
				get = true
			case "-z", "--null", "--name-only", "--show-origin", "--show-scope", "--local", "--global", "--system", "--worktree":
			default:
				if strings.HasPrefix(a, "-") {
					return callMutating // --unset, --add, --replace-all, --remove-section, unknown
				}
			}
		}
		if get {
			return callReadOnly
		}
	}
	return callMutating
}

// cacheableResult reports whether an answer may be kept: a success, or the
// expected negative answer of `show-ref --verify --quiet <ref>` (exit
// status 1: the ref does not exist). Any other failure (fatal errors with
// status 128, cancellation, a kill, a failure to start) is asked again.
func cacheableResult(ctx context.Context, args []string, err error) (*GitError, bool) {
	if err == nil {
		return nil, true
	}
	var ge *GitError
	if !errors.As(err, &ge) || ge.ExitCode != 1 || args[0] != "show-ref" || ctx.Err() != nil || errors.Is(ge.Err, ErrForceKilled) {
		return nil, false
	}
	return ge, true
}

// runCached runs one invocation with the query cache of ctx, if any (see
// classify): answers cacheable queries from the cache, and invalidates the
// cache after a mutation.
func runCached(ctx context.Context, o runOpts, dir string, args []string) (string, error) {
	sc, _ := ctx.Value(cacheScopeKey{}).(cacheScope)
	if sc.c == nil {
		return runProcess(ctx, o, dir, args...)
	}
	switch classify(args) {
	case callCacheable:
		// A done context runs git, which fails as it would without a cache.
		if sc.fresh || o.stdin != nil || o.force != nil || o.interactive || ctx.Err() != nil {
			return runProcess(ctx, o, dir, args...)
		}
		l := sc.c.lookup(dir, args)
		if l.hit {
			return l.out, l.err
		}
		out, err := runProcess(ctx, o, dir, args...)
		if ge, ok := cacheableResult(ctx, args, err); ok && l.storable {
			sc.c.store(l.ticket, out, ge)
			if ge == nil {
				for _, d := range derivedAnswers(args, out) {
					t := l.ticket
					t.key.args = argsKey(d.args)
					sc.c.store(t, d.out, nil)
				}
			}
		}
		return out, err
	case callReadOnly:
		return runProcess(ctx, o, dir, args...)
	}
	before, okBefore := repoKey(dir)
	out, err := runProcess(ctx, o, dir, args...)
	after, okAfter := repoKey(dir)
	sc.c.invalidate(before, after, okBefore, okAfter)
	return out, err
}

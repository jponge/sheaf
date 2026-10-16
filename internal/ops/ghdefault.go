package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
)

// ghDefaultTimeout bounds one `gh repo set-default` (it asks the service):
// add and adopt must not hang on gh.
const ghDefaultTimeout = 30 * time.Second

// ghDefaults sets the gh default repository of the primary checkouts that a
// command adds or adopts, so that gh commands run in a checkout (`gh pr
// list`, ...) target the reference repository: the recorded slug, which is
// upstream for a fork. The setting lives in the git configuration of the
// repository, shared by its worktrees, so the primary checkout is enough.
//
// It never fails the command. The calls of a command are serialized (one
// ghDefaults per command): when gh is missing, not authenticated or does not
// answer, the repository that found out gets one warning and no further gh
// call starts for the rest of the command, even with parallel jobs; other
// failures are warned about per repository. A nil *ghDefaults, or one
// without hosting, does nothing.
type ghDefaults struct {
	h       hosting.Hosting
	timeout time.Duration

	mu       sync.Mutex // held during each gh call
	unusable bool
}

func newGHDefaults(h hosting.Hosting) *ghDefaults {
	return &ghDefaults{h: h, timeout: ghDefaultTimeout}
}

// set makes entry's slug the gh default repository of the checkout primary,
// for a GitHub repository without one, and reports the outcome through emit.
// An existing default is kept: silently when it is that repository, with a
// note otherwise (also when several are configured).
func (g *ghDefaults) set(ctx context.Context, primary string, entry bundle.Repository, emit func(Event)) {
	if g == nil || g.h == nil || !entry.IsGitHub() {
		return
	}
	if ctx.Err() != nil {
		return
	}
	// An existing default is the user's (or gh's) choice: keep it.
	switch cur, err := readGHDefault(ctx, primary); {
	case err != nil:
		if ctx.Err() == nil {
			emit(Event{Message: fmt.Sprintf("warning: cannot read the gh default repository of %s: %s; not changed", entry.Name, firstLineOf(err.Error()))})
		}
		return
	case len(cur) == 1 && strings.EqualFold(cur[0].Slug, entry.Slug):
		return
	case len(cur) == 1:
		emit(Event{Message: fmt.Sprintf("note: gh default repository of %s is %s (reference: %s); kept", entry.Name, cur[0].Slug, entry.Slug)})
		return
	case len(cur) > 1:
		emit(Event{Message: fmt.Sprintf("note: several gh default repositories are configured for %s (%s); kept", entry.Name, describeAll(cur))})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unusable || ctx.Err() != nil {
		return
	}
	err := g.call(ctx, primary, entry.Slug)
	if err == nil {
		emit(Event{Message: "gh default repository: " + entry.Slug})
		return
	}
	if ctx.Err() != nil {
		return // cancelled: not gh's fault, nothing to warn about
	}
	var why, fix string
	switch {
	case errors.Is(err, hosting.ErrGHNotFound):
		why, fix = "gh not found", "install the GitHub CLI from https://cli.github.com/ and run `gh auth login`"
	case errors.Is(err, hosting.ErrGHNotAuthenticated):
		why, fix = "gh is not authenticated", "run `gh auth login`"
		if d := ghDetail(err); !strings.Contains(d, hosting.ErrGHNotAuthenticated.Error()) {
			why += " (" + d + ")" // what gh said, if it said something
		}
	case errors.Is(err, context.DeadlineExceeded):
		why, fix = fmt.Sprintf("gh did not answer within %v", g.timeout), "check gh and the network"
	default:
		emit(Event{Message: fmt.Sprintf("warning: gh default repository not set for %s (%s): %s; gh commands run there may ask which repository to use; fix: run `cd %s && gh repo set-default %s`",
			entry.Name, entry.Slug, ghDetail(err), shellQuote(primary), shellQuote(entry.Slug))})
		return
	}
	g.unusable = true
	emit(Event{Message: "warning: gh default repository not set: " + why +
		"; not tried for the other repositories of this command; gh commands run in their checkouts (e.g. `gh pr list`) may ask which repository to use; fix: " +
		fix + ", then run `sheaf adopt --recheck --all`"})
}

// ghDetail returns what gh said in err (the text after "exit status <n>: "
// in the gh adapter's errors), else the first line of err.
func ghDetail(err error) string {
	s := firstLineOf(err.Error())
	if i := strings.Index(s, "exit status "); i >= 0 {
		if _, after, ok := strings.Cut(s[i:], ": "); ok && after != "" {
			return after
		}
	}
	return s
}

// shellQuote returns s as one word for POSIX shells and fish: unchanged when
// made of safe characters only, else wrapped in single quotes, each single
// quote of s being closed, backslash-escaped and reopened.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%=", r))
	}) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (g *ghDefaults) call(ctx context.Context, primary, slug string) error {
	owner, name, err := splitSlug(slug)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.h.SetDefaultRepo(cctx, primary, owner, name)
}

// ghDefaultEntry is one gh default repository setting of a checkout: the
// git configuration variable remote.<Remote>.gh-resolved and its value.
type ghDefaultEntry struct {
	Remote string
	Value  string
	Slug   string // the repository it designates; empty when unknown
}

// describe returns "<slug> via <remote>".
func (e ghDefaultEntry) describe() string {
	slug := e.Slug
	if slug == "" {
		slug = fmt.Sprintf("an unknown repository (%q)", e.Value)
	}
	return slug + " via " + e.Remote
}

// remoteConfig reads the remote.* variables of the git configuration of
// dir with one git call, keyed by variable name as git prints it (section
// and variable names lower-cased, remote names as written).
func remoteConfig(ctx context.Context, dir string) (map[string][]string, error) {
	out, err := gitx.Run(ctx, dir, "config", "-z", "--get-regexp", `^remote\.`)
	if err != nil {
		if ge, ok := errors.AsType[*gitx.GitError](err); ok && ge.ExitCode == 1 {
			return map[string][]string{}, nil // no such variable
		}
		return nil, err
	}
	cfg := map[string][]string{}
	for entry := range strings.SplitSeq(out, "\x00") {
		if key, val, _ := strings.Cut(entry, "\n"); key != "" {
			cfg[key] = append(cfg[key], val)
		}
	}
	return cfg, nil
}

// lastValue returns the value git uses for a single-valued variable (the
// last one), or "" when unset.
func lastValue(cfg map[string][]string, key string) string {
	if vs := cfg[key]; len(vs) > 0 {
		return vs[len(vs)-1]
	}
	return ""
}

// ghDefaultsIn returns the gh default repository settings found in cfg (see
// remoteConfig), sorted by remote name. No gh call, no network.
//
// `gh repo set-default` stores remote.<name>.gh-resolved = base, <name>
// being the remote of the chosen repository (observed with gh on the
// user's machine, 2026-10-16): the slug is derived from that remote's
// configured URL. Any other value is taken as an owner/name slug; that is an
// inference, gh's source was not checked.
func ghDefaultsIn(cfg map[string][]string) []ghDefaultEntry {
	var out []ghDefaultEntry
	for key := range cfg {
		rest, ok := strings.CutPrefix(key, "remote.")
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(rest, ".gh-resolved")
		if !ok || name == "" {
			continue
		}
		e := ghDefaultEntry{Remote: name, Value: lastValue(cfg, key)}
		if e.Value == "base" {
			if _, slug, err := ParseRemoteURL(lastValue(cfg, "remote."+name+".url")); err == nil {
				e.Slug = slug
			}
		} else {
			e.Slug = e.Value
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b ghDefaultEntry) int { return strings.Compare(a.Remote, b.Remote) })
	return out
}

// readGHDefault returns the gh default repository settings of the checkout
// dir (see ghDefaultsIn).
func readGHDefault(ctx context.Context, dir string) ([]ghDefaultEntry, error) {
	cfg, err := remoteConfig(ctx, dir)
	if err != nil {
		return nil, err
	}
	return ghDefaultsIn(cfg), nil
}

// describeAll joins the descriptions of es.
func describeAll(es []ghDefaultEntry) string {
	var ds []string
	for _, e := range es {
		ds = append(ds, e.describe())
	}
	return strings.Join(ds, ", ")
}

package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
	"github.com/jponge/sheaf/internal/hosting"
)

// AddRequest selects the repositories to add: either Slugs ("owner/name"),
// or Org with All or Repos (names within the org).
type AddRequest struct {
	Slugs []string
	// As overrides the directory name; only valid when exactly one repository
	// is requested (one slug, or Org with a single entry in Repos).
	As    string
	Org   string
	All   bool
	Repos []string
	// IncludeArchived makes archived repositories of Org eligible.
	IncludeArchived bool
}

func (r AddRequest) validate() error {
	switch {
	case r.Org == "" && len(r.Slugs) == 0:
		return errors.New("nothing to add: give owner/repo slugs or an organization")
	case r.Org != "" && len(r.Slugs) > 0:
		return errors.New("slugs and an organization cannot be combined")
	case r.Org == "" && (r.All || len(r.Repos) > 0 || r.IncludeArchived):
		return errors.New("selecting repositories (all, repo names, archived) requires an organization")
	case r.All && len(r.Repos) > 0:
		return errors.New("all and explicit repository names cannot be combined")
	}
	if r.As != "" {
		single := len(r.Slugs) == 1 || r.Org != "" && !r.All && len(r.Repos) == 1
		if !single {
			return errors.New("--as is only valid when adding a single repository")
		}
		if err := bundle.ValidateRepoName(r.As); err != nil {
			return err
		}
	}
	for _, s := range r.Slugs {
		if _, _, err := splitSlug(s); err != nil {
			return err
		}
	}
	return nil
}

func splitSlug(s string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("invalid repository %q: expected owner/name", s)
	}
	return owner, name, nil
}

// SelectionRequiredError is returned by Add when an organization is given
// without All or Repos. Available lists the eligible repositories, sorted by
// name, for a UI to offer a choice (or to print in CLI mode).
type SelectionRequiredError struct {
	Org       string
	Available []hosting.Repo
}

func (e *SelectionRequiredError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "choose repositories of %s with --all or --repo <name>; available:", e.Org)
	if len(e.Available) == 0 {
		sb.WriteString(" none")
	}
	for _, r := range e.Available {
		sb.WriteString("\n  " + r.Name)
		if r.Fork {
			sb.WriteString(" (fork)")
		}
		if r.Archived {
			sb.WriteString(" (archived)")
		}
	}
	return sb.String()
}

// lookup is a requested repository after the hosting lookup.
type lookup struct {
	label string // what the user asked for, used when the lookup failed
	repo  hosting.Repo
	err   error
}

// Add resolves the requested repositories through h and clones each of them
// into <root>/<name>/<normalized default branch>/, at most jobs at a time.
//
// The returned error is reserved for invalid requests and failures that
// affect the whole request (e.g. the organization cannot be listed); per
// repository outcomes are in the Summary: repositories already in the bundle
// (same slug) are skipped, lookup failures, name clashes (fix with As),
// empty repositories and clone failures are failed results that do not stop
// the others.
//
// With Org, repositories are recorded with origin org:<Org>, using the
// organization name as spelled by the hosting service, and the organization
// is added to the bundle's sources (if missing) together with the first
// successful clone; when nothing was cloned it is added only if nothing
// failed either (empty organization with All, or everything already in the
// bundle). With Repos, a listing reported as truncated
// (hosting.ErrPossiblyTruncated) is completed by looking up the names
// missing from it; with All it is an error.
//
// sheaf.yaml is saved after each successful clone, never with an entry
// whose clone is incomplete: a clone that fails or is cancelled has its
// directory removed and is not recorded, while the clones that completed
// before stay recorded. So, whatever happens mid-way (error, cancellation,
// even a crash of sheaf except between a clone's end and its save), the
// file describes exactly the repositories on disk; saving only at the end
// would instead lose the record of completed clones on a crash.
func Add(ctx context.Context, w *Workspace, h hosting.Hosting, req AddRequest, jobs int, rep Reporter) (Summary, error) {
	if err := req.validate(); err != nil {
		return Summary{}, err
	}
	var (
		specs  []lookup
		origin = bundle.OriginExplicit
		source string // organization to track, if any
	)
	if req.Org != "" {
		repos, org, err := selectFromOrg(ctx, h, req)
		if err != nil {
			return Summary{}, err
		}
		for _, r := range repos {
			specs = append(specs, lookup{label: r.Slug(), repo: r})
		}
		origin = bundle.OriginOrgPrefix + org
		source = org
	} else {
		seen := map[string]bool{}
		for _, s := range req.Slugs {
			if seen[strings.ToLower(s)] {
				continue
			}
			seen[strings.ToLower(s)] = true
			owner, name, _ := splitSlug(s)
			r, err := h.Repo(ctx, owner, name)
			specs = append(specs, lookup{label: s, repo: r, err: err})
		}
	}
	sum := Execute(ctx, jobs, planClones(w, specs, req.As, origin, source), rep)
	// Successful clones recorded the source with their entry. Nothing was
	// cloned: track the organization anyway when nothing failed either (an
	// empty organization selected with All, or everything already present).
	if source != "" && sum.Count(StatusDone) == 0 && sum.Count(StatusFailed) == 0 {
		emit := func(e Event) { e.Repo, e.Phase, e.Kind = source, PhaseResolve, KindProgress; rep.Report(e) }
		if rep == nil {
			emit = nil
		}
		if err := w.updateCtx(ctx, emit, func(b *bundle.Bundle) error { return addSourceIfMissing(b, source) }); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func addSourceIfMissing(b *bundle.Bundle, org string) error {
	for _, s := range b.Sources {
		if strings.EqualFold(s.Org, org) {
			return nil
		}
	}
	return b.AddSource(org)
}

// selectFromOrg lists the organization and applies the selection. It returns
// the selected repositories and the organization's canonical name.
func selectFromOrg(ctx context.Context, h hosting.Hosting, req AddRequest) ([]hosting.Repo, string, error) {
	listed, err := h.ListRepos(ctx, req.Org)
	truncated := errors.Is(err, hosting.ErrPossiblyTruncated) && !req.All && len(req.Repos) > 0
	if err != nil && !truncated {
		return nil, "", fmt.Errorf("listing repositories of %s: %w", req.Org, err)
	}
	org := req.Org
	if len(listed) > 0 {
		org = listed[0].Owner
	}
	eligible := hosting.Filter(listed, hosting.FilterOpts{IncludeArchived: req.IncludeArchived})
	if req.All {
		return eligible, org, nil
	}
	if len(req.Repos) == 0 {
		return nil, "", &SelectionRequiredError{Org: org, Available: eligible}
	}
	var out []hosting.Repo
	seen := map[string]bool{}
	for _, name := range req.Repos {
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		r, ok := findRepo(eligible, name)
		if !ok && truncated {
			if _, listedToo := findRepo(listed, name); !listedToo {
				got, err := h.Repo(ctx, req.Org, name)
				if err != nil {
					return nil, "", err
				}
				if got.Archived && !req.IncludeArchived {
					return nil, "", fmt.Errorf("repository %s is archived; pass --include-archived to add it", got.Slug())
				}
				r, ok = got, true
			}
		}
		if !ok {
			if a, ok := findRepo(listed, name); ok && a.Archived {
				return nil, "", fmt.Errorf("repository %s is archived; pass --include-archived to add it", a.Slug())
			}
			return nil, "", fmt.Errorf("%s has no repository %q: %w", org, name, hosting.ErrNotFound)
		}
		out = append(out, r)
	}
	return out, org, nil
}

func findRepo(repos []hosting.Repo, name string) (hosting.Repo, bool) {
	for _, r := range repos {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return hosting.Repo{}, false
}

func failedTask(repo string, err error) Task {
	return Task{Repo: repo, Phase: PhaseResolve, Run: func(context.Context, func(Event)) (string, error) { return "", err }}
}

func skippedTask(repo, reason string) Task {
	return Task{Repo: repo, Phase: PhaseResolve, Run: func(context.Context, func(Event)) (string, error) { return "", Skip(reason) }}
}

// planClones turns lookups into tasks, checking names against the bundle and
// against each other (case-insensitively, like the bundle does).
func planClones(w *Workspace, specs []lookup, as, origin, source string) []Task {
	b := w.Bundle()
	taken := map[string]bool{}
	for _, r := range b.Repositories {
		taken[strings.ToLower(r.Name)] = true
	}
	var tasks []Task
	for _, s := range specs {
		if s.err != nil {
			tasks = append(tasks, failedTask(s.label, s.err))
			continue
		}
		r := s.repo
		name := r.Name
		if as != "" {
			name = as
		}
		if e := existingByKey(b, "", r.Slug()); e != nil {
			tasks = append(tasks, skippedTask(e.Name, fmt.Sprintf("%s is already in the bundle as %s", r.Slug(), e.Name)))
			continue
		}
		if err := bundle.ValidateRepoName(name); err != nil {
			tasks = append(tasks, failedTask(s.label, err))
			continue
		}
		if taken[strings.ToLower(name)] {
			tasks = append(tasks, failedTask(name, fmt.Errorf("cannot add %s: a repository named %q already exists in the bundle; use --as <dir> to choose another directory name", r.Slug(), name)))
			continue
		}
		if r.DefaultBranch == "" {
			tasks = append(tasks, failedTask(name, fmt.Errorf("%s is empty (no default branch); nothing to clone", r.Slug())))
			continue
		}
		branchDir, err := bundle.NormalizeBranch(r.DefaultBranch)
		if err != nil {
			tasks = append(tasks, failedTask(name, err))
			continue
		}
		taken[strings.ToLower(name)] = true
		entry := bundle.Repository{Name: name, Slug: r.Slug(), URL: r.CloneURL, DefaultBranch: r.DefaultBranch, Origin: origin}
		tasks = append(tasks, Task{Repo: name, Phase: PhaseClone, Run: func(ctx context.Context, emit func(Event)) (string, error) {
			return cloneRepo(ctx, w, entry, branchDir, source, emit)
		}})
	}
	return tasks
}

// existingByKey finds the repository with the given host (empty means
// github.com) and slug, case-insensitively.
func existingByKey(b *bundle.Bundle, host, slug string) *bundle.Repository {
	want := bundle.Repository{Host: host}.HostName()
	for i := range b.Repositories {
		if r := b.Repositories[i]; strings.EqualFold(r.HostName(), want) && strings.EqualFold(r.Slug, slug) {
			return &b.Repositories[i]
		}
	}
	return nil
}

// cloneRepo creates <root>/<name> (which must not exist), clones into it and
// records the entry. On any failure the repository directory is removed.
func cloneRepo(ctx context.Context, w *Workspace, entry bundle.Repository, branchDir, source string, emit func(Event)) (string, error) {
	repoDir := bundle.RepoDir(w.Root, entry.Name)
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%s already exists but is not in the bundle: adopt it, or use --as <dir> to clone elsewhere", repoDir)
		}
		return "", err
	}
	dest := filepath.Join(repoDir, branchDir)
	emit(Event{Message: "cloning " + entry.URL + " into " + dest})
	err := gitx.Clone(ctx, entry.URL, dest, entry.DefaultBranch)
	if err == nil {
		err = w.updateCtx(ctx, emit, func(b *bundle.Bundle) error {
			if source != "" {
				if err := addSourceIfMissing(b, source); err != nil {
					return err
				}
			}
			return b.AddRepository(entry)
		})
	}
	if err != nil {
		if rerr := os.RemoveAll(repoDir); rerr != nil {
			err = fmt.Errorf("%w (cleaning up %s also failed: %v)", err, repoDir, rerr)
		}
		return "", err
	}
	return "cloned into " + dest, nil
}

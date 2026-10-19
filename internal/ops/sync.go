package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"

	"github.com/jponge/sheaf/internal/hosting"
)

// ProposalKind classifies a Proposal of Sync.
type ProposalKind string

const (
	// ProposalNew is for a repository of a tracked organization that is not in the
	// bundle and not excluded. Accepting it clones it (SyncAddNew);
	// declining it for good records it in the source's exclude list
	// (SyncExclude).
	ProposalNew ProposalKind = "new"
	// ProposalGone is for a repository recorded with origin org:<name> that the
	// organization no longer lists (deleted, transferred or renamed: the
	// hosting service lists repositories under their current name, so a
	// rename shows up as one gone and one new proposal). Accepting it removes
	// the repository (SyncRemoveGone, with all the safety of Remove).
	ProposalGone ProposalKind = "gone"
	// ProposalArchived is for a repository of the bundle that the organization
	// lists as archived. Informational only: nothing is applied for it.
	ProposalArchived ProposalKind = "archived"
	// ProposalMoved is for a repository recorded with origin org:<name> that the
	// organization no longer lists but that still exists under another
	// owner or name (transferred or renamed; Proposal.Repo is its current
	// identity). Informational only.
	ProposalMoved ProposalKind = "moved"
)

// Proposal is one difference between an organization listing and the
// bundle.
//
// Key is stable across runs ("<kind>:<owner>/<name>", lower case) and
// unique within a SyncReport, so a UI can use it as the key of a selection.
// A TUI (tui.MultiSelect) maps a proposal to an Item{Key: Key, Label: Name,
// Detail: Slug and Description or Note, Tags: Kind plus "fork"/"archived"},
// with nothing preselected (adding clones, removing deletes: both must be
// explicit choices) and ProposalArchived items Disabled; unselected new
// proposals are simply left alone (excluding them is a separate, explicit
// choice).
type Proposal struct {
	Key  string
	Kind ProposalKind
	// Org is the tracked organization, as spelled in sheaf.yaml.
	Org string
	// Name is the repository name on the hosting service (new), or the
	// directory name in the bundle (gone, archived).
	Name string
	// Slug is owner/name: from the listing (new, archived) or from
	// sheaf.yaml (gone).
	Slug string
	// Repo is the listed repository (new, archived), or the repository's
	// current identity (moved); zero for gone.
	Repo hosting.Repo
	// URL is the clone URL: from the listing (new, archived, moved) or from
	// sheaf.yaml (gone).
	URL string
	// Note explains the proposal or a problem with applying it (e.g. a
	// directory name already used in the bundle), empty if none.
	Note string
	// MassRemoval is set on the gone proposals of an organization whose
	// gone repositories are more than half of its tracked ones (and more
	// than one): SyncRemoveGone refuses them unless explicitly allowed.
	MassRemoval bool
	// PossiblyMoved is set on a new proposal whose name or URL matches an
	// entry of another organization that was not synced (its slug): the
	// repository may have moved here. SyncAddNew skips such proposals.
	PossiblyMoved string
}

// OrgSync is the listing outcome of one tracked organization.
type OrgSync struct {
	Org    string // as spelled in sheaf.yaml
	Listed int    // number of repositories listed (archived included)
	// Truncated is set when the listing may be incomplete
	// (hosting.ErrPossiblyTruncated): no gone proposal is made for the
	// organization since absence from the listing proves nothing.
	Truncated bool
	// Err is set when the organization could not be listed; no proposal is
	// made for it.
	Err error
	// Warnings and Notes are messages for the user (Notes are informational).
	Warnings, Notes []string
	// MassRemoval: see Proposal.MassRemoval.
	MassRemoval bool
	// Unconfirmed lists the gone candidates whose lookup failed (not
	// proposed); SyncReport.Err reports them.
	Unconfirmed []string
}

// SyncOptions configures Sync.
type SyncOptions struct {
	// Orgs restricts the sync to these tracked organizations (matched
	// case-insensitively); empty means every source of the bundle.
	Orgs []string
	// IncludeArchived makes archived repositories eligible as new.
	IncludeArchived bool
}

// SyncReport is the result of Sync.
type SyncReport struct {
	Orgs []OrgSync // in the order of sheaf.yaml (sorted by org)
	// Proposals are sorted by org, then kind (new, gone, archived), then name.
	Proposals []Proposal
}

// Err returns nil when every organization was listed and every gone
// candidate confirmed, and an error naming the failures otherwise.
func (r SyncReport) Err() error {
	var errs []error
	for _, o := range r.Orgs {
		if o.Err != nil {
			errs = append(errs, o.Err)
		}
		if len(o.Unconfirmed) > 0 {
			errs = append(errs, fmt.Errorf("could not confirm whether %s (%s) are gone", strings.Join(o.Unconfirmed, ", "), o.Org))
		}
	}
	return errors.Join(errs...)
}

// Sync compares the listings of the tracked organizations (the sources of
// sheaf.yaml, or opts.Orgs among them) with the bundle and returns the
// differences as proposals. It changes nothing.
//
// Only github.com repositories of the bundle (Repository.IsGitHub) are
// compared, by slug, case-insensitively:
//
//   - a listed repository with no bundle entry is proposed as new, unless
//     it is in the source's exclude list or archived (unless
//     opts.IncludeArchived);
//   - a listed repository that is archived and has a bundle entry (whatever
//     its origin) is reported as archived;
//   - a bundle entry with origin org:<org> that the listing lacks is a gone
//     candidate, unless the listing may be truncated. Each candidate is
//     looked up (h.Repo): not found makes it gone; found under the same
//     slug means the listing is incomplete (warning, no proposal); found
//     under another slug makes it moved (and the new proposal for that
//     slug, if any, is dropped); another error is a warning. When more
//     than half of the org's tracked entries (and more than one) are gone,
//     the org and its gone proposals are flagged MassRemoval;
//   - a listed repository whose slug is recorded under an SSH alias of
//     github.com (see looksLikeGitHubAlias) is not proposed; a note says so;
//   - a new proposal whose name or clone URL matches an entry with origin
//     org:<other> where <other> is not synced in this run gets
//     PossiblyMoved (and a note), since only syncing that organization can
//     tell whether it moved;
//   - an empty listing of an organization with tracked entries flags its
//     gone proposals MassRemoval too, whatever their number;
//   - gone candidates whose lookup failed are listed in Unconfirmed and
//     make SyncReport.Err non-nil.
//
// A failed listing is recorded in its OrgSync and does not stop the other
// organizations (see SyncReport.Err). The returned error is reserved for
// invalid options, a bundle without sources and cancellation.
func Sync(ctx context.Context, w *Workspace, h hosting.Hosting, opts SyncOptions) (SyncReport, error) {
	b := w.Bundle()
	sources, err := syncSources(b, opts.Orgs)
	if err != nil {
		return SyncReport{}, err
	}
	var rep SyncReport
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return SyncReport{}, err
		}
		repos, err := h.ListRepos(ctx, src.Org)
		o := OrgSync{Org: src.Org, Listed: len(repos)}
		if err != nil {
			if ctx.Err() != nil {
				return SyncReport{}, ctx.Err()
			}
			if errors.Is(err, hosting.ErrPossiblyTruncated) {
				o.Truncated = true
			} else {
				o.Listed, o.Err = 0, fmt.Errorf("listing repositories of %s: %w", src.Org, err)
				rep.Orgs = append(rep.Orgs, o)
				continue
			}
		}
		props, notes := diffOrg(b, src, repos, !o.Truncated, opts.IncludeArchived)
		o.Notes = notes
		props, err = confirmGone(ctx, b, h, src, props, len(repos), &o)
		if err != nil {
			return SyncReport{}, err
		}
		rep.Orgs = append(rep.Orgs, o)
		rep.Proposals = append(rep.Proposals, props...)
	}
	// A repository moved into a tracked organization is reported as moved,
	// not also as new.
	movedTo := map[string]bool{}
	for _, p := range rep.Proposals {
		if p.Kind == ProposalMoved {
			movedTo[strings.ToLower(p.Repo.Slug())] = true
		}
	}
	rep.Proposals = slices.DeleteFunc(rep.Proposals, func(p Proposal) bool {
		return p.Kind == ProposalNew && movedTo[strings.ToLower(p.Slug)]
	})
	markPossiblyMoved(b, sources, rep.Proposals)
	kindRank := map[ProposalKind]int{ProposalNew: 0, ProposalGone: 1, ProposalMoved: 2, ProposalArchived: 3}
	slices.SortStableFunc(rep.Proposals, func(x, y Proposal) int {
		if c := strings.Compare(strings.ToLower(x.Org), strings.ToLower(y.Org)); c != 0 {
			return c
		}
		if c := kindRank[x.Kind] - kindRank[y.Kind]; c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)); c != 0 {
			return c
		}
		return strings.Compare(x.Name, y.Name)
	})
	return rep, nil
}

// confirmGone looks up the gone candidates among props (see Sync) and
// returns the proposals with the candidates resolved; warnings and the
// mass-removal flag go to o. Only cancellation is returned as an error.
func confirmGone(ctx context.Context, b *bundle.Bundle, h hosting.Hosting, src bundle.Source, props []Proposal, listed int, o *OrgSync) ([]Proposal, error) {
	var out, gone []Proposal
	for _, p := range props {
		if p.Kind != ProposalGone {
			out = append(out, p)
			continue
		}
		owner, name, _ := strings.Cut(p.Slug, "/")
		got, err := h.Repo(ctx, owner, name)
		switch {
		case err == nil && strings.EqualFold(got.Slug(), p.Slug):
			o.Warnings = append(o.Warnings, fmt.Sprintf("listing for org %s looks incomplete (missing visible repo %s); check gh authentication scope", src.Org, got.Slug()))
		case err == nil:
			p.Key, p.Kind, p.Repo, p.URL = proposalKey(ProposalMoved, p.Slug), ProposalMoved, got, got.CloneURL
			p.Note = fmt.Sprintf("moved %s → %s (transferred or renamed); nothing is changed: the clone keeps working through GitHub's redirect, to track it under its new name remove it and add %s",
				p.Slug, got.Slug(), got.Slug())
			out = append(out, p)
		case errors.Is(err, hosting.ErrNotFound):
			gone = append(gone, p)
		default:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			o.Warnings = append(o.Warnings, fmt.Sprintf("could not confirm that %s is gone, not proposed: %v", p.Slug, err))
			o.Unconfirmed = append(o.Unconfirmed, p.Slug)
		}
	}
	tracked := 0
	for _, e := range b.Repositories {
		if org, ok := strings.CutPrefix(e.Origin, bundle.OriginOrgPrefix); ok && strings.EqualFold(org, src.Org) && e.IsGitHub() {
			tracked++
		}
	}
	if len(gone) > 1 && 2*len(gone) > tracked || len(gone) > 0 && listed == 0 {
		o.MassRemoval = true
		for i := range gone {
			gone[i].MassRemoval = true
		}
	}
	return append(out, gone...), nil
}

// markPossiblyMoved sets PossiblyMoved on the new proposals whose name or
// clone URL matches an entry with origin org:<other>, <other> not among
// sources.
func markPossiblyMoved(b *bundle.Bundle, sources []bundle.Source, props []Proposal) {
	synced := func(org string) bool {
		return slices.ContainsFunc(sources, func(s bundle.Source) bool { return strings.EqualFold(s.Org, org) })
	}
	for i := range props {
		p := &props[i]
		if p.Kind != ProposalNew {
			continue
		}
		for _, e := range b.Repositories {
			org, ok := strings.CutPrefix(e.Origin, bundle.OriginOrgPrefix)
			if !ok || synced(org) || strings.EqualFold(e.Slug, p.Slug) {
				continue
			}
			if strings.EqualFold(e.Name, p.Repo.Name) || p.Repo.CloneURL != "" && e.URL == p.Repo.CloneURL {
				p.PossiblyMoved = e.Slug
				p.Note = "possibly moved from " + e.Slug + "; run sync for that org too"
				break
			}
		}
	}
}

// syncSources returns the sources to sync, sorted by org: all of them, or
// those named in orgs (case-insensitively, each one must be a source).
func syncSources(b *bundle.Bundle, orgs []string) ([]bundle.Source, error) {
	if len(b.Sources) == 0 {
		return nil, errors.New("the bundle tracks no organization: start tracking one with sheaf add --org <org>")
	}
	all := slices.Clone(b.Sources)
	slices.SortFunc(all, func(x, y bundle.Source) int { return strings.Compare(strings.ToLower(x.Org), strings.ToLower(y.Org)) })
	if len(orgs) == 0 {
		return all, nil
	}
	var known []string
	for _, s := range all {
		known = append(known, s.Org)
	}
	for _, o := range orgs {
		if !slices.ContainsFunc(all, func(s bundle.Source) bool { return strings.EqualFold(s.Org, o) }) {
			return nil, fmt.Errorf("%q is not a tracked organization (tracked: %s)", o, strings.Join(known, ", "))
		}
	}
	var out []bundle.Source
	for _, s := range all {
		if slices.ContainsFunc(orgs, func(o string) bool { return strings.EqualFold(s.Org, o) }) {
			out = append(out, s)
		}
	}
	return out, nil
}

func proposalKey(kind ProposalKind, slug string) string {
	return string(kind) + ":" + strings.ToLower(slug)
}

// diffOrg computes the proposals of one organization from its listing.
// complete tells whether the listing is known to be complete (gone
// proposals are only made then).
func diffOrg(b *bundle.Bundle, src bundle.Source, repos []hosting.Repo, complete, includeArchived bool) (out []Proposal, notes []string) {
	bySlug := map[string]bundle.Repository{}
	byName := map[string]bundle.Repository{}
	for _, r := range b.Repositories {
		byName[strings.ToLower(r.Name)] = r
		if r.IsGitHub() {
			bySlug[strings.ToLower(r.Slug)] = r
		}
	}
	excluded := map[string]bool{}
	for _, e := range src.Exclude {
		excluded[strings.ToLower(e)] = true
	}
	listedSlugs := map[string]bool{}
	for _, r := range repos {
		listedSlugs[strings.ToLower(r.Slug())] = true
		if e, ok := bySlug[strings.ToLower(r.Slug())]; ok {
			if r.Archived {
				out = append(out, Proposal{
					Key: proposalKey(ProposalArchived, r.Slug()), Kind: ProposalArchived, Org: src.Org,
					Name: e.Name, Slug: r.Slug(), Repo: r, URL: r.CloneURL,
					Note: "archived on GitHub (read-only); nothing is changed, remove it with sheaf remove " + e.Name + " if it is no longer needed",
				})
			}
			continue
		}
		if r.Archived && !includeArchived || excluded[strings.ToLower(r.Name)] {
			continue
		}
		if e := existingOnAliasHost(b, r.Slug()); e != nil {
			notes = append(notes, fmt.Sprintf("%s is already in the bundle as %s (host %s): not proposed", r.Slug(), e.Name, e.Host))
			continue
		}
		var note string
		switch e, clash := byName[strings.ToLower(r.Name)]; {
		case clash:
			note = fmt.Sprintf("the directory name %q is already used by %s: add it with sheaf add --org %s --repo %s --as <dir>", e.Name, e.Slug, src.Org, r.Name)
		case bundle.ValidateRepoName(r.Name) != nil:
			note = fmt.Sprintf("%q is not usable as a directory name: add it with sheaf add --org %s --repo %s --as <dir>", r.Name, src.Org, r.Name)
		case r.DefaultBranch == "":
			note = "empty repository (no default branch): nothing to clone yet"
		}
		out = append(out, Proposal{
			Key: proposalKey(ProposalNew, r.Slug()), Kind: ProposalNew, Org: src.Org,
			Name: r.Name, Slug: r.Slug(), Repo: r, URL: r.CloneURL, Note: note,
		})
	}
	if !complete {
		return out, notes
	}
	for _, e := range b.Repositories {
		org, ok := strings.CutPrefix(e.Origin, bundle.OriginOrgPrefix)
		if !ok || !strings.EqualFold(org, src.Org) || !e.IsGitHub() || listedSlugs[strings.ToLower(e.Slug)] {
			continue
		}
		out = append(out, Proposal{
			Key: proposalKey(ProposalGone, e.Slug), Kind: ProposalGone, Org: src.Org,
			Name: e.Name, Slug: e.Slug, URL: e.URL,
			Note: "no longer listed in " + src.Org + ": deleted, transferred or renamed (a renamed repository is also proposed as new under its new name)",
		})
	}
	return out, notes
}

// SyncAddNewWithOptions clones the repositories of new proposals like Add
// does for an organization (origin org:<owner>, canonical owner spelling,
// sheaf.yaml saved after each clone), at most jobs at a time, and returns the
// per repository results (named after the directory). Empty repositories are
// skipped. Proposals of another kind are failed results; so are directory
// name clashes, also between the proposals of different organizations.
//
// Forks are detected like Add does: unless opts.NoFork, the user's forks are
// listed once through opts.Hosting, and a repository with a fork is cloned
// with origin = the fork and upstream = the repository.
func SyncAddNewWithOptions(ctx context.Context, w *Workspace, props []Proposal, opts CloneOptions, jobs int, rep Reporter) Summary {
	type planned struct {
		spec  lookup
		owner string
	}
	var tasks []Task
	var clones []planned
	seen := map[string]bool{}
	for _, p := range props {
		if p.Kind != ProposalNew {
			tasks = append(tasks, failedTask(p.Name, fmt.Errorf("%s is a %s proposal, not a new repository", p.Slug, p.Kind)))
			continue
		}
		if p.PossiblyMoved != "" {
			tasks = append(tasks, skippedTask(p.Repo.Name, p.Slug+" was possibly moved from "+p.PossiblyMoved+"; run sync for that org too"))
			continue
		}
		if p.Repo.DefaultBranch == "" {
			tasks = append(tasks, skippedTask(p.Repo.Name, p.Slug+" is an empty repository (no default branch): nothing to clone yet"))
			continue
		}
		if seen[strings.ToLower(p.Repo.Name)] {
			tasks = append(tasks, failedTask(p.Slug, fmt.Errorf("cannot add %s: another selected repository is also named %q; add it with sheaf add --org %s --repo %s --as <dir>", p.Slug, p.Repo.Name, p.Org, p.Repo.Name)))
			continue
		}
		seen[strings.ToLower(p.Repo.Name)] = true
		owner := p.Repo.Owner
		if owner == "" {
			owner = p.Org
		}
		clones = append(clones, planned{spec: lookup{label: p.Slug, repo: p.Repo}, owner: owner})
	}
	var forks forkIndex
	if !opts.NoFork {
		var specs []lookup
		for _, c := range clones {
			specs = append(specs, c.spec)
		}
		forks = detectForks(ctx, opts.Hosting, clonable(w.Bundle(), specs), rep)
	}
	gd := newGHDefaults(opts.Hosting)
	for _, c := range clones {
		tasks = append(tasks, planClones(w, gitx.Net{Interactive: opts.Interactive}, []lookup{c.spec}, "", bundle.OriginOrgPrefix+c.owner, "", forks, gd)...)
	}
	return Execute(ctx, jobs, tasks, rep)
}

// SyncExclude records the repositories of new proposals in the exclude
// lists of their sources, so that Sync stops proposing them, in a single
// update of sheaf.yaml. It fails, changing nothing, if a proposal is not a
// new one or its source is no longer tracked.
func SyncExclude(ctx context.Context, w *Workspace, props []Proposal) error {
	for _, p := range props {
		if p.Kind != ProposalNew {
			return fmt.Errorf("only new repositories can be excluded; %s is a %s proposal", p.Slug, p.Kind)
		}
	}
	return w.updateCtx(ctx, nil, func(b *bundle.Bundle) error {
		for _, p := range props {
			if err := b.AddExclude(p.Org, p.Repo.Name); err != nil {
				return err
			}
		}
		return nil
	})
}

// SyncRemoveOptions configures SyncRemoveGone.
type SyncRemoveOptions struct {
	Remove RemoveOptions
	// AllowMassRemoval accepts proposals flagged MassRemoval.
	AllowMassRemoval bool
}

// SyncRemoveGone removes the repositories of gone proposals through Remove
// with opts (same safety: fetch, risk checks, refusal unless opts.Force,
// opts.Confirm), at most jobs at a time, and returns the per repository
// results (a refusal is a failed result with a *RisksError). opts.Confirm
// must be safe for concurrent use when jobs > 1. Proposals of another kind,
// and MassRemoval ones unless AllowMassRemoval, are failed results. Each
// task first checks that the bundle entry still has the proposal's slug
// and origin, and skips it ("changed since sync") otherwise.
func SyncRemoveGone(ctx context.Context, w *Workspace, props []Proposal, sopts SyncRemoveOptions, jobs int, rep Reporter) Summary {
	opts := sopts.Remove
	var tasks []Task
	for _, p := range props {
		if p.Kind != ProposalGone {
			tasks = append(tasks, failedTask(p.Name, fmt.Errorf("%s is a %s proposal, not a gone repository", p.Slug, p.Kind)))
			continue
		}
		if p.MassRemoval && !sopts.AllowMassRemoval {
			args := []string{"sync", "--org", p.Org, "--remove-gone", "--yes"}
			if opts.Force {
				args = append(args, "--force")
			}
			tasks = append(tasks, failedTask(p.Name, fmt.Errorf("refusing to remove %s: most repositories of %s look gone, which is more likely an incomplete listing (authentication scope) than real deletions; check, then run: %s",
				p.Slug, p.Org, CommandLine(append(args, "--allow-mass-removal")...))))
			continue
		}
		tasks = append(tasks, Task{Repo: p.Name, Phase: PhaseRemove, Run: func(ctx context.Context, emit func(Event)) (string, error) {
			e, ok := w.Bundle().Repository(p.Name)
			if !ok || !strings.EqualFold(e.Slug, p.Slug) || !strings.EqualFold(e.Origin, bundle.OriginOrgPrefix+p.Org) {
				return "", Skip("changed since sync: " + p.Name + " is no longer " + p.Slug + " with origin org:" + p.Org + "; run sync again")
			}
			o := opts
			o.ExpectedGone = p.Slug
			return Remove(ctx, w, p.Name, o, emit)
		}})
	}
	return Execute(ctx, jobs, tasks, rep)
}

// CloneOptions configures the fork detection of SyncAddNewWithOptions.
type CloneOptions struct {
	// Hosting lists the user's forks and sets the gh default repository of
	// the clones; nil disables both.
	Hosting hosting.Hosting
	// NoFork disables fork detection (see AddRequest.NoFork).
	NoFork bool
	// Interactive clones in interactive git mode (see gitx.Net).
	Interactive bool
}

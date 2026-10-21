package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jponge/sheaf/internal/ops"
	"github.com/spf13/cobra"
)

// addSyncCommands registers the sync command (organization sources vs. the bundle).
func addSyncCommands(root *cobra.Command, d Deps) { root.AddCommand(newSyncCmd(d)) }

type syncFlags struct {
	orgs                []string
	includeArchived     bool
	asJSON              bool
	addNew, removeGone  bool
	exclude             []string
	yes, force, offline bool
	allowMass           bool
	noFork              bool
}

func (fl syncFlags) validate() error {
	applying := fl.addNew || fl.removeGone || len(fl.exclude) > 0
	switch {
	case fl.asJSON && applying:
		return errors.New("--json only lists the proposals; it cannot be combined with --add-new, --exclude or --remove-gone")
	case fl.offline:
		return errors.New("--offline is not supported by sync: the fetch is what reveals that a gone repository's remote no longer exists " +
			"(its clone may then hold the only copy of its history); use sheaf remove <repo> --offline for a single repository if you must")
	case fl.removeGone && !fl.yes:
		return errors.New("--remove-gone deletes repositories: confirm with --yes (sheaf never prompts in CLI mode)")
	case !fl.removeGone && (fl.yes || fl.force || fl.allowMass):
		return errors.New("--yes, --force and --allow-mass-removal only apply to --remove-gone")
	}
	return nil
}

func newSyncCmd(d Deps) *cobra.Command {
	var fl syncFlags
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Compare the tracked organizations with the bundle and apply chosen changes",
		Long: "List the repositories of each tracked organization (the sources of sheaf.yaml, or the\n" +
			"--org ones) and compare them with the bundle:\n" +
			"  new       listed, not in the bundle, not excluded (archived ones need --include-archived)\n" +
			"  gone      recorded with origin org:<org> but no longer listed (deleted, transferred or\n" +
			"            renamed: a renamed repository also shows up as new under its new name)\n" +
			"  moved     recorded with origin org:<org>, no longer listed, but found under another\n" +
			"            owner or name (informational; nothing is removed nor added)\n" +
			"  archived  in the bundle and archived on GitHub (informational)\n" +
			"Only github.com repositories are compared. Each gone candidate is looked up first; when a\n" +
			"listing may be truncated, nothing is proposed as gone for that organization. When more than\n" +
			"half of an organization's tracked repositories look gone (more than one), --remove-gone\n" +
			"refuses them unless --allow-mass-removal: an incomplete listing is the likelier cause (an\n" +
			"empty listing counts as such whenever the organization has tracked repositories). A gone\n" +
			"repository whose remote still answers is refused (gh authentication scope) unless --force.\n" +
			"Gone candidates that cannot be looked up are not proposed and make sync exit with 1.\n" +
			"With --org, a new repository whose name or URL matches a repository of an organization\n" +
			"not synced in this run is noted as possibly moved and skipped by --add-new.\n\n" +
			"Without action flags nothing changes. --exclude records declined new repositories in\n" +
			"sheaf.yaml so they are no longer proposed, --add-new clones the remaining new ones, and\n" +
			"--remove-gone --yes removes the gone ones with the checks of `sheaf remove` (refused when\n" +
			"data would be lost, unless --force; a remote that no longer exists counts as data at risk).\n" +
			"Actions run in that order.\n\n" +
			"In TUI mode, without action flags (and without --json), a menu offers the new and gone\n" +
			"repositories (nothing preselected; gone ones refused as a mass removal are shown but not\n" +
			"selectable), asks to confirm removals, and applies the choices as above (never with\n" +
			"--force or --allow-mass-removal). New repositories left unselected are not excluded:\n" +
			"the next sync proposes them again (use --exclude to decline them for good).\n\n" +
			"--json prints {version, bundle, orgs: [{org, listed, truncated, mass_removal, error?,\n" +
			"warnings?, notes?, unconfirmed?}], proposals: [{key, kind, org, name (directory name for gone,\n" +
			"moved and archived), slug (as recorded for gone and moved), url, default_branch?, fork?,\n" +
			"archived?, description?, current_slug? (moved), mass_removal? (gone), possibly_moved? (new),\n" +
			"note?}]}; '?' marks keys present\n" +
			"only when applicable: fork and archived only when the repository was listed or found.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error { return runSync(c, d, fl) },
	}
	f := cmd.Flags()
	f.StringArrayVar(&fl.orgs, "org", nil, "only this tracked organization (repeatable)")
	f.BoolVar(&fl.includeArchived, "include-archived", false, "also propose archived repositories as new")
	f.BoolVar(&fl.asJSON, "json", false, "print the proposals as a JSON document (no changes)")
	f.BoolVar(&fl.addNew, "add-new", false, "clone every new repository (except the --exclude ones)")
	f.BoolVar(&fl.noFork, "no-fork", false, "when adding, do not look for your forks: clone each repository as origin")
	f.StringArrayVar(&fl.exclude, "exclude", nil, "record a new repository ([org/]name) as declined so that sync stops proposing it (repeatable)")
	f.BoolVar(&fl.removeGone, "remove-gone", false, "remove the repositories gone from their organization (requires --yes)")
	f.BoolVarP(&fl.yes, "yes", "y", false, "with --remove-gone: confirm the removals")
	f.BoolVar(&fl.force, "force", false, "with --remove-gone: remove even if data would be lost")
	f.BoolVar(&fl.allowMass, "allow-mass-removal", false, "with --remove-gone: also remove when most repositories of an organization look gone")
	// Accepted only to explain why it is refused (remove suggests it).
	f.BoolVar(&fl.offline, "offline", false, "not supported (see sheaf remove --offline)")
	_ = f.MarkHidden("offline")
	return cmd
}

// listingError is the failed listing of an organization in the error of
// sync (see syncErr): its warning gave the reason (Err).
type listingError struct {
	Org string
	Err error
}

func (e *listingError) Error() string { return "listing repositories of " + e.Org + " failed" }

func (e *listingError) Unwrap() error { return e.Err }

// Summary is the reason on one line, for the message of the home screen
// (see errorLine), which shows no warning.
func (e *listingError) Summary() string { return firstLine(e.Err.Error()) }

// syncErr returns the error of rep (see ops.SyncReport.Err) once its
// warnings are printed: the failed listings, whose warnings said why, are
// only named (see listingError).
func syncErr(rep ops.SyncReport) error {
	rep.Orgs = slices.Clone(rep.Orgs)
	for i, o := range rep.Orgs {
		if o.Err != nil {
			rep.Orgs[i].Err = &listingError{Org: o.Org, Err: o.Err}
		}
	}
	return rep.Err()
}

func runSync(c *cobra.Command, d Deps, fl syncFlags) error {
	if err := fl.validate(); err != nil {
		return err
	}
	w, err := openWorkspace(c, d)
	if err != nil {
		return err
	}
	ctx, interactive := c.Context(), d.interactiveGit(c)
	menu := !(fl.addNew || fl.removeGone || len(fl.exclude) > 0) && !fl.asJSON && d.tuiMode(c)
	orgs := fl.orgs
	if len(orgs) == 0 {
		for _, src := range w.Bundle().Sources {
			orgs = append(orgs, src.Org)
		}
	}
	if menu && len(orgs) > 0 {
		// The listing may take a while before the menu: say so, as add --org.
		fmt.Fprintf(liveOnly(c.ErrOrStderr()), "listing repositories of %s...\n", strings.Join(orgs, ", "))
	}
	label := "listing repositories"
	switch {
	case menu && len(orgs) > 0:
		label = "" // the line above names it
	case len(orgs) > 0:
		label += " of " + strings.Join(orgs, ", ")
	}
	stop := d.spin.start(label)
	rep, err := ops.Sync(ctx, w, d.hosting(), ops.SyncOptions{Orgs: fl.orgs, IncludeArchived: fl.includeArchived})
	stop()
	if err != nil {
		return err
	}
	out, errOut := c.OutOrStdout(), d.leveled(c, c.ErrOrStderr())
	defer flush(errOut)
	for _, o := range rep.Orgs {
		switch {
		case o.Err != nil:
			fmt.Fprintf(errOut, "sheaf: warning: %v\n", o.Err)
		case o.Truncated:
			fmt.Fprintf(errOut, "sheaf: warning: the listing of %s may be truncated (%s listed): no repository is proposed as gone for it\n", o.Org, syPlural(o.Listed, "repository", "repositories"))
		}
		for _, m := range o.Warnings {
			fmt.Fprintf(errOut, "sheaf: warning: %s\n", m)
		}
		for _, m := range o.Notes {
			fmt.Fprintf(errOut, "sheaf: note: %s\n", m)
		}
	}
	if fl.asJSON {
		if err := syWriteJSON(out, w.Root, rep); err != nil {
			return err
		}
		return syncErr(rep)
	}
	if menu {
		return tuiSync(c, d, w, rep, fl.noFork)
	}
	excluded, err := syResolveExcludes(rep, fl.exclude, fl.includeArchived)
	if err != nil {
		return err
	}
	syWriteProposals(c.Context(), out, rep, w.Root, fl.orgs, !(fl.addNew || fl.removeGone || len(fl.exclude) > 0))

	errs := []error{syncErr(rep)}
	if len(excluded) > 0 {
		if err := ops.SyncExclude(ctx, w, excluded); err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, p := range excluded {
			fmt.Fprintf(out, "excluded %s (recorded in sheaf.yaml, no longer proposed)\n", p.Slug)
		}
	}
	r := plainRenderer{errOut}
	if fl.addNew {
		var add []ops.Proposal
		for _, p := range syOfKind(rep.Proposals, ops.ProposalNew) {
			if !syContainsKey(excluded, p.Key) {
				add = append(add, p)
			}
		}
		if len(add) == 0 {
			fmt.Fprintln(out, "no new repository to add")
		} else {
			defer d.emergencyNote(noteClone)()
			errs = append(errs, printSummary(out, ops.SyncAddNewWithOptions(ctx, w, add, ops.CloneOptions{Hosting: d.hosting(), NoFork: fl.noFork, Interactive: interactive}, OptionsFrom(c).Jobs, r)))
		}
	}
	if fl.removeGone {
		gone := syOfKind(rep.Proposals, ops.ProposalGone)
		if len(gone) == 0 {
			fmt.Fprintln(out, "no gone repository to remove")
		} else {
			wd, err := d.getwd()
			if err != nil {
				return err
			}
			// --yes is required (validate), so every removal that passes the
			// checks is confirmed.
			opts := ops.SyncRemoveOptions{
				Remove:           ops.RemoveOptions{Force: fl.force, WorkingDir: wd, Confirm: func(ops.RemovePlan) bool { return true }, Interactive: interactive},
				AllowMassRemoval: fl.allowMass,
			}
			errs = append(errs, printSummary(out, ops.SyncRemoveGone(ctx, w, gone, opts, OptionsFrom(c).Jobs, r)))
		}
	}
	return errors.Join(errs...)
}

func syPlural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func syOfKind(ps []ops.Proposal, k ops.ProposalKind) []ops.Proposal {
	var out []ops.Proposal
	for _, p := range ps {
		if p.Kind == k {
			out = append(out, p)
		}
	}
	return out
}

func syContainsKey(ps []ops.Proposal, key string) bool {
	for _, p := range ps {
		if p.Key == key {
			return true
		}
	}
	return false
}

// syResolveExcludes maps each --exclude argument, "name" or "org/name"
// (case-insensitive), to the new proposal it designates.
func syResolveExcludes(rep ops.SyncReport, args []string, includeArchived bool) ([]ops.Proposal, error) {
	var out []ops.Proposal
	news := syOfKind(rep.Proposals, ops.ProposalNew)
	for _, a := range args {
		org, name, qualified := strings.Cut(a, "/")
		if !qualified {
			org, name = "", a
		}
		var match []ops.Proposal
		for _, p := range news {
			if strings.EqualFold(p.Repo.Name, name) && (!qualified || strings.EqualFold(p.Org, org) || strings.EqualFold(p.Repo.Owner, org)) {
				match = append(match, p)
			}
		}
		switch len(match) {
		case 0:
			why := ""
			for _, o := range rep.Orgs {
				if o.Err != nil && (!qualified || strings.EqualFold(o.Org, org)) {
					why += "; listing repositories of " + o.Org + " failed, so its repositories are unknown"
				}
			}
			if !includeArchived {
				why += "; archived repositories are only proposed with --include-archived"
			}
			return nil, fmt.Errorf("--exclude %s: not a new repository proposed by sync%s (nothing was changed)", a, why)
		case 1:
			if !syContainsKey(out, match[0].Key) {
				out = append(out, match[0])
			}
		default:
			return nil, fmt.Errorf("--exclude %s: several organizations have a new repository with that name; use <org>/%s (nothing was changed)", a, name)
		}
	}
	return out, nil
}

// syWriteProposals prints one line per proposal and, when hints is set,
// how to act on them in the bundle of resolved root root.
func syWriteProposals(ctx context.Context, w io.Writer, rep ops.SyncReport, root string, orgs []string, hints bool) {
	if len(rep.Proposals) == 0 {
		var orgs []string
		for _, o := range rep.Orgs {
			if o.Err == nil {
				orgs = append(orgs, o.Org)
			}
		}
		if len(orgs) > 0 {
			fmt.Fprintf(w, "nothing to sync: the bundle matches %s\n", strings.Join(orgs, ", "))
		}
		return
	}
	for _, p := range rep.Proposals {
		line := fmt.Sprintf("%-8s %s", p.Kind, p.Slug)
		if p.Kind != ops.ProposalNew {
			if _, repoName, _ := strings.Cut(p.Slug, "/"); repoName != p.Name {
				line += " (directory " + p.Name + ")"
			}
		}
		var tags []string
		if p.Repo.Fork {
			tags = append(tags, "fork")
		}
		if p.Kind == ops.ProposalNew && p.Repo.Archived {
			tags = append(tags, "archived")
		}
		if p.MassRemoval {
			tags = append(tags, "mass removal: needs --allow-mass-removal")
		}
		if len(tags) > 0 {
			line += " (" + strings.Join(tags, ", ") + ")"
		}
		if p.Note != "" {
			line += ": " + p.Note
		}
		fmt.Fprintln(w, line)
	}
	if !hints {
		return
	}
	// The commands of the hints keep the organizations of this run.
	syncCmd := func(orgs []string, flags ...string) string {
		args := []string{"sync"}
		for _, o := range orgs {
			args = append(args, "--org", o)
		}
		if slices.Contains(flags, "--yes") {
			return ops.YesCommandLine(ctx, root, append(args, flags...)...)
		}
		return ops.CommandLine(ctx, append(args, flags...)...)
	}
	if len(syOfKind(rep.Proposals, ops.ProposalNew)) > 0 {
		fmt.Fprintln(w, "add the new ones with: "+syncCmd(orgs, "--add-new")+" (decline some for good with --exclude <repo>)")
	}
	if len(syOfKind(rep.Proposals, ops.ProposalGone)) > 0 {
		hint := "remove the gone ones with: " + syncCmd(orgs, "--remove-gone", "--yes") + " (refused when data would be lost)"
		var mass []string
		for _, p := range rep.Proposals {
			if p.MassRemoval && !slices.Contains(mass, p.Org) {
				mass = append(mass, p.Org)
			}
		}
		if len(mass) > 0 {
			hint += "; most repositories of an organization look gone: check its listing (gh authentication scope), then run: " +
				syncCmd(mass, "--remove-gone", "--yes", "--allow-mass-removal")
		}
		fmt.Fprintln(w, hint)
	}
}

type syJSONOrg struct {
	Org         string   `json:"org"`
	Listed      int      `json:"listed"`
	Truncated   bool     `json:"truncated"`
	MassRemoval bool     `json:"mass_removal"`
	Error       string   `json:"error,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	Unconfirmed []string `json:"unconfirmed,omitempty"`
}

type syJSONProposal struct {
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Org           string `json:"org"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	URL           string `json:"url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Fork          *bool  `json:"fork,omitempty"`
	Archived      *bool  `json:"archived,omitempty"`
	Description   string `json:"description,omitempty"`
	CurrentSlug   string `json:"current_slug,omitempty"`
	MassRemoval   bool   `json:"mass_removal,omitempty"`
	PossiblyMoved string `json:"possibly_moved,omitempty"`
	Note          string `json:"note,omitempty"`
}

type syJSONDoc struct {
	Version   int              `json:"version"`
	Bundle    string           `json:"bundle"`
	Orgs      []syJSONOrg      `json:"orgs"`
	Proposals []syJSONProposal `json:"proposals"`
}

func syWriteJSON(w io.Writer, root string, rep ops.SyncReport) error {
	doc := syJSONDoc{Version: 1, Bundle: root, Orgs: []syJSONOrg{}, Proposals: []syJSONProposal{}}
	for _, o := range rep.Orgs {
		jo := syJSONOrg{Org: o.Org, Listed: o.Listed, Truncated: o.Truncated, MassRemoval: o.MassRemoval, Warnings: o.Warnings, Notes: o.Notes, Unconfirmed: o.Unconfirmed}
		if o.Err != nil {
			jo.Error = o.Err.Error()
		}
		doc.Orgs = append(doc.Orgs, jo)
	}
	for _, p := range rep.Proposals {
		jp := syJSONProposal{
			Key: p.Key, Kind: string(p.Kind), Org: p.Org, Name: p.Name, Slug: p.Slug, URL: p.URL,
			DefaultBranch: p.Repo.DefaultBranch, Description: p.Repo.Description, MassRemoval: p.MassRemoval, PossiblyMoved: p.PossiblyMoved, Note: p.Note,
		}
		if p.Kind != ops.ProposalGone { // gone: no hosting data
			fork, archived := p.Repo.Fork, p.Repo.Archived
			jp.Fork, jp.Archived = &fork, &archived
		}
		if p.Kind == ops.ProposalMoved {
			jp.CurrentSlug = p.Repo.Slug()
		}
		doc.Proposals = append(doc.Proposals, jp)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

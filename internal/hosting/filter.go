package hosting

import (
	"slices"
	"strings"
)

// FilterOpts controls Filter.
type FilterOpts struct {
	IncludeArchived bool
}

// Filter drops archived repositories unless opts.IncludeArchived is set (forks
// are always kept) and returns a new slice sorted case-insensitively by name (then exact name, then owner).
func Filter(repos []Repo, opts FilterOpts) []Repo {
	out := make([]Repo, 0, len(repos))
	for _, r := range repos {
		if r.Archived && !opts.IncludeArchived {
			continue
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b Repo) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Owner, b.Owner)
	})
	return out
}

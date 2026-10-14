package hosting

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Fake is an in-memory Hosting for tests of other packages. Set CloneURL on
// the repos you add (for instance to file:// URLs of local bare repositories).
// It is not safe for concurrent mutation; concurrent reads are fine.
type Fake struct {
	// Injected errors, keyed by owner (ListErr) or "owner/name" (RepoErr, PRErr). They take precedence over stored data.
	// Owners, names and error keys are matched case-insensitively, like on GitHub;
	// returned repos carry the canonical (stored) spelling. Inputs are validated
	// like the gh adapter does (ErrInvalidInput).
	ListErr map[string]error
	RepoErr map[string]error
	PRErr   map[string]error

	owners map[string][]Repo
	prs    map[string]PR
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		ListErr: map[string]error{}, RepoErr: map[string]error{}, PRErr: map[string]error{},
		owners: map[string][]Repo{}, prs: map[string]PR{},
	}
}

// AddOwner registers an owner with no repositories (listing succeeds, empty).
func (f *Fake) AddOwner(owner string) {
	key := strings.ToLower(owner)
	if _, ok := f.owners[key]; !ok {
		f.owners[key] = nil
	}
}

// AddRepo registers r under r.Owner, replacing any repo with the same name.
func (f *Fake) AddRepo(r Repo) {
	key := strings.ToLower(r.Owner)
	rs := f.owners[key]
	for i := range rs {
		if strings.EqualFold(rs[i].Name, r.Name) {
			rs[i] = r
			return
		}
	}
	f.owners[key] = append(rs, r)
}

// AddPR registers a pull request of owner/name, keyed by p.Number.
func (f *Fake) AddPR(owner, name string, p PR) {
	f.prs[prKey(owner, name, p.Number)] = p
}

// lookupErr finds an injected error, comparing keys case-insensitively.
func lookupErr(m map[string]error, key string) error {
	for k, err := range m {
		if strings.EqualFold(k, key) {
			return err
		}
	}
	return nil
}

func prKey(owner, name string, n int) string {
	return strings.ToLower(owner+"/"+name) + "#" + strconv.Itoa(n)
}

// ListRepos returns a copy of the owner's repos in insertion order.
func (f *Fake) ListRepos(_ context.Context, owner string) ([]Repo, error) {
	if err := validateOwner(owner); err != nil {
		return nil, err
	}
	if err := lookupErr(f.ListErr, owner); err != nil {
		return nil, err
	}
	rs, ok := f.owners[strings.ToLower(owner)]
	if !ok {
		return nil, fmt.Errorf("owner %q: %w", owner, ErrNotFound)
	}
	return append([]Repo{}, rs...), nil
}

// Repo returns one repository.
func (f *Fake) Repo(_ context.Context, owner, name string) (Repo, error) {
	if err := validateRepo(owner, name); err != nil {
		return Repo{}, err
	}
	if err := lookupErr(f.RepoErr, owner+"/"+name); err != nil {
		return Repo{}, err
	}
	for _, r := range f.owners[strings.ToLower(owner)] {
		if strings.EqualFold(r.Name, name) {
			return r, nil
		}
	}
	return Repo{}, fmt.Errorf("repository %s/%s: %w", owner, name, ErrNotFound)
}

// PR returns one pull request.
func (f *Fake) PR(_ context.Context, owner, name string, number int) (PR, error) {
	if err := validatePR(owner, name, number); err != nil {
		return PR{}, err
	}
	if err := lookupErr(f.PRErr, owner+"/"+name); err != nil {
		return PR{}, err
	}
	if p, ok := f.prs[prKey(owner, name, number)]; ok {
		return p, nil
	}
	return PR{}, fmt.Errorf("pull request %s/%s#%d: %w", owner, name, number, ErrNotFound)
}

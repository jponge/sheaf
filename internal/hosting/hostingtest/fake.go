// Package hostingtest provides an in-memory hosting.Hosting for tests.
package hostingtest

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/jponge/sheaf/internal/hosting"
)

// Fake is an in-memory hosting.Hosting for tests of other packages. Set CloneURL on
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

	// User is the login returned by CurrentUser; UserErr and ForksErr,
	// when set, are returned by CurrentUser and ListForks.
	User     string
	UserErr  error
	ForksErr error
	// DefaultRepoErr, when set, is returned by SetDefaultRepo (the call
	// is recorded anyway).
	DefaultRepoErr error
	// DefaultRepoHook, when set, is called by SetDefaultRepo with its
	// context after recording the call and before returning (outside the
	// Fake's lock), e.g. to hold concurrent calls or cancel the context.
	DefaultRepoHook func(ctx context.Context)
	// BrowseErr, when set, is returned by Browse (the call is recorded
	// anyway); BrowseHook, when set, is called by Browse like
	// DefaultRepoHook by SetDefaultRepo.
	BrowseErr  error
	BrowseHook func(ctx context.Context)

	mu        sync.Mutex
	forks     []hosting.Fork
	forkCalls int

	defaultCalls []DefaultRepoCall
	browseCalls  []BrowseCall

	owners map[string][]hosting.Repo
	prs    map[string]hosting.PR
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		ListErr: map[string]error{}, RepoErr: map[string]error{}, PRErr: map[string]error{},
		owners: map[string][]hosting.Repo{}, prs: map[string]hosting.PR{},
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
func (f *Fake) AddRepo(r hosting.Repo) {
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
func (f *Fake) AddPR(owner, name string, p hosting.PR) {
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
func (f *Fake) ListRepos(_ context.Context, owner string) ([]hosting.Repo, error) {
	if err := hosting.ValidateOwner(owner); err != nil {
		return nil, err
	}
	if err := lookupErr(f.ListErr, owner); err != nil {
		return nil, err
	}
	rs, ok := f.owners[strings.ToLower(owner)]
	if !ok {
		return nil, fmt.Errorf("owner %q: %w", owner, hosting.ErrNotFound)
	}
	return append([]hosting.Repo{}, rs...), nil
}

// Repo returns one repository.
func (f *Fake) Repo(_ context.Context, owner, name string) (hosting.Repo, error) {
	if err := hosting.ValidateRepo(owner, name); err != nil {
		return hosting.Repo{}, err
	}
	if err := lookupErr(f.RepoErr, owner+"/"+name); err != nil {
		return hosting.Repo{}, err
	}
	for _, r := range f.owners[strings.ToLower(owner)] {
		if strings.EqualFold(r.Name, name) {
			return r, nil
		}
	}
	return hosting.Repo{}, fmt.Errorf("repository %s/%s: %w", owner, name, hosting.ErrNotFound)
}

// PR returns one pull request.
func (f *Fake) PR(_ context.Context, owner, name string, number int) (hosting.PR, error) {
	if err := hosting.ValidatePR(owner, name, number); err != nil {
		return hosting.PR{}, err
	}
	if err := lookupErr(f.PRErr, owner+"/"+name); err != nil {
		return hosting.PR{}, err
	}
	if p, ok := f.prs[prKey(owner, name, number)]; ok {
		return p, nil
	}
	return hosting.PR{}, fmt.Errorf("pull request %s/%s#%d: %w", owner, name, number, hosting.ErrNotFound)
}

// FakeUser is the login returned by Fake.CurrentUser when User is empty.
const FakeUser = "sheaf-fake-user"

// AddFork registers a fork, listed by ListForks(f.Owner).
func (f *Fake) AddFork(fk hosting.Fork) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forks = append(f.forks, fk)
}

// ForkCalls returns how many times ListForks was called.
func (f *Fake) ForkCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forkCalls
}

// CurrentUser returns UserErr, or User (FakeUser when empty).
func (f *Fake) CurrentUser(context.Context) (string, error) {
	if f.UserErr != nil {
		return "", f.UserErr
	}
	if f.User == "" {
		return FakeUser, nil
	}
	return f.User, nil
}

// ListForks returns ForksErr, or a copy of the forks owned by owner (an
// owner without forks gets an empty list).
func (f *Fake) ListForks(_ context.Context, owner string) ([]hosting.Fork, error) {
	if err := hosting.ValidateOwner(owner); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forkCalls++
	if f.ForksErr != nil {
		return nil, f.ForksErr
	}
	out := []hosting.Fork{}
	for _, fk := range f.forks {
		if strings.EqualFold(fk.Owner, owner) {
			out = append(out, fk)
		}
	}
	return out, nil
}

// DefaultRepoCall is a SetDefaultRepo call recorded by the Fake: the
// directory and "owner/name" as passed.
type DefaultRepoCall struct{ Dir, Slug string }

// DefaultRepoCalls returns the valid SetDefaultRepo calls so far, in call
// order (failed ones included).
func (f *Fake) DefaultRepoCalls() []DefaultRepoCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DefaultRepoCall(nil), f.defaultCalls...)
}

// SetDefaultRepo validates its input like the gh adapter (an absolute dir,
// a valid owner/name), records the call, runs DefaultRepoHook and returns
// DefaultRepoErr. It is safe for concurrent use.
func (f *Fake) SetDefaultRepo(ctx context.Context, dir, owner, name string) error {
	if err := hosting.ValidateRepo(owner, name); err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: directory %q must be an absolute path", hosting.ErrInvalidInput, dir)
	}
	f.mu.Lock()
	f.defaultCalls = append(f.defaultCalls, DefaultRepoCall{Dir: dir, Slug: owner + "/" + name})
	hook, err := f.DefaultRepoHook, f.DefaultRepoErr
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	return err
}

// BrowseCall is a Browse call recorded by the Fake: "owner/name" as passed
// and the page.
type BrowseCall struct {
	Slug string
	Page hosting.Page
}

// BrowseCalls returns the valid Browse calls so far, in call order (failed
// ones included).
func (f *Fake) BrowseCalls() []BrowseCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BrowseCall(nil), f.browseCalls...)
}

// Browse validates its input like the gh adapter, records the call, runs
// BrowseHook and returns BrowseErr, or ErrNotFound for a Head without a
// pull request (see AddPR); nothing is opened. It is safe for concurrent use.
func (f *Fake) Browse(ctx context.Context, owner, name string, page hosting.Page) error {
	if err := hosting.ValidatePage(owner, name, page); err != nil {
		return err
	}
	f.mu.Lock()
	f.browseCalls = append(f.browseCalls, BrowseCall{Slug: owner + "/" + name, Page: page})
	hook, err := f.BrowseHook, f.BrowseErr
	f.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	if err == nil && page.PR <= 0 && page.Head != "" && !f.hasHead(owner, name, page.Head) {
		err = fmt.Errorf("no pull requests found for branch %q: %w", page.Head, hosting.ErrNotFound)
	}
	return err
}

// hasHead reports whether owner/name has a pull request whose head is head
// ("<branch>" of the repository, "<owner>:<branch>" of a fork).
func (f *Fake) hasHead(owner, name, head string) bool {
	prefix := strings.ToLower(owner + "/" + name + "#")
	for k, p := range f.prs {
		label := p.HeadBranch
		if p.IsCrossRepository {
			label = p.HeadOwner + ":" + p.HeadBranch
		}
		if strings.HasPrefix(k, prefix) && label == head {
			return true
		}
	}
	return false
}

// Package hosting abstracts the code-hosting service (GitHub through the gh CLI)
// used to list an owner's repositories and to resolve repository and pull request metadata.
package hosting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrNotFound is returned (wrapped) when an owner, repository or pull request
// does not exist. The Fake knows this for certain; the gh adapter infers it
// from gh's stderr (messages verified against real gh output),
// and always keeps the stderr text in the error message.
var ErrNotFound = errors.New("not found")

// ErrInvalidInput is returned (wrapped) for a malformed owner, repository name or PR number.
var ErrInvalidInput = errors.New("invalid input")

// Repo describes a hosted repository.
type Repo struct {
	Owner         string
	Name          string
	CloneURL      string // SSH or HTTPS URL, according to the adapter's protocol choice
	HTTPSURL      string
	SSHURL        string
	DefaultBranch string // empty for an empty repository
	Archived      bool
	Fork          bool
	Description   string
}

// Slug returns "owner/name".
func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// PR describes the head of a pull request.
type PR struct {
	Number            int
	HeadBranch        string
	HeadOwner         string // empty if the head repository was deleted
	HeadRepo          string // empty if the head repository was deleted
	IsCrossRepository bool
	URL               string
}

// Fork is a repository forked by the user, with the repository it was
// forked from (its parent). The hosting service gives the parent's owner and
// name only (no URL).
type Fork struct {
	Repo
	ParentOwner string
	ParentName  string
}

// ParentSlug returns "owner/name" of the parent.
func (f Fork) ParentSlug() string { return f.ParentOwner + "/" + f.ParentName }

// badChars reports whether s contains '/', whitespace or control characters.
func badChars(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == '/' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// ValidateOwner returns an error wrapping ErrInvalidInput unless owner is
// non-empty, has no leading '-', and has no '/', whitespace or control
// characters. '_' is allowed (enterprise managed user logins).
func ValidateOwner(owner string) error {
	if owner == "" || owner[0] == '-' || badChars(owner) {
		return fmt.Errorf("%w: owner %q", ErrInvalidInput, owner)
	}
	return nil
}

// ValidateRepo validates owner like ValidateOwner, then requires name to be
// non-empty, free of '/', whitespace and control characters, and not "." or
// "..". A leading '.' or '-' is allowed (".github") because the name is only
// ever passed after "owner/".
func ValidateRepo(owner, name string) error {
	if err := ValidateOwner(owner); err != nil {
		return err
	}
	if name == "" || name == "." || name == ".." || badChars(name) {
		return fmt.Errorf("%w: repository name %q", ErrInvalidInput, name)
	}
	return nil
}

// ValidatePR validates owner and name like ValidateRepo, then requires a
// positive pull request number.
func ValidatePR(owner, name string, number int) error {
	if err := ValidateRepo(owner, name); err != nil {
		return err
	}
	if number <= 0 {
		return fmt.Errorf("%w: pull request number %d must be positive", ErrInvalidInput, number)
	}
	return nil
}

// Hosting is the read-only view of a hosting service needed by sheaf, plus
// the local configuration of its CLI (SetDefaultRepo).
//
// Owner and repository names are matched case-insensitively by the service, and
// repositories can be renamed or transferred (redirects). Callers must use the
// Owner and Name of the returned Repo values (the canonical spelling), not the
// strings they passed in, when deriving slugs, directory names or comparisons.
type Hosting interface {
	ListRepos(ctx context.Context, owner string) ([]Repo, error)
	Repo(ctx context.Context, owner, name string) (Repo, error)
	PR(ctx context.Context, owner, name string, number int) (PR, error)
	// CurrentUser returns the login of the authenticated user.
	CurrentUser(ctx context.Context) (string, error)
	// ListForks lists the forks owned by owner, with their parents.
	// Repositories without a known parent are left out.
	ListForks(ctx context.Context, owner string) ([]Fork, error)
	// SetDefaultRepo makes owner/name the default repository of the
	// service's CLI for the local checkout dir (an absolute path), e.g. the
	// repository `gh pr list` uses there. It changes local configuration
	// only (the checkout's), nothing on the service.
	SetDefaultRepo(ctx context.Context, dir, owner, name string) error
	// Browse opens page of owner/name in the web browser, through the
	// service's CLI.
	Browse(ctx context.Context, owner, name string, page Page) error
}

// Page is a web page of a repository (see Hosting.Browse): pull request PR
// when it is positive; else, when Head is not empty, the pull request whose
// head is Head (a branch of the repository, or "<owner>:<branch>" for a
// branch of a fork; ErrNotFound when there is none); else the tree of
// Branch when it is not empty; else the home page of the repository.
type Page struct {
	PR     int
	Head   string
	Branch string
}

// ValidatePage validates owner and name like ValidateRepo, then page: a
// pull request number that is not negative, and a head and a branch that
// do not begin with '-' (they would be read as options) and has no control
// characters (git refuses ASCII spaces in branch names, not every Unicode
// space).
func ValidatePage(owner, name string, page Page) error {
	if err := ValidateRepo(owner, name); err != nil {
		return err
	}
	if page.PR < 0 {
		return fmt.Errorf("%w: pull request number %d must not be negative", ErrInvalidInput, page.PR)
	}
	for _, ref := range []string{page.Head, page.Branch} {
		if strings.HasPrefix(ref, "-") || strings.ContainsFunc(ref, unicode.IsControl) {
			return fmt.Errorf("%w: branch %q", ErrInvalidInput, ref)
		}
	}
	return nil
}

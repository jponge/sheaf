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

// badChars reports whether s contains '/', whitespace or control characters.
func badChars(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r == '/' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// validateOwner: non-empty, no leading '-', no '/', whitespace or control
// characters. '_' is allowed (enterprise managed user logins).
func validateOwner(owner string) error {
	if owner == "" || owner[0] == '-' || badChars(owner) {
		return fmt.Errorf("%w: owner %q", ErrInvalidInput, owner)
	}
	return nil
}

// validateRepo: the name is non-empty, free of '/', whitespace and control
// characters, and not "." or "..". A leading '.' or '-' is allowed (".github")
// because the name is only ever passed after "owner/".
func validateRepo(owner, name string) error {
	if err := validateOwner(owner); err != nil {
		return err
	}
	if name == "" || name == "." || name == ".." || badChars(name) {
		return fmt.Errorf("%w: repository name %q", ErrInvalidInput, name)
	}
	return nil
}

func validatePR(owner, name string, number int) error {
	if err := validateRepo(owner, name); err != nil {
		return err
	}
	if number <= 0 {
		return fmt.Errorf("%w: pull request number %d must be positive", ErrInvalidInput, number)
	}
	return nil
}

// Hosting is the read-only view of a hosting service needed by sheaf.
//
// Owner and repository names are matched case-insensitively by the service, and
// repositories can be renamed or transferred (redirects). Callers must use the
// Owner and Name of the returned Repo values (the canonical spelling), not the
// strings they passed in, when deriving slugs, directory names or comparisons.
type Hosting interface {
	ListRepos(ctx context.Context, owner string) ([]Repo, error)
	Repo(ctx context.Context, owner, name string) (Repo, error)
	PR(ctx context.Context, owner, name string, number int) (PR, error)
}

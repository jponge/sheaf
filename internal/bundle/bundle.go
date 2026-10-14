// Package bundle models the sheaf.yaml file of a bundle, locates bundles on
// disk and computes the on-disk layout (repository and worktree directories).
package bundle

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// FileName is the name of the bundle metadata file.
const FileName = "sheaf.yaml"

// Version is the only supported sheaf.yaml schema version.
const Version = 1

// Origin values for Repository.Origin.
const (
	OriginExplicit  = "explicit"
	OriginAdopted   = "adopted"
	OriginOrgPrefix = "org:"
)

var (
	// ErrNotFound is returned by Find when no sheaf.yaml exists in any ancestor.
	ErrNotFound = errors.New("no sheaf.yaml found")
	// ErrExists is returned by Create when sheaf.yaml already exists.
	ErrExists = errors.New("sheaf.yaml already exists")
)

// Bundle is the in-memory form of sheaf.yaml.
type Bundle struct {
	Version      int          `yaml:"version"`
	Sources      []Source     `yaml:"sources"`
	Repositories []Repository `yaml:"repositories"`
}

// Source is a GitHub organization tracked by sync.
type Source struct {
	Org     string   `yaml:"org"`
	Exclude []string `yaml:"exclude,omitempty"`
}

// Repository is a repository belonging to the bundle.
type Repository struct {
	Name          string `yaml:"name"`
	Slug          string `yaml:"slug"`
	URL           string `yaml:"url"`
	DefaultBranch string `yaml:"default_branch"`
	Origin        string `yaml:"origin"`
}

// ValidateRepoName checks that name is usable as a directory name directly
// under the bundle root: non-empty, characters in [A-Za-z0-9._-], not
// starting with '.', and not the bundle file name (case-insensitively).
func ValidateRepoName(name string) error {
	switch {
	case name == "":
		return errors.New("repository name must not be empty")
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("invalid repository name %q: must not start with '.'", name)
	case strings.EqualFold(name, FileName):
		return fmt.Errorf("invalid repository name %q: reserved for the bundle file", name)
	case !validSlugPart(name):
		return fmt.Errorf("invalid repository name %q: only letters, digits, '.', '_' and '-' are allowed (use --as <dir> to choose another name)", name)
	}
	return nil
}

func validSlugPart(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isSafe(r) {
			return false
		}
	}
	return true
}

func validateOrg(org string) error {
	if org == "" {
		return errors.New("source org must not be empty")
	}
	if !validSlugPart(org) {
		return fmt.Errorf("invalid source org %q", org)
	}
	return nil
}

func validateSlug(slug string) error {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok || !validSlugPart(owner) || !validSlugPart(name) {
		return fmt.Errorf("invalid slug %q: expected owner/name", slug)
	}
	return nil
}

func validateOrigin(o string) error {
	if o == OriginExplicit || o == OriginAdopted {
		return nil
	}
	if org, ok := strings.CutPrefix(o, OriginOrgPrefix); ok && org != "" {
		return nil
	}
	return fmt.Errorf("invalid origin %q: expected explicit, adopted or org:<name>", o)
}

func validateRepository(r Repository) error {
	if err := ValidateRepoName(r.Name); err != nil {
		return err
	}
	if err := validateSlug(r.Slug); err != nil {
		return fmt.Errorf("repository %q: %w", r.Name, err)
	}
	if r.URL == "" {
		return fmt.Errorf("repository %q: url must not be empty", r.Name)
	}
	if r.DefaultBranch == "" {
		return fmt.Errorf("repository %q: default_branch must not be empty", r.Name)
	}
	if _, err := NormalizeBranch(r.DefaultBranch); err != nil {
		return fmt.Errorf("repository %q: default_branch: %w", r.Name, err)
	}
	if err := validateOrigin(r.Origin); err != nil {
		return fmt.Errorf("repository %q: %w", r.Name, err)
	}
	return nil
}

// Validate checks the structural invariants of the bundle.
func (b *Bundle) Validate() error {
	if b.Version == 0 {
		return errors.New("sheaf.yaml: missing version")
	}
	if b.Version != Version {
		return fmt.Errorf("unsupported sheaf.yaml version %d (supported: %d)", b.Version, Version)
	}
	orgs := map[string]bool{}
	for _, s := range b.Sources {
		if err := validateOrg(s.Org); err != nil {
			return err
		}
		k := strings.ToLower(s.Org)
		if orgs[k] {
			return fmt.Errorf("duplicate source org %q", s.Org)
		}
		orgs[k] = true
		ex := map[string]bool{}
		for _, e := range s.Exclude {
			if e == "" {
				return fmt.Errorf("source %q: exclude entries must not be empty", s.Org)
			}
			if ex[strings.ToLower(e)] {
				return fmt.Errorf("source %q: duplicate exclude %q", s.Org, e)
			}
			ex[strings.ToLower(e)] = true
		}
	}
	names := map[string]bool{}
	slugs := map[string]bool{}
	for _, r := range b.Repositories {
		if err := validateRepository(r); err != nil {
			return err
		}
		nk := strings.ToLower(r.Name)
		if names[nk] {
			return fmt.Errorf("duplicate repository name %q", r.Name)
		}
		names[nk] = true
		k := strings.ToLower(r.Slug)
		if slugs[k] {
			return fmt.Errorf("duplicate repository slug %q", r.Slug)
		}
		slugs[k] = true
	}
	return nil
}

// Load reads and validates dir/sheaf.yaml. Unknown fields are rejected.
func Load(dir string) (*Bundle, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: file is empty", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected additional YAML document")
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &b, nil
}

func (b *Bundle) marshal() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	c := Bundle{
		Version:      b.Version,
		Sources:      slices.Clone(b.Sources),
		Repositories: slices.Clone(b.Repositories),
	}
	slices.SortFunc(c.Sources, func(x, y Source) int { return strings.Compare(x.Org, y.Org) })
	slices.SortFunc(c.Repositories, func(x, y Repository) int { return strings.Compare(x.Name, y.Name) })
	if c.Sources == nil {
		c.Sources = []Source{}
	}
	if c.Repositories == nil {
		c.Repositories = []Repository{}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFile atomically replaces dir/sheaf.yaml via a temp file in dir.
func writeFile(dir string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".sheaf.yaml.tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, FileName))
}

// Save validates b and atomically writes it to dir/sheaf.yaml with sources
// sorted by org and repositories sorted by name. b itself is not modified.
func Save(dir string, b *Bundle) error {
	data, err := b.marshal()
	if err != nil {
		return err
	}
	return writeFile(dir, data)
}

// Create writes an empty bundle to dir/sheaf.yaml, failing with ErrExists if
// the file is already there. dir must exist.
func Create(dir string) error {
	data, err := (&Bundle{Version: Version}).marshal()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Find walks up from start (made absolute, symlinks resolved) to the first
// directory containing a regular file sheaf.yaml and returns it.
func Find(start string) (string, error) {
	return findUntil(start, "")
}

// findUntil is Find with an optional inclusive stop boundary (a resolved
// directory); tests use it to be independent of the real ancestors of
// t.TempDir(). An empty boundary means the filesystem root.
func findUntil(start, boundary string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	for {
		fi, err := os.Stat(filepath.Join(dir, FileName))
		if err == nil && fi.Mode().IsRegular() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if dir == boundary || parent == dir {
			return "", fmt.Errorf("%w in %s or any parent directory", ErrNotFound, start)
		}
		dir = parent
	}
}

func isSafe(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		r == '.' || r == '_' || r == '-'
}

// NormalizeBranch maps a branch name to a directory name: every character
// outside [A-Za-z0-9._-] (including '/') becomes '-', runs of '-' collapse,
// and leading/trailing '-' and '.' are trimmed. Non-ASCII characters are
// mapped to '-'. It fails when nothing is left.
func NormalizeBranch(branch string) (string, error) {
	var sb strings.Builder
	for _, r := range branch {
		if isSafe(r) {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('-')
		}
	}
	s := sb.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-.")
	if s == "" {
		return "", fmt.Errorf("branch %q cannot be mapped to a directory name", branch)
	}
	return s, nil
}

// RepoDir returns the directory holding all checkouts of a repository.
func RepoDir(root, repoName string) string {
	return filepath.Join(root, repoName)
}

// WorktreeDir returns the directory of the checkout of branch (the primary
// checkout when branch is the default branch).
func WorktreeDir(root, repoName, branch string) (string, error) {
	if err := ValidateRepoName(repoName); err != nil {
		return "", err
	}
	n, err := NormalizeBranch(branch)
	if err != nil {
		return "", err
	}
	return filepath.Join(RepoDir(root, repoName), n), nil
}

// CheckWorktreeName reports an error when the normalized directory name of
// branch is already used by a different branch. existing maps directory
// names to the branches that occupy them.
func CheckWorktreeName(existing map[string]string, branch string) error {
	n, err := NormalizeBranch(branch)
	if err != nil {
		return err
	}
	// Compare case-insensitively: the bundle may live on a case-insensitive
	// filesystem. Iterate in sorted order for a deterministic message.
	for _, dirName := range slices.Sorted(maps.Keys(existing)) {
		if other := existing[dirName]; other != branch && strings.EqualFold(dirName, n) {
			return fmt.Errorf("branches %q and %q both map to directory %q", branch, other, n)
		}
	}
	return nil
}

// AddRepository appends r after validating it against the bundle.
func (b *Bundle) AddRepository(r Repository) error {
	if err := validateRepository(r); err != nil {
		return err
	}
	for _, e := range b.Repositories {
		if strings.EqualFold(e.Slug, r.Slug) {
			return fmt.Errorf("repository %q is already in the bundle as %q", r.Slug, e.Name)
		}
	}
	for _, e := range b.Repositories {
		if strings.EqualFold(e.Name, r.Name) {
			return fmt.Errorf("a repository named %q already exists in the bundle; use --as <dir> to choose another directory name", e.Name)
		}
	}
	b.Repositories = append(b.Repositories, r)
	return nil
}

// RemoveRepository deletes the named repository from the bundle metadata.
func (b *Bundle) RemoveRepository(name string) error {
	for i, r := range b.Repositories {
		if r.Name == name {
			b.Repositories = slices.Delete(b.Repositories, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("no repository named %q in the bundle", name)
}

// Repository looks a repository up by directory name. The returned pointer
// aliases the bundle's storage and is invalidated by add/remove.
func (b *Bundle) Repository(name string) (*Repository, bool) {
	for i := range b.Repositories {
		if b.Repositories[i].Name == name {
			return &b.Repositories[i], true
		}
	}
	return nil, false
}

func (b *Bundle) source(org string) *Source {
	for i := range b.Sources {
		if strings.EqualFold(b.Sources[i].Org, org) {
			return &b.Sources[i]
		}
	}
	return nil
}

// AddSource starts tracking an organization.
func (b *Bundle) AddSource(org string) error {
	if err := validateOrg(org); err != nil {
		return err
	}
	if b.source(org) != nil {
		return fmt.Errorf("source %q already exists", org)
	}
	b.Sources = append(b.Sources, Source{Org: org})
	return nil
}

// RemoveSource stops tracking an organization.
func (b *Bundle) RemoveSource(org string) error {
	for i, s := range b.Sources {
		if s.Org == org {
			b.Sources = slices.Delete(b.Sources, i, i+1)
			return nil
		}
	}
	return fmt.Errorf("no source %q in the bundle", org)
}

// AddExclude records that repoName of a tracked org was deliberately declined.
// It is idempotent.
func (b *Bundle) AddExclude(org, repoName string) error {
	if repoName == "" {
		return errors.New("excluded repository name must not be empty")
	}
	s := b.source(org)
	if s == nil {
		return fmt.Errorf("no source %q in the bundle", org)
	}
	if !slices.ContainsFunc(s.Exclude, func(e string) bool { return strings.EqualFold(e, repoName) }) {
		s.Exclude = append(s.Exclude, repoName)
	}
	return nil
}

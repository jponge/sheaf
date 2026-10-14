package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/jponge/sheaf/internal/bundle"
	"github.com/jponge/sheaf/internal/gitx"
)

// FindBundle returns the resolved root of the bundle to operate on: flag
// (the --bundle option, relative to cwd) when not empty, which must contain
// sheaf.yaml itself, otherwise the nearest directory containing sheaf.yaml
// from cwd upwards (bundle.Find).
func FindBundle(flag, cwd string) (string, error) {
	if flag == "" {
		return bundle.Find(cwd)
	}
	if !filepath.IsAbs(flag) {
		flag = filepath.Join(cwd, flag)
	}
	root, err := filepath.EvalSymlinks(flag)
	if err != nil {
		return "", fmt.Errorf("bundle %s: %w", flag, err)
	}
	fi, err := os.Stat(filepath.Join(root, bundle.FileName))
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a bundle: no %s in it", flag, bundle.FileName)
	}
	return root, nil
}

// Workspace is an open bundle: its resolved root and its metadata. Metadata
// changes go through update, which serializes them (within the process and,
// on Unix, with other processes through a lock file) and saves sheaf.yaml,
// so a Workspace can be shared by concurrent tasks.
type Workspace struct {
	Root string

	mu sync.Mutex
	b  *bundle.Bundle
}

// OpenWorkspace loads the bundle whose root is root.
func OpenWorkspace(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return nil, err
	}
	b, err := bundle.Load(abs)
	if err != nil {
		return nil, err
	}
	return &Workspace{Root: abs, b: b}, nil
}

// Bundle returns a deep copy of the current bundle metadata.
func (w *Workspace) Bundle() *bundle.Bundle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return cloneBundle(w.b)
}

// update locks the bundle, reloads sheaf.yaml (another process may have
// changed it), applies fn and saves the result atomically; the in-memory
// state only changes when everything succeeds.
func (w *Workspace) update(fn func(b *bundle.Bundle) error) error {
	return w.updateCtx(context.Background(), nil, fn)
}

// updateCtx is update with cancellation of the lock wait.
func (w *Workspace) updateCtx(ctx context.Context, emit func(Event), fn func(b *bundle.Bundle) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	waiting := func() {
		if emit != nil {
			emit(Event{Message: "waiting for " + lockFileName + " (another sheaf process is updating the bundle)"})
		}
	}
	unlock, err := lockBundle(ctx, w.Root, waiting)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("locking %s: %w", bundle.FileName, err)
	}
	defer unlock()
	c, err := bundle.Load(w.Root)
	if err != nil {
		return err
	}
	if err := fn(c); err != nil {
		return err
	}
	if err := bundle.Save(w.Root, c); err != nil {
		return fmt.Errorf("saving %s: %w", bundle.FileName, err)
	}
	w.b = c
	return nil
}

func cloneBundle(b *bundle.Bundle) *bundle.Bundle {
	c := &bundle.Bundle{Version: b.Version, Repositories: slices.Clone(b.Repositories)}
	for _, s := range b.Sources {
		c.Sources = append(c.Sources, bundle.Source{Org: s.Org, Exclude: slices.Clone(s.Exclude)})
	}
	return c
}

// Create creates dir (and its parents) if needed and an empty sheaf.yaml in
// it. It fails with bundle.ErrExists when dir already is a bundle, and
// refuses a directory inside another bundle (a work tree is fine: the
// bundle is a fresh directory). It returns the resolved bundle root.
func Create(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if outer, err := bundle.Find(resolveExisting(filepath.Dir(abs))); err == nil {
		return "", fmt.Errorf("refusing to create a bundle in %s: it is inside the bundle %s", abs, outer)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return createIn(dir)
}

func createIn(dir string) (string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return "", err
	}
	if err := bundle.Create(root); err != nil {
		return "", err
	}
	return root, nil
}

// CandidateKind tells how an existing checkout is laid out.
type CandidateKind int

const (
	// CandidatePlainClone: <dir>/.git; Adopt moves it to <dir>/<branch>/.
	CandidatePlainClone CandidateKind = iota + 1
	// CandidateLaidOut: <dir>/<branch-dir>/.git; Adopt only registers it.
	CandidateLaidOut
)

func (k CandidateKind) String() string {
	switch k {
	case CandidatePlainClone:
		return "plain clone"
	case CandidateLaidOut:
		return "laid out"
	}
	return fmt.Sprintf("CandidateKind(%d)", int(k))
}

// Candidate is an existing checkout found directly under a bundle root that
// Adopt can register.
type Candidate struct {
	Name    string // directory name under the bundle root
	Path    string // absolute path of that directory
	Kind    CandidateKind
	Primary string // the primary checkout (Path itself for a plain clone)
}

// Init creates sheaf.yaml in the existing directory dir and returns the
// resolved root with the checkouts found in it (see Candidates). It does not
// move or register anything. It fails with bundle.ErrExists when dir already
// is a bundle.
//
// It refuses a directory inside a git work tree or inside another bundle,
// and writes sheaf.yaml only once the detection succeeded.
func Init(ctx context.Context, dir string) (string, []Candidate, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return "", nil, err
	}
	if !fi.IsDir() {
		return "", nil, fmt.Errorf("%s is not a directory", dir)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return "", nil, err
	}
	if gitx.IsRepo(ctx, root) {
		return "", nil, fmt.Errorf("refusing to initialize a bundle in %s: it is inside a git work tree", root)
	}
	if outer, err := bundle.Find(filepath.Dir(root)); err == nil {
		return "", nil, fmt.Errorf("refusing to initialize a bundle in %s: it is inside the bundle %s", root, outer)
	}
	cands, err := Candidates(ctx, root, &bundle.Bundle{Version: bundle.Version})
	if err != nil {
		return "", nil, err
	}
	if _, err := createIn(root); err != nil {
		return "", nil, err
	}
	return root, cands, nil
}

// Candidates lists, sorted by name, the immediate subdirectories of root
// that hold a checkout and are not registered in b:
//
//   - plain clones: <name>/.git is a directory and <name> is the top level
//     of its work tree;
//   - laid-out repositories: exactly one non-hidden subdirectory
//     <name>/<child> holds a primary checkout (a .git directory, not the .git
//     file of a linked worktree), and <child> is the normalized name of its
//     checked-out branch (case-insensitively; not detached).
//
// Symbolic links, hidden entries and names that are not valid repository
// directory names are ignored, as are ambiguous layouts (several primary
// checkouts).
func Candidates(ctx context.Context, root string, b *bundle.Bundle) ([]Candidate, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || bundle.ValidateRepoName(name) != nil || registered(b, name) {
			continue
		}
		dir := filepath.Join(root, name)
		if isPrimaryCheckout(ctx, dir) {
			out = append(out, Candidate{Name: name, Path: dir, Kind: CandidatePlainClone, Primary: dir})
			continue
		}
		if p, err := findPrimary(ctx, dir); err == nil {
			if b, err := gitx.CurrentBranch(ctx, p); err == nil && checkLaidOut(p, b) == nil {
				out = append(out, Candidate{Name: name, Path: dir, Kind: CandidateLaidOut, Primary: p})
			}
		}
	}
	return out, nil
}

func registered(b *bundle.Bundle, name string) bool {
	return slices.ContainsFunc(b.Repositories, func(r bundle.Repository) bool { return strings.EqualFold(r.Name, name) })
}

// isPrimaryCheckout reports whether dir (not a symlink) has a .git directory
// and is the top level of its work tree.
func isPrimaryCheckout(ctx context.Context, dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	gi, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil || !gi.IsDir() {
		return false
	}
	top, err := gitx.TopLevel(ctx, dir)
	if err != nil {
		return false
	}
	rd, err := filepath.EvalSymlinks(dir)
	return err == nil && filepath.Clean(top) == rd
}

var errNoPrimary = errors.New("no primary checkout")

// primaries returns the non-hidden subdirectories of repoDir that are
// primary checkouts (nil when repoDir does not exist).
func primaries(ctx context.Context, repoDir string) ([]string, error) {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if p := filepath.Join(repoDir, e.Name()); isPrimaryCheckout(ctx, p) {
			found = append(found, p)
		}
	}
	return found, nil
}

// findPrimary returns the single non-hidden subdirectory of repoDir that is a
// primary checkout.
func findPrimary(ctx context.Context, repoDir string) (string, error) {
	found, err := primaries(ctx, repoDir)
	if err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%s: %w", repoDir, errNoPrimary)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%s: several primary checkouts: %s", repoDir, strings.Join(found, ", "))
}

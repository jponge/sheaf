package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validBundle() *Bundle {
	return &Bundle{
		Version: 1,
		Sources: []Source{{Org: "smallrye", Exclude: []string{"legacy-thing"}}},
		Repositories: []Repository{
			{Name: "boiler-vroom", Slug: "jponge/boiler-vroom", URL: "git@github.com:jponge/boiler-vroom.git", DefaultBranch: "main", Origin: "explicit"},
		},
	}
}

func TestValidate(t *testing.T) {
	repo := func(name, slug, origin string) Repository {
		return Repository{Name: name, Slug: slug, URL: "u", DefaultBranch: "main", Origin: origin}
	}
	tests := []struct {
		name    string
		mutate  func(b *Bundle)
		wantErr string // "" = valid
	}{
		{"valid", func(b *Bundle) {}, ""},
		{"version 2", func(b *Bundle) { b.Version = 2 }, "version"},
		{"version 0", func(b *Bundle) { b.Version = 0 }, "missing version"},
		{"version 2 is unsupported", func(b *Bundle) { b.Version = 2 }, "unsupported"},
		{"duplicate name case", func(b *Bundle) { b.Repositories = append(b.Repositories, repo("Boiler-Vroom", "x/y", "explicit")) }, "duplicate"},
		{"name space", func(b *Bundle) { b.Repositories[0].Name = "a b" }, "name"},
		{"name unicode", func(b *Bundle) { b.Repositories[0].Name = "café" }, "name"},
		{"name sheaf.yaml", func(b *Bundle) { b.Repositories[0].Name = "Sheaf.YAML" }, "name"},
		{"empty url", func(b *Bundle) { b.Repositories[0].URL = "" }, "url"},
		{"empty default branch", func(b *Bundle) { b.Repositories[0].DefaultBranch = "" }, "default_branch"},
		{"unnormalizable default branch", func(b *Bundle) { b.Repositories[0].DefaultBranch = "///" }, "default_branch"},
		{"duplicate org case", func(b *Bundle) { b.Sources = append(b.Sources, Source{Org: "SmallRye"}) }, "duplicate"},
		{"org with slash", func(b *Bundle) { b.Sources[0].Org = "a/b" }, "org"},
		{"empty exclude", func(b *Bundle) { b.Sources[0].Exclude = []string{""} }, "exclude"},
		{"duplicate exclude", func(b *Bundle) { b.Sources[0].Exclude = []string{"x", "X"} }, "duplicate"},
		{"empty name", func(b *Bundle) { b.Repositories[0].Name = "" }, "name"},
		{"name with slash", func(b *Bundle) { b.Repositories[0].Name = "a/b" }, "name"},
		{"name with backslash", func(b *Bundle) { b.Repositories[0].Name = `a\b` }, "name"},
		{"name dot", func(b *Bundle) { b.Repositories[0].Name = "." }, "name"},
		{"name dotdot", func(b *Bundle) { b.Repositories[0].Name = ".." }, "name"},
		{"name leading dot", func(b *Bundle) { b.Repositories[0].Name = ".hidden" }, "name"},
		{"duplicate name", func(b *Bundle) { b.Repositories = append(b.Repositories, repo("boiler-vroom", "x/y", "explicit")) }, "duplicate"},
		{"duplicate slug", func(b *Bundle) {
			b.Repositories = append(b.Repositories, repo("other", "jponge/boiler-vroom", "explicit"))
		}, "duplicate"},
		{"duplicate slug case", func(b *Bundle) {
			b.Repositories = append(b.Repositories, repo("other", "JPonge/Boiler-Vroom", "explicit"))
		}, "duplicate"},
		{"slug no slash", func(b *Bundle) { b.Repositories[0].Slug = "boiler" }, "slug"},
		{"slug two slashes", func(b *Bundle) { b.Repositories[0].Slug = "a/b/c" }, "slug"},
		{"slug empty owner", func(b *Bundle) { b.Repositories[0].Slug = "/b" }, "slug"},
		{"slug empty name", func(b *Bundle) { b.Repositories[0].Slug = "a/" }, "slug"},
		{"slug space", func(b *Bundle) { b.Repositories[0].Slug = "a/b c" }, "slug"},
		{"origin adopted", func(b *Bundle) { b.Repositories[0].Origin = "adopted" }, ""},
		{"origin org", func(b *Bundle) { b.Repositories[0].Origin = "org:smallrye" }, ""},
		{"origin org empty", func(b *Bundle) { b.Repositories[0].Origin = "org:" }, "origin"},
		{"origin bogus", func(b *Bundle) { b.Repositories[0].Origin = "whatever" }, "origin"},
		{"origin empty", func(b *Bundle) { b.Repositories[0].Origin = "" }, "origin"},
		{"empty org", func(b *Bundle) { b.Sources[0].Org = "" }, "org"},
		{"duplicate org", func(b *Bundle) { b.Sources = append(b.Sources, Source{Org: "smallrye"}) }, "duplicate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := validBundle()
			tc.mutate(b)
			err := b.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestSaveLoadRoundTripAndDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	b := &Bundle{
		Version: 1,
		Sources: []Source{{Org: "zeta"}, {Org: "alpha", Exclude: []string{"x"}}},
		Repositories: []Repository{
			{Name: "zz", Slug: "o/zz", URL: "u1", DefaultBranch: "main", Origin: "adopted"},
			{Name: "aa", Slug: "o/aa", URL: "u2", DefaultBranch: "trunk", Origin: "org:alpha"},
		},
	}
	if err := Save(dir, b); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &Bundle{
		Version: 1,
		Sources: []Source{{Org: "alpha", Exclude: []string{"x"}}, {Org: "zeta"}},
		Repositories: []Repository{
			{Name: "aa", Slug: "o/aa", URL: "u2", DefaultBranch: "trunk", Origin: "org:alpha"},
			{Name: "zz", Slug: "o/zz", URL: "u1", DefaultBranch: "main", Origin: "adopted"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	// Caller's value must not be reordered.
	if b.Repositories[0].Name != "zz" {
		t.Fatalf("Save mutated caller's bundle")
	}
	// Idempotent bytes.
	first, _ := os.ReadFile(filepath.Join(dir, FileName))
	if err := Save(dir, got); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, FileName))
	if string(first) != string(second) {
		t.Fatalf("Save not deterministic:\n%s\n---\n%s", first, second)
	}
}

func TestSaveModeAndNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, validBundle()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected only sheaf.yaml in dir, got %v", entries)
	}
}

func TestSaveInvalidDoesNotTouchExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, validBundle()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, FileName))
	bad := validBundle()
	bad.Version = 7
	if err := Save(dir, bad); err == nil {
		t.Fatal("expected validation error")
	}
	after, _ := os.ReadFile(filepath.Join(dir, FileName))
	if string(before) != string(after) {
		t.Fatal("existing file modified by failed Save")
	}
}

func TestSaveWriteFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	// A directory named sheaf.yaml makes the final rename fail.
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, validBundle()); err == nil {
		t.Fatal("expected Save to fail")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func writeYAML(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"unknown version", "version: 2\nsources: []\nrepositories: []\n", "version"},
		{"unknown field top", "version: 1\nsourcez: []\n", "sourcez"},
		{"unknown field repo", "version: 1\nrepositories:\n  - name: a\n    slug: o/a\n    url: u\n    default_branch: main\n    origin: explicit\n    defualt: x\n", "defualt"},
		{"empty file", "", "empty"},
		{"trailing garbage document", "version: 1\n---\ngarbage: [\n", "document"},
		{"trailing second document", "version: 1\n---\nversion: 99\n", "document"},
		{"missing version", "sources: []\n", "missing version"},
		{"malformed", "version: [1\n", "sheaf.yaml"},
		{"invalid content", "version: 1\nsources:\n  - org: ''\n", "org"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeYAML(t, dir, tc.content)
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want containing %q", err, tc.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		_, err := Load(t.TempDir())
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err = %v, want os.ErrNotExist", err)
		}
	})
}

func TestLoadPlanExample(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, `version: 1
sources:
  - org: smallrye
    exclude: [legacy-thing]
repositories:
  - name: boiler-vroom
    slug: jponge/boiler-vroom
    url: git@github.com:jponge/boiler-vroom.git
    default_branch: main
    origin: explicit
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b, validBundle()) {
		t.Fatalf("got %+v", b)
	}
}

func TestCreate(t *testing.T) {
	dir := t.TempDir()
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.Version != 1 || len(b.Sources) != 0 || len(b.Repositories) != 0 {
		t.Fatalf("unexpected empty bundle: %+v", b)
	}
	err = Create(dir)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Create err = %v, want ErrExists", err)
	}
	// Existing content must be untouched.
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestCreateModeAndNoLeftovers(t *testing.T) {
	dir := t.TempDir()
	if err := Create(dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("unexpected entries: %v", entries)
	}
	fi, _ := os.Stat(filepath.Join(dir, FileName))
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestCreateFailsOnExistingDirectoryOrDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if err := Create(dir); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nowhere")); err == nil {
		t.Fatal("Create wrote through a symlink")
	}
}

func TestCreateDoesNotClobber(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, validBundle()); err != nil {
		t.Fatal(err)
	}
	if err := Create(dir); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	b, _ := Load(dir)
	if len(b.Repositories) != 1 {
		t.Fatal("Create clobbered existing bundle")
	}
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFind(t *testing.T) {
	root := realPath(t, t.TempDir())
	if err := Create(root); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "repo", "main", "sub", "dir")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, start := range []string{root, deep} {
		got, err := Find(start)
		if err != nil || got != root {
			t.Fatalf("Find(%s) = %q, %v; want %q", start, got, err, root)
		}
	}
}

func TestFindRelativeAndSymlink(t *testing.T) {
	root := realPath(t, t.TempDir())
	if err := Create(root); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	got, err := Find(link)
	if err != nil || got != root {
		t.Fatalf("Find(symlink) = %q, %v; want %q", got, err, root)
	}
	t.Chdir(sub)
	got, err = Find(".")
	if err != nil || got != root {
		t.Fatalf("Find(.) = %q, %v; want %q", got, err, root)
	}
}

func TestFindIgnoresDirectoryNamedSheafYaml(t *testing.T) {
	root := realPath(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(root, FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := findUntil(root, root)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFindNotFound(t *testing.T) {
	// findUntil takes a boundary so the test is immune to a sheaf.yaml in a
	// real ancestor of the temp dir.
	root := realPath(t, t.TempDir())
	deep := filepath.Join(root, "x", "y")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := findUntil(deep, root)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// Boundary is inclusive: a bundle at the boundary is found.
	if err := Create(root); err != nil {
		t.Fatal(err)
	}
	got, err := findUntil(deep, root)
	if err != nil || got != root {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindPublicNotFound(t *testing.T) {
	root := realPath(t, t.TempDir())
	for d := root; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, FileName)); err == nil {
			t.Skipf("ancestor %s has a sheaf.yaml", d)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	_, err := Find(root)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestNormalizeBranch(t *testing.T) {
	tests := []struct{ in, want string }{
		{"main", "main"},
		{"feat/cli/rework-ergonomics", "feat-cli-rework-ergonomics"},
		{"release/1.2", "release-1.2"},
		{"feat//double", "feat-double"},
		{"a---b", "a-b"},
		{"/leading", "leading"},
		{"trailing/", "trailing"},
		{".hidden", "hidden"},
		{"foo.", "foo"},
		{"-x-", "x"},
		{"feat/.dot", "feat-.dot"},
		{"a b", "a-b"},
		{"a:b*c?d", "a-b-c-d"},
		{`win\path`, "win-path"},
		{"feat/日本語", "feat"},
		{"café", "caf"},
		{"é/x", "x"},
		{"UPPER_case.1", "UPPER_case.1"},
		{"a..b", "a..b"},
		{"user@host#1", "user-host-1"},
		{"x\x00y", "x-y"},
		{"x\ny", "x-y"},
	}
	for _, tc := range tests {
		got, err := NormalizeBranch(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeBranch(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestNormalizeBranchErrors(t *testing.T) {
	for _, in := range []string{"", ".", "..", "...", "/", "//", "-", "---", "日本語", "/../", " ", "./."} {
		got, err := NormalizeBranch(in)
		if err == nil {
			t.Errorf("NormalizeBranch(%q) = %q, want error", in, got)
		}
	}
}

func TestLayout(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "b")
	if got, want := RepoDir(root, "boiler-vroom"), filepath.Join(root, "boiler-vroom"); got != want {
		t.Errorf("RepoDir = %q, want %q", got, want)
	}
	got, err := WorktreeDir(root, "boiler-vroom", "feat/123")
	if err != nil || got != filepath.Join(root, "boiler-vroom", "feat-123") {
		t.Errorf("WorktreeDir = %q, %v", got, err)
	}
	if _, err := WorktreeDir(root, "boiler-vroom", "///"); err == nil {
		t.Error("expected error for unnormalizable branch")
	}
	for _, bad := range []string{"", "..", "a/b", ".x"} {
		if _, err := WorktreeDir(root, bad, "main"); err == nil {
			t.Errorf("expected error for repo name %q", bad)
		}
	}
}

func TestCheckWorktreeName(t *testing.T) {
	existing := map[string]string{"feat-a-b": "feat/a-b", "main": "main"}
	if err := CheckWorktreeName(existing, "feat/a-b"); err != nil {
		t.Errorf("same branch must not collide: %v", err)
	}
	if err := CheckWorktreeName(existing, "other/x"); err != nil {
		t.Errorf("unused name: %v", err)
	}
	err := CheckWorktreeName(existing, "feat-a/b")
	if err == nil {
		t.Fatal("expected collision")
	}
	for _, s := range []string{"feat/a-b", "feat-a/b"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q must mention %q", err, s)
		}
	}
	if err := CheckWorktreeName(map[string]string{"feat": "feat"}, "Feat"); err == nil {
		t.Error("case-insensitive collision not detected")
	}
	if err := CheckWorktreeName(map[string]string{"Feat": "Feat"}, "feat"); err == nil {
		t.Error("case-insensitive collision not detected (reverse)")
	}
	if err := CheckWorktreeName(nil, "///"); err == nil {
		t.Error("expected normalization error")
	}
	if err := CheckWorktreeName(nil, "x"); err != nil {
		t.Errorf("nil map: %v", err)
	}
}

func TestRepositoryMutations(t *testing.T) {
	b := &Bundle{Version: 1}
	r := Repository{Name: "a", Slug: "o/a", URL: "u", DefaultBranch: "main", Origin: "explicit"}
	if err := b.AddRepository(r); err != nil {
		t.Fatal(err)
	}
	got, ok := b.Repository("a")
	if !ok || got.Slug != "o/a" {
		t.Fatalf("lookup = %+v, %v", got, ok)
	}
	if _, ok := b.Repository("zzz"); ok {
		t.Fatal("unexpected hit")
	}

	dupName := Repository{Name: "a", Slug: "p/a", URL: "u", DefaultBranch: "main", Origin: "explicit"}
	err := b.AddRepository(dupName)
	if err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("dup name err = %v, want hint about --as", err)
	}
	err = b.AddRepository(Repository{Name: "A", Slug: "p/other", URL: "u", DefaultBranch: "main", Origin: "explicit"})
	if err == nil || !strings.Contains(err.Error(), "--as") {
		t.Fatalf("case-variant name err = %v, want --as hint", err)
	}
	// Re-adding the very same repo reports it is already present, no --as hint.
	err = b.AddRepository(r)
	if err == nil || !strings.Contains(err.Error(), "already in the bundle as") || strings.Contains(err.Error(), "--as") {
		t.Fatalf("re-add err = %v", err)
	}
	dupSlug := Repository{Name: "b", Slug: "O/A", URL: "u", DefaultBranch: "main", Origin: "explicit"}
	if err := b.AddRepository(dupSlug); err == nil || !strings.Contains(err.Error(), "already in the bundle") {
		t.Fatalf("dup slug err = %v", err)
	}
	if err := b.AddRepository(Repository{Name: "../x", Slug: "o/x", Origin: "explicit"}); err == nil {
		t.Fatal("invalid repository accepted")
	}
	if len(b.Repositories) != 1 {
		t.Fatalf("failed adds changed the bundle: %+v", b.Repositories)
	}

	if err := b.RemoveRepository("nope"); err == nil {
		t.Fatal("expected error removing unknown repo")
	}
	if err := b.RemoveRepository("a"); err != nil {
		t.Fatal(err)
	}
	if len(b.Repositories) != 0 {
		t.Fatal("not removed")
	}
}

func TestSourceMutations(t *testing.T) {
	b := &Bundle{Version: 1}
	if err := b.AddSource("smallrye"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddSource("smallrye"); err == nil {
		t.Fatal("expected duplicate error")
	}
	if err := b.AddSource("SmallRye"); err == nil {
		t.Fatal("expected case-insensitive duplicate error")
	}
	if err := b.AddSource("a/b"); err == nil {
		t.Fatal("expected invalid-org error")
	}
	if err := b.AddSource(""); err == nil {
		t.Fatal("expected empty-org error")
	}
	if err := b.AddExclude("smallrye", "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddExclude("smallrye", "legacy"); err != nil {
		t.Fatalf("re-adding an exclude must be idempotent: %v", err)
	}
	if got := b.Sources[0].Exclude; !reflect.DeepEqual(got, []string{"legacy"}) {
		t.Fatalf("exclude = %v", got)
	}
	if err := b.AddExclude("smallrye", "Legacy"); err != nil {
		t.Fatal(err)
	}
	if got := b.Sources[0].Exclude; !reflect.DeepEqual(got, []string{"legacy"}) {
		t.Fatalf("case-variant exclude duplicated: %v", got)
	}
	if err := b.AddExclude("unknown", "x"); err == nil {
		t.Fatal("expected error for unknown source")
	}
	if err := b.AddExclude("smallrye", ""); err == nil {
		t.Fatal("expected error for empty repo name")
	}
	if err := b.RemoveSource("unknown"); err == nil {
		t.Fatal("expected error for unknown source")
	}
	if err := b.RemoveSource("smallrye"); err != nil {
		t.Fatal(err)
	}
	if len(b.Sources) != 0 {
		t.Fatal("not removed")
	}
}

func FuzzNormalizeBranch(f *testing.F) {
	for _, s := range []string{"main", "feat/cli/x", "release/1.2", "..", "", "日本", "a..b", ".-.", "sheaf.yaml"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, err := NormalizeBranch(in)
		if err != nil {
			return
		}
		if out == "" || out == "." || out == ".." {
			t.Fatalf("NormalizeBranch(%q) = %q", in, out)
		}
		again, err := NormalizeBranch(out)
		if err != nil || again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q, %v", in, out, again, err)
		}
		// "sheaf.yaml" is reserved only at the bundle root, not under a repo.
		if !strings.EqualFold(out, FileName) {
			if err := ValidateRepoName(out); err != nil {
				t.Fatalf("ValidateRepoName rejects NormalizeBranch(%q) = %q: %v", in, out, err)
			}
		}
	})
}

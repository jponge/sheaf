package bundle

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func forkedRepo() Repository {
	return Repository{Name: "smallrye-mutiny", Slug: "smallrye/smallrye-mutiny", URL: "git@github.com:smallrye/smallrye-mutiny.git",
		DefaultBranch: "main", Origin: "org:smallrye",
		Fork: Fork{Slug: "jponge/smallrye-mutiny", URL: "git@github.com:jponge/smallrye-mutiny.git"}}
}

func TestForkRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	b := &Bundle{Version: 1, Repositories: []Repository{forkedRepo(), validBundle().Repositories[0]}}
	if err := Save(dir, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	want := "    fork:\n      slug: jponge/smallrye-mutiny\n      url: git@github.com:jponge/smallrye-mutiny.git\n"
	if !strings.Contains(string(data), want) {
		t.Fatalf("saved file lacks the fork block %q:\n%s", want, data)
	}
	if n := strings.Count(string(data), "fork:"); n != 1 {
		t.Fatalf("fork must be omitted for the repository without fork, got %d fork keys:\n%s", n, data)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := got.Repository("smallrye-mutiny")
	if !ok || !reflect.DeepEqual(*r, forkedRepo()) {
		t.Fatalf("round trip: got %+v, want %+v", r, forkedRepo())
	}
	if !r.Forked() {
		t.Fatal("Forked() = false for a repository with a fork")
	}
	plain, _ := got.Repository("boiler-vroom")
	if plain.Forked() || plain.Fork != (Fork{}) {
		t.Fatalf("repository without fork: %+v", plain)
	}
}

func TestForkLoadFromYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yml := `version: 1
sources: []
repositories:
  - name: x
    slug: up/x
    url: https://github.com/up/x.git
    default_branch: main
    origin: explicit
    fork:
      slug: me/x-renamed
      url: https://github.com/me/x-renamed.git
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f := b.Repositories[0].Fork; f.Slug != "me/x-renamed" || f.URL != "https://github.com/me/x-renamed.git" {
		t.Fatalf("fork = %+v", f)
	}
}

func TestForkValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(f *Fork, r *Repository)
		wantErr string
	}{
		{"valid", func(*Fork, *Repository) {}, ""},
		{"empty slug", func(f *Fork, _ *Repository) { f.Slug = "" }, "fork"},
		{"invalid slug", func(f *Fork, _ *Repository) { f.Slug = "a b/c" }, "fork"},
		{"empty url", func(f *Fork, _ *Repository) { f.URL = "" }, "fork"},
		{"same url as the reference", func(f *Fork, r *Repository) { f.URL = r.URL }, "fork"},
		{"same slug as the reference", func(f *Fork, r *Repository) { f.Slug = strings.ToUpper(r.Slug) }, "fork"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := forkedRepo()
			tc.mutate(&r.Fork, &r)
			b := &Bundle{Version: 1, Repositories: []Repository{r}}
			err := b.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if err := (&Bundle{Version: 1}).AddRepository(r); err == nil {
				t.Fatal("AddRepository accepted an invalid fork")
			}
		})
	}
}

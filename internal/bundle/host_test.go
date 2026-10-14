package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hostRepo(name, slug, host string) Repository {
	return Repository{Name: name, Slug: slug, URL: "u", DefaultBranch: "main", Origin: OriginAdopted, Host: host}
}

func TestHostRoundTripAndGitHubOmitted(t *testing.T) {
	dir := t.TempDir()
	b := &Bundle{Version: 1, Repositories: []Repository{
		hostRepo("a", "o/a", "gitlab.example.com"),
		hostRepo("b", "o/b", "GitHub.com"),
		hostRepo("c", "o/c", ""),
		hostRepo("d", "o/d", HostLocal),
	}}
	if err := Save(dir, b); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "host:"); n != 2 {
		t.Fatalf("file has %d host fields, want 2 (github.com omitted):\n%s", n, raw)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		host   string
		name   string
		github bool
	}{"a": {"gitlab.example.com", "gitlab.example.com", false}, "b": {"", "github.com", true}, "c": {"", "github.com", true}, "d": {"local", "local", false}}
	for _, r := range got.Repositories {
		w := want[r.Name]
		if r.Host != w.host || r.HostName() != w.name || r.IsGitHub() != w.github {
			t.Errorf("%s: host=%q HostName=%q IsGitHub=%v, want %+v", r.Name, r.Host, r.HostName(), r.IsGitHub(), w)
		}
	}
	if b.Repositories[1].Host != "GitHub.com" {
		t.Fatal("Save modified its argument")
	}
}

func TestHostValidation(t *testing.T) {
	for _, h := range []string{"bad host", "-x.com", "x-.com", "a..b", ".com", "com.", "h/x", "h:22"} {
		b := &Bundle{Version: 1, Repositories: []Repository{hostRepo("a", "o/a", h)}}
		if err := b.Validate(); err == nil {
			t.Errorf("host %q accepted", h)
		}
	}
	for _, h := range []string{"", "local", "github.com", "gitlab.example.com", "git-server", "10.0.0.1", "github_work"} {
		b := &Bundle{Version: 1, Repositories: []Repository{hostRepo("a", "o/a", h)}}
		if err := b.Validate(); err != nil {
			t.Errorf("host %q rejected: %v", h, err)
		}
	}
}

func TestSameSlugOnDifferentHosts(t *testing.T) {
	b := &Bundle{Version: 1}
	if err := b.AddRepository(hostRepo("a", "o/a", "")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRepository(hostRepo("a2", "o/a", "gitlab.example.com")); err != nil {
		t.Fatalf("same slug on another host rejected: %v", err)
	}
	if err := b.AddRepository(hostRepo("a3", "O/A", "github.com")); err == nil {
		t.Fatal("duplicate github.com slug accepted (empty host means github.com)")
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	b.Repositories = append(b.Repositories, hostRepo("a4", "o/a", "GITLAB.example.com"))
	if err := b.Validate(); err == nil {
		t.Fatal("duplicate host+slug accepted by Validate")
	}
}

package hosting

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// Browse runs the gh command opening the page (see Hosting.Browse); gh
// itself starts the browser.
func TestGHBrowseCommands(t *testing.T) {
	for _, c := range []struct {
		page Page
		want string
	}{
		{Page{}, "repo view o/r --web"},
		{Page{PR: 12}, "pr view 12 --repo o/r --web"},
		{Page{Branch: "feat/x"}, "browse --repo o/r --branch feat/x"},
		{Page{PR: 12, Branch: "feat/x"}, "pr view 12 --repo o/r --web"},
	} {
		path, calls := stubGH(t, map[string]resp{"repo view": {}, "pr view": {}, "browse --repo": {}})
		if err := NewGH(GHOptions{Path: path}).Browse(context.Background(), "o", "r", c.page); err != nil {
			t.Fatalf("%+v: %v", c.page, err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{c.want}) {
			t.Errorf("%+v: calls = %q, want %q", c.page, got, c.want)
		}
	}
}

func TestGHBrowseErrors(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo view": {stderr: unauthenticatedStderr, code: 4}})
	if err := NewGH(GHOptions{Path: path}).Browse(context.Background(), "o", "r", Page{}); !errors.Is(err, ErrGHNotAuthenticated) {
		t.Fatalf("unauthenticated: err = %v", err)
	}
	if err := NewGH(GHOptions{Path: filepath.Join(t.TempDir(), "no-gh")}).Browse(context.Background(), "o", "r", Page{}); !errors.Is(err, ErrGHNotFound) {
		t.Fatalf("gh missing: err = %v", err)
	}
	path, calls := stubGH(t, map[string]resp{"repo view": {}, "pr view": {}, "browse --repo": {}})
	gh := NewGH(GHOptions{Path: path})
	for _, c := range []struct {
		owner, name string
		page        Page
	}{
		{"-o", "r", Page{}},
		{"o", "", Page{}},
		{"o", "r", Page{PR: -1}},
		{"o", "r", Page{Branch: "-x"}},
		{"o", "r", Page{Branch: "a b"}},
		{"o", "r", Page{Branch: "a\x1bb"}},
	} {
		if err := gh.Browse(context.Background(), c.owner, c.name, c.page); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("Browse(%q, %q, %+v) = %v, want ErrInvalidInput", c.owner, c.name, c.page, err)
		}
	}
	if got := calls(); got != nil {
		t.Fatalf("gh called: %q", got)
	}
}

package hosting

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
		{Page{Branch: "a b"}, "browse --repo o/r --branch a b"}, // git allows a no-break space
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

// gh's browser launcher may leave the browser running with gh's standard
// output and error (xdg-open starting a browser, $BROWSER): Browse returns
// when gh exits, successfully, without waiting for them.
func TestGHBrowseIgnoresLingeringChildren(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\n(sleep 5; echo late; echo late >&2) &\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := NewGH(GHOptions{Path: path}).Browse(context.Background(), "o", "r", Page{})
	if elapsed := time.Since(start); err != nil || elapsed > time.Second {
		t.Fatalf("Browse = %v after %v, want success well under the 2 s WaitDelay", err, elapsed)
	}
}

// A failure keeps gh's message, without the line announcing the page.
func TestGHBrowseFailureMessage(t *testing.T) {
	path, _ := stubGH(t, map[string]resp{"repo view": {stderr: "Opening github.com/o/r in your browser.\nexec: \"xdg-open\": executable file not found in $PATH", code: 1}})
	err := NewGH(GHOptions{Path: path}).Browse(context.Background(), "o", "r", Page{})
	if err == nil || !strings.Contains(err.Error(), `exec: "xdg-open": executable file not found`) || strings.Contains(err.Error(), "Opening") {
		t.Fatalf("err = %v", err)
	}
}

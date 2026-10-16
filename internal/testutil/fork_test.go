package testutil

import (
	"path/filepath"
	"testing"
)

func TestNewForkedOrigin(t *testing.T) {
	t.Parallel()
	f := NewForkedOrigin(t, OriginOpts{Branches: []string{"feat"}})
	if f.Upstream.Path == f.Fork.Path || f.Fork.URL != "file://"+f.Fork.Path {
		t.Fatalf("fixture = %+v", f)
	}
	if filepath.Base(filepath.Dir(f.Upstream.Path)) != "upstream" || filepath.Base(filepath.Dir(f.Fork.Path)) != "fork" {
		t.Fatalf("paths must end in upstream/<x>.git and fork/<x>.git (distinct slugs): %+v", f)
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/feat"} {
		if Git(t, f.Upstream.Path, "rev-parse", ref) != Git(t, f.Fork.Path, "rev-parse", ref) {
			t.Fatalf("%s differs between upstream and fork", ref)
		}
	}
	// Independent: advancing the upstream does not move the fork.
	AdvanceOrigin(t, f.Upstream, "main", "up.txt", "up\n", "upstream only")
	if Git(t, f.Upstream.Path, "rev-parse", "main") == Git(t, f.Fork.Path, "rev-parse", "main") {
		t.Fatal("fork follows the upstream")
	}
}

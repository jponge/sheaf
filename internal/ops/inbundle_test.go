package ops

import (
	"testing"

	"github.com/jponge/sheaf/internal/bundle"
)

// InBundle tells whether Add would skip a GitHub slug as already present:
// same slug (case-insensitively) on github.com or on an SSH alias of it,
// not on another host.
func TestInBundle(t *testing.T) {
	b := &bundle.Bundle{Repositories: []bundle.Repository{
		{Name: "alpha", Slug: "O/Alpha"},
		{Name: "beta-work", Slug: "o/beta", Host: "github_work"},
		{Name: "gamma", Slug: "o/gamma", Host: "gitlab.com"},
	}}
	for _, tc := range []struct {
		slug, name string
		ok         bool
	}{
		{"o/alpha", "alpha", true},
		{"o/beta", "beta-work", true},
		{"o/gamma", "", false},
		{"o/delta", "", false},
	} {
		name, ok := InBundle(b, tc.slug)
		if name != tc.name || ok != tc.ok {
			t.Errorf("InBundle(%q) = %q, %v; want %q, %v", tc.slug, name, ok, tc.name, tc.ok)
		}
	}
}

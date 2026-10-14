package hosting

import (
	"reflect"
	"testing"
)

func names(rs []Repo) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestFilterDefaultsExcludeArchivedKeepForksSorted(t *testing.T) {
	in := []Repo{
		{Owner: "o", Name: "zeta"},
		{Owner: "o", Name: "alpha", Archived: true},
		{Owner: "o", Name: "forky", Fork: true},
		{Owner: "o", Name: "beta"},
	}
	got := names(Filter(in, FilterOpts{}))
	want := []string{"beta", "forky", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFilterIncludeArchived(t *testing.T) {
	in := []Repo{{Name: "b", Archived: true}, {Name: "a"}}
	got := names(Filter(in, FilterOpts{IncludeArchived: true}))
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFilterDoesNotMutateInputAndHandlesNil(t *testing.T) {
	in := []Repo{{Name: "b"}, {Name: "a"}}
	Filter(in, FilterOpts{})
	if in[0].Name != "b" || in[1].Name != "a" {
		t.Fatalf("input was mutated: %v", names(in))
	}
	if got := Filter(nil, FilterOpts{}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestFilterSortIsCaseInsensitive(t *testing.T) {
	in := []Repo{{Owner: "o", Name: "beta"}, {Owner: "o", Name: "Alpha"}, {Owner: "o", Name: "alpha"}, {Owner: "o", Name: "Zed"}}
	got := names(Filter(in, FilterOpts{}))
	if want := []string{"Alpha", "alpha", "beta", "Zed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

package ops

import "testing"

func TestParseRemoteURLSlug(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"git@github.com:jponge/boiler-vroom.git":          "jponge/boiler-vroom",
		"git@github.com:jponge/boiler-vroom":              "jponge/boiler-vroom",
		"git@github.com:jponge/boiler-vroom.git/":         "jponge/boiler-vroom",
		"https://github.com/smallrye/smallrye-config":     "smallrye/smallrye-config",
		"https://github.com/smallrye/smallrye-config.git": "smallrye/smallrye-config",
		"https://user@github.com/o/n.git":                 "o/n",
		"ssh://git@github.com/o/n.git":                    "o/n",
		"ssh://git@github.com:22/o/n.git":                 "o/n",
		"https://github.com/smallrye/.github.git":         "smallrye/.github",
		"file:///tmp/x/001/origin.git":                    "001/origin",
		"/tmp/x/team/repo.git":                            "team/repo",
		"https://gitlab.example.com/group/sub/proj.git":   "sub/proj",
	}
	for in, want := range ok {
		_, got, err := ParseRemoteURL(in)
		if err != nil || got != want {
			t.Errorf("ParseRemoteURL(%q) slug = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"",
		"git@github.com:onlyname.git",
		"https://github.com/onlyowner",
		"https://github.com/a/b/c", // GitHub paths must be exactly owner/name
		"git@github.com:a/b/c.git",
		"https://github.com/o/we ird",
		"file:///repo.git",
		"https://github.com/o/.git",
	}
	for _, in := range bad {
		if _, got, err := ParseRemoteURL(in); err == nil {
			t.Errorf("ParseRemoteURL(%q) slug = %q, want error", in, got)
		}
	}
}

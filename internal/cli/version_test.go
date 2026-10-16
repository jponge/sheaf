package cli

import (
	"runtime/debug"
	"testing"
)

func TestVersionString(t *testing.T) {
	t.Parallel()
	vcs := func(rev, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified}}
	}
	const rev = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name    string
		stamped string
		bi      *debug.BuildInfo
		want    string
	}{
		{"stamped wins", "1.2.3", &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, "1.2.3"},
		{"no build info", "dev", nil, "dev"},
		{"go install @tag", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v1.4.0"}}, "v1.4.0"},
		{"local build with VCS pseudo-version", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261016120000-0123456789ab+dirty"}, Settings: vcs(rev, "true")}, "v0.0.0-20261016120000-0123456789ab+dirty"},
		{"devel with clean revision", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs(rev, "false")}, "dev (0123456789ab)"},
		{"devel with modified revision", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs(rev, "true")}, "dev (0123456789ab-dirty)"},
		{"devel without VCS", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
		{"empty main version", "dev", &debug.BuildInfo{}, "dev"},
	} {
		if got := versionString(tc.stamped, tc.bi); got != tc.want {
			t.Errorf("%s: versionString = %q, want %q", tc.name, got, tc.want)
		}
	}
}

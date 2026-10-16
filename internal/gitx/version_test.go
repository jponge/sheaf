package gitx

import (
	"context"
	"errors"
	"testing"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want GitVersion
	}{
		{"git version 2.47.3\n", GitVersion{2, 47, 3}},
		{"git version 2.39.3 (Apple Git-146)\n", GitVersion{2, 39, 3}},
		{"git version 2.45.1.windows.1", GitVersion{2, 45, 1}},
		{"git version 2.48.0.rc1", GitVersion{2, 48, 0}},
		{"git version 2.36.0-rc2", GitVersion{2, 36, 0}},
		{"git version 2.36", GitVersion{2, 36, 0}},
		{"git version 3.0.0", GitVersion{3, 0, 0}},
	} {
		got, err := ParseVersion(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "git version", "git version x.y.z", "hub version 2.47.0", "git version 2", "git version .1.2"} {
		if v, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", in, v)
		}
	}
}

func TestGitVersionCompareAndString(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v    GitVersion
		want bool
	}{
		{GitVersion{2, 36, 0}, true},
		{GitVersion{2, 36, 1}, true},
		{GitVersion{2, 47, 0}, true},
		{GitVersion{3, 0, 0}, true},
		{GitVersion{2, 35, 9}, false},
		{GitVersion{1, 99, 99}, false},
	} {
		if got := tc.v.AtLeast(MinGitVersion); got != tc.want {
			t.Errorf("%v.AtLeast(%v) = %v, want %v", tc.v, MinGitVersion, got, tc.want)
		}
		err := checkVersion(tc.v)
		if (err == nil) != tc.want || (err != nil && !errors.Is(err, ErrGitTooOld)) {
			t.Errorf("checkVersion(%v) = %v", tc.v, err)
		}
	}
	if s := (GitVersion{2, 39, 3}).String(); s != "2.39.3" {
		t.Errorf("String() = %q", s)
	}
}

func TestVersionAndCheckVersionWithInstalledGit(t *testing.T) {
	t.Parallel()
	v, err := Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Major < 2 {
		t.Fatalf("Version = %v", v)
	}
	if got, err := CheckVersion(context.Background()); err != nil || got != v {
		t.Fatalf("CheckVersion = %v, %v (installed git %v must satisfy the minimum for this test suite)", got, err, v)
	}
}

// CheckVersion must not depend on TMPDIR pointing to an
// existing directory.
func TestCheckVersionWithMissingTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", "/nonexistent-sheaf-test-dir")
	if _, err := CheckVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
}

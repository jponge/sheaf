package cli

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewRootCmd(Streams{In: strings.NewReader(""), Out: &out, Err: &errb})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func TestVersionPrintsVersionVariable(t *testing.T) {
	out, _, err := run(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != Version {
		t.Fatalf("version output = %q, want %q", got, Version)
	}
}

func TestVersionHonoursOverride(t *testing.T) {
	old := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = old })
	out, _, err := run(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != "1.2.3" {
		t.Fatalf("version output = %q, want 1.2.3", got)
	}
}

func TestPersistentFlagsDefaults(t *testing.T) {
	t.Parallel()
	cmd := NewRootCmd(Streams{})
	f := cmd.PersistentFlags()
	if fl := f.Lookup("bundle"); fl == nil || fl.DefValue != "" {
		t.Fatalf("bundle flag missing or bad default: %+v", fl)
	}
	if fl := f.Lookup("no-tui"); fl == nil || fl.DefValue != "false" {
		t.Fatalf("no-tui flag missing or bad default: %+v", fl)
	}
	fl := f.Lookup("jobs")
	if fl == nil {
		t.Fatal("jobs flag missing")
	}
	if fl.Shorthand != "j" {
		t.Fatalf("jobs shorthand = %q, want j", fl.Shorthand)
	}
	if fl.DefValue != strconv.Itoa(runtime.NumCPU()) {
		t.Fatalf("jobs default = %q, want %d", fl.DefValue, runtime.NumCPU())
	}
}

func TestFlagsParsedIntoOptions(t *testing.T) {
	t.Parallel()
	var got Options
	cmd := NewRootCmd(Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	probe := &cobra.Command{Use: "probe", RunE: func(c *cobra.Command, _ []string) error {
		got = OptionsFrom(c)
		return nil
	}}
	cmd.AddCommand(probe)
	cmd.SetArgs([]string{"--bundle", "/x/y", "--no-tui", "-j", "3", "probe"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := Options{Bundle: "/x/y", NoTUI: true, Jobs: 3}
	if got != want {
		t.Fatalf("options = %+v, want %+v", got, want)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	t.Parallel()
	if _, _, err := run(t, "nope"); err == nil {
		t.Fatal("expected error for unknown command")
	}
}

func TestExecuteExitCodes(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	s := Streams{In: strings.NewReader(""), Out: &out, Err: &errb}
	if code := Execute([]string{"version"}, s); code != 0 {
		t.Fatalf("version exit = %d", code)
	}
	if code := Execute([]string{"nope"}, s); code != 1 {
		t.Fatalf("unknown exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "nope") {
		t.Fatalf("stderr = %q, want mention of command", errb.String())
	}
}

func TestJobsMustBePositive(t *testing.T) {
	t.Parallel()
	for _, j := range []string{"0", "-5"} {
		_, _, err := run(t, "-j", j, "version")
		if err == nil || !strings.Contains(err.Error(), "--jobs") {
			t.Errorf("-j %s: err = %v, want error mentioning --jobs", j, err)
		}
	}
	if _, _, err := run(t, "-j", "1", "version"); err != nil {
		t.Errorf("-j 1: %v", err)
	}
}

func TestRootHelpDescribesTheHomeScreen(t *testing.T) {
	t.Parallel()
	out, _, err := run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Manage git repository checkouts and worktrees in a bundle", "home screen", "--no-tui"} {
		if !strings.Contains(out, want) {
			t.Errorf("sheaf --help lacks %q:\n%s", want, out)
		}
	}
}

// The generated parts of every command's help fit 80 columns (B62 M8:
// cobra does not wrap them, the terminal did, inside words): the one-liners
// of the commands list (each at most 60 columns) and the global flags. The
// -j usage is short: what -j 1 does to git's prompts is said once, in the
// root help.
func TestHelpListsAndGlobalFlagsFit80Columns(t *testing.T) {
	t.Parallel()
	var walk func(c *cobra.Command) []*cobra.Command
	walk = func(c *cobra.Command) []*cobra.Command {
		all := []*cobra.Command{c}
		for _, s := range c.Commands() {
			if s.IsAvailableCommand() || s.Name() == "help" {
				all = append(all, walk(s)...)
			}
		}
		return all
	}
	root := NewRootCmd(Streams{In: strings.NewReader("")})
	for _, c := range walk(root) {
		if n := len(c.Short); n > 60 {
			t.Errorf("%q: one-liner of %d columns: %q", c.CommandPath(), n, c.Short)
		}
		args := append(strings.Fields(c.CommandPath())[1:], "--help")
		out, _, err := run(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		section := ""
		for _, l := range strings.Split(out, "\n") {
			switch {
			case l == "":
				section = ""
				continue
			case strings.HasSuffix(l, ":") && !strings.HasPrefix(l, " "):
				section = l
				continue
			}
			global := section == "Global Flags:" || (c == root && section == "Flags:")
			if (section == "Available Commands:" || global) && len([]rune(l)) > 80 {
				t.Errorf("%q --help: %s line of %d columns: %q", c.CommandPath(), section, len([]rune(l)), l)
			}
		}
	}
	out, _, err := run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "With -j 1") || strings.Count(out, "passphrase") != 1 {
		t.Errorf("the root help does not explain -j 1 once:\n%s", out)
	}
}

func TestWindowsIsRefusedAtStartup(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"windows", "linux", "darwin"} {
		var out, errb bytes.Buffer
		code := executeOn(goos, []string{"version"}, Streams{In: strings.NewReader(""), Out: &out, Err: &errb})
		if goos == "windows" {
			if code != 1 || out.Len() != 0 || !strings.Contains(errb.String(), "Windows is not supported") {
				t.Errorf("%s: exit %d, stdout %q, stderr %q; want exit 1, no output, a refusal", goos, code, out.String(), errb.String())
			}
			continue
		}
		if code != 0 || strings.TrimSpace(out.String()) != Version {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", goos, code, out.String(), errb.String())
		}
	}
}

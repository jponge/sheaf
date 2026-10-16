package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var shells = []string{"bash", "zsh", "fish"}

// addShellCommands registers completion and shell-init commands.
func addShellCommands(root *cobra.Command) {
	root.AddCommand(newCompletionCmd(), newShellInitCmd())
}

// Registering our own "completion" command makes cobra skip its default one.
func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion bash|zsh|fish",
		Short: "Generate the shell completion script for bash, zsh or fish",
		Long: "Generate the completion script of sheaf for a shell. Besides commands and flags it\n" +
			"completes repository names, worktree branches and directories, branches for\n" +
			"`worktree add` and organizations, from the bundle (--bundle on the command line is\n" +
			"honoured); it never uses the network. See `sheaf completion <shell> --help` to\n" +
			"install it, and `sheaf shell-init --help` for the scd function.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("missing shell: expected bash, zsh or fish")
		},
	}
	gen := func(shell, short, long string, write func(*cobra.Command, io.Writer) error) *cobra.Command {
		return &cobra.Command{
			Use:   shell,
			Short: short,
			Long:  long,
			Args:  cobra.NoArgs,
			RunE:  func(c *cobra.Command, _ []string) error { return write(c.Root(), c.OutOrStdout()) },
		}
	}
	cmd.AddCommand(
		gen("bash", "Generate the bash completion script",
			"Generate the bash completion script (needs the bash-completion package).\n\n"+
				"Load it in the current session:\n\n"+
				"  source <(sheaf completion bash)\n\n"+
				"or install it for every session, for instance:\n\n"+
				"  sheaf completion bash > ~/.local/share/bash-completion/completions/sheaf\n"+
				"  sheaf completion bash | sudo tee /etc/bash_completion.d/sheaf >/dev/null\n\n"+
				"or add `source <(sheaf completion bash)` to ~/.bashrc.",
			func(r *cobra.Command, w io.Writer) error { return r.GenBashCompletionV2(w, true) }),
		gen("zsh", "Generate the zsh completion script",
			"Generate the zsh completion script. Completion must be enabled in your shell; if it\n"+
				"is not, run once:\n\n"+
				"  echo \"autoload -U compinit; compinit\" >> ~/.zshrc\n\n"+
				"Load it in the current session (after compinit):\n\n"+
				"  source <(sheaf completion zsh)\n\n"+
				"or install it for every session, in a directory of $fpath:\n\n"+
				"  sheaf completion zsh > \"${fpath[1]}/_sheaf\"\n\n"+
				"and restart the shell.",
			func(r *cobra.Command, w io.Writer) error { return r.GenZshCompletion(w) }),
		gen("fish", "Generate the fish completion script",
			"Generate the fish completion script.\n\n"+
				"Load it in the current session:\n\n"+
				"  sheaf completion fish | source\n\n"+
				"or install it for every session:\n\n"+
				"  sheaf completion fish > ~/.config/fish/completions/sheaf.fish",
			genFishCompletion),
	)
	return cmd
}

// fishCompletionFixes patch the script of cobra's GenFishCompletion (v1.10.2),
// which evals "$args[1] __complete $args[2..-1] $lastArg": the previous words
// come unescaped from `commandline -opc`, so eval splits words with spaces and
// runs a quoted "(cmd)" on TAB, and the current token is escaped as typed, so
// q\& reaches sheaf as q\&. The fix escapes the expanded words (`commandline
// -x` expands ~ and variables but not command substitutions, fish >= 3.7; older
// fish falls back to -o, without expansion) and unquotes the current token.
var fishCompletionFixes = []struct{ old, new string }{
	{"    set -l args (commandline -opc)\n",
		"    set -l args (string escape -- (commandline -xpc 2>/dev/null; or commandline -opc))\n"},
	{"    set -l lastArg (string escape -- (commandline -ct))\n",
		"    set -l lastArg (string escape -- (commandline -ct | string unescape --style=script))\n"},
}

func genFishCompletion(r *cobra.Command, w io.Writer) error {
	var b strings.Builder
	if err := r.GenFishCompletion(&b, true); err != nil {
		return err
	}
	script := b.String()
	for _, f := range fishCompletionFixes {
		if n := strings.Count(script, f.old); n != 1 {
			return fmt.Errorf("internal error: cobra's fish completion script changed (%d occurrences of %q)", n, strings.TrimSpace(f.old))
		}
		script = strings.Replace(script, f.old, f.new, 1)
	}
	_, err := io.WriteString(w, script)
	return err
}

func newShellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell-init bash|zsh|fish",
		Short: "Print the shell code defining scd, a cd that understands sheaf paths",
		Long: "Print shell code defining the function scd: `scd <args>` changes the directory to\n" +
			"the output of `sheaf path <args>` (scd alone: the bundle root; scd <repo>; scd <repo>\n" +
			"<branch-or-dir>). When sheaf path fails its status is returned and the directory is\n" +
			"not changed. Completion of scd is the one of `sheaf path`, and the code does not\n" +
			"need the output of `sheaf completion`.\n\n" +
			"It also defines a function named sheaf, which runs sheaf and, when enter was pressed\n" +
			"on a checkout in the home screen (sheaf without a command, in a bundle, on a\n" +
			"terminal), changes the directory to that checkout; sheaf writes it to the file named\n" +
			"by SHEAF_CD_FILE, a temporary file created by the function (sheaf removes the\n" +
			"variable from its environment at startup: the processes it starts never see it).\n" +
			"Every other use of sheaf behaves as without the function (same arguments, streams\n" +
			"and exit status). In the home screen, q or esc quits with status 0, even after an\n" +
			"action failed (its error is printed and shown in the screen); ctrl+c exits with 130.\n" +
			"The fish function needs fish 3.1 or later.\n\n" +
			"Add to ~/.bashrc or ~/.zshrc:\n\n" +
			"  eval \"$(sheaf shell-init bash)\"    # or zsh\n\n" +
			"Add to ~/.config/fish/config.fish:\n\n" +
			"  sheaf shell-init fish | source\n\n" +
			"The zsh completion needs compinit to have run before (see `sheaf completion zsh --help`).",
		// no ValidArgs: cobra would then complete nothing after the shell
		Args: func(c *cobra.Command, args []string) error {
			if err := cobra.ExactArgs(1)(c, args); err != nil {
				return err
			}
			if !slices.Contains(shells, args[0]) {
				return fmt.Errorf("unsupported shell %q: expected bash, zsh or fish", args[0])
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			script, ok := shellInit[args[0]]
			if !ok {
				return fmt.Errorf("unsupported shell %q: expected bash, zsh or fish", args[0])
			}
			_, err := io.WriteString(c.OutOrStdout(), script)
			return err
		},
	}
}

// The completion functions below call `sheaf __complete path <words>`, which
// prints one candidate per line (optionally followed by a TAB and a
// description) and a final line ":<directive>" (a bit set: 4 means no file
// completion, otherwise directories are offered when there is no candidate).
// They rejoin "--bundle=X" and expand a leading ~ of the --bundle value, which
// the shells hand over unexpanded, and hide the global flags that mean nothing
// for scd (--jobs, -j, --no-tui).
var shellInit = map[string]string{
	"bash": `# sheaf runs sheaf; after the home screen (bare sheaf) it changes the
# directory to the checkout chosen there, which sheaf writes to the file
# named by SHEAF_CD_FILE. Other commands run as without this function.
sheaf() {
  local tmp dir rc
  tmp="$(mktemp "${TMPDIR:-/tmp}/sheaf-cd.XXXXXX" 2>/dev/null)" || { command sheaf "$@"; return; }
  rc=0
  SHEAF_CD_FILE="$tmp" command sheaf "$@" || rc=$?
  dir=
  if [ -s "$tmp" ]; then dir="$(cat -- "$tmp")"; fi
  rm -f -- "$tmp"
  if [ "$rc" -eq 0 ] && [ -n "$dir" ]; then
    cd -- "$dir" || return
  fi
  return "$rc"
}

scd() {
  local dir
  dir="$(command sheaf path "$@")" || return $?
  [ -n "$dir" ] || return 1
  cd -- "$dir"
}

# _sheaf_scd_dequote word: sets dq to the word as the shell would pass it
# (quote removal, no expansion), dq_quote to the quote left open at its end
# (' or ", or empty) and dq_head to the value of the part before that quote.
_sheaf_scd_dequote() {
  local s=$1 c i bq=$'\x60'
  dq= dq_quote= dq_head=
  for ((i = 0; i < ${#s}; i++)); do
    c=${s:i:1}
    if [[ $dq_quote == "'" ]]; then
      if [[ $c == "'" ]]; then dq_quote=; else dq+=$c; fi
    elif [[ $dq_quote == '"' ]]; then
      if [[ $c == '"' ]]; then
        dq_quote=
      elif [[ $c == '\' && ( ${s:i+1:1} == [\"\$\\] || ${s:i+1:1} == "$bq" ) ]]; then
        dq+=${s:i+1:1}
        ((i++))
      else
        dq+=$c
      fi
    elif [[ $c == "'" || $c == '"' ]]; then
      dq_quote=$c
      dq_head=$dq
    elif [[ $c == '\' ]]; then
      dq+=${s:i+1:1}
      ((i++))
    else
      dq+=$c
    fi
  done
}

_sheaf_scd() {
  local i n w v pre cur line dq dq_quote dq_head bq=$'\x60' d=4
  local -a args=() out=()
  COMPREPLY=()
  # COMP_WORDS are as typed: unquote them. COMP_WORDBREAKS splits --bundle=X
  # into three words: join them again.
  for ((i = 1; i <= COMP_CWORD; i++)); do
    _sheaf_scd_dequote "${COMP_WORDS[i]}"
    w=$dq
    n=${#args[@]}
    if ((n)) && { [[ $w == = ]] || [[ ${args[n-1]} == -*= ]]; }; then
      args[n-1]+=$w
    else
      args+=("$w")
    fi
  done
  for ((i = 0; i < ${#args[@]}; i++)); do
    v=${args[i]}
    pre=
    if [[ $v == --bundle=* ]]; then
      pre=--bundle=
      v=${v#--bundle=}
    elif ((i > 0)) && [[ ${args[i-1]} == --bundle ]]; then
      :
    else
      continue
    fi
    if [[ $v == '~' || $v == '~/'* ]]; then
      args[i]=$pre$HOME${v#\~}
    fi
  done
  # dq* now describe the current word. Inside a quote left open, readline
  # replaces the text after the quote and closes it: insert the rest of the
  # candidate, quoted for that quote, instead of escaping it.
  cur=$dq
  [[ $cur == = ]] && cur=
  while IFS= read -r line; do
    out+=("$line")
  done < <(command sheaf __complete path "${args[@]}" 2>/dev/null)
  if ((${#out[@]})); then
    line=${out[${#out[@]}-1]}
    d=${line#:}
    [[ $d =~ ^[0-9]+$ ]] || d=4
    unset 'out[${#out[@]}-1]'
  fi
  for line in "${out[@]}"; do
    line="${line%%$'\t'*}"
    case "$line" in
      '' | --jobs | -j | --no-tui) continue ;;
      "$cur"*)
        if [[ $dq_quote == "'" ]]; then
          COMPREPLY+=("${line#"$dq_head"}")
        elif [[ $dq_quote == '"' ]]; then
          line=${line#"$dq_head"}
          line=${line//\\/\\\\}
          line=${line//\$/\\\$}
          line=${line//"$bq"/\\$bq}
          COMPREPLY+=("${line//\"/\\\"}")
        else
          COMPREPLY+=("$(printf '%q' "$line")")
        fi
        ;;
    esac
  done
  if ((${#COMPREPLY[@]} == 0 && (d & 4) == 0)); then
    compopt -o filenames 2>/dev/null
    while IFS= read -r line; do
      COMPREPLY+=("$line")
    done < <(compgen -d -- "$cur")
  fi
}
complete -F _sheaf_scd scd
`,
	"zsh": `# sheaf runs sheaf; after the home screen (bare sheaf) it changes the
# directory to the checkout chosen there, which sheaf writes to the file
# named by SHEAF_CD_FILE. Other commands run as without this function.
sheaf() {
  local tmp dir rc
  tmp="$(mktemp "${TMPDIR:-/tmp}/sheaf-cd.XXXXXX" 2>/dev/null)" || { command sheaf "$@"; return; }
  rc=0
  SHEAF_CD_FILE="$tmp" command sheaf "$@" || rc=$?
  dir=
  if [ -s "$tmp" ]; then dir="$(cat -- "$tmp")"; fi
  rm -f -- "$tmp"
  if [ "$rc" -eq 0 ] && [ -n "$dir" ]; then
    cd -- "$dir" || return
  fi
  return "$rc"
}

scd() {
  local dir
  dir="$(command sheaf path "$@")" || return $?
  [ -n "$dir" ] || return 1
  cd -- "$dir"
}

_sheaf_scd() {
  local tab=$'\t' v pre last i d=4
  local -a args lines
  # words are as typed: unquote them; an opening quote of the current word is
  # in compstate[quote] (', " or $'), not in PREFIX: close it to unquote
  args=("${(@Q)words[2,CURRENT-1]}")
  v=$compstate[quote]$PREFIX$SUFFIX${compstate[quote]#\$}
  args+=("${(Q)v}")
  for ((i = 1; i <= $#args; i++)); do
    v=$args[i]
    pre=
    if [[ $v == --bundle=* ]]; then
      pre=--bundle=
      v=${v#--bundle=}
    elif ((i > 1)) && [[ $args[i-1] == --bundle ]]; then
      :
    else
      continue
    fi
    if [[ $v == '~' || $v == '~/'* ]]; then
      args[i]=$pre$HOME${v#\~}
    fi
  done
  lines=("${(@f)$(command sheaf __complete path "${(@)args}" 2>/dev/null)}")
  last=${lines[-1]}
  d=${last#:}
  [[ $d == <-> ]] || d=4
  lines=("${(@)lines[1,-2]}")
  lines=("${(@)lines:#(|--jobs|-j|--no-tui)(|${tab}*)}")
  if (( ${#lines} )); then
    compadd -- "${(@)lines%%${tab}*}"
  elif (( ! (d & 4) )); then
    _path_files -/
  fi
}
if (( $+functions[compdef] )); then
  compdef _sheaf_scd scd
fi
`,
	"fish": `# sheaf runs sheaf; after the home screen (bare sheaf) it changes the
# directory to the checkout chosen there, which sheaf writes to the file
# named by SHEAF_CD_FILE. Other commands run as without this function.
function sheaf --description 'run sheaf; cd to the checkout chosen in its home screen'
    set -l tmpdir /tmp
    if set -q TMPDIR; and test -n "$TMPDIR"
        set tmpdir $TMPDIR
    end
    set -l tmp (mktemp "$tmpdir/sheaf-cd.XXXXXX" 2>/dev/null)
    or begin
        command sheaf $argv
        return
    end
    SHEAF_CD_FILE=$tmp command sheaf $argv
    set -l rc $status
    set -l dir
    if test -s $tmp
        set dir (string collect < $tmp)
    end
    rm -f -- $tmp
    if test $rc -eq 0; and test -n "$dir"
        cd $dir
        or return
    end
    return $rc
end

function scd --description 'cd to the output of sheaf path'
    set -l dir (command sheaf path $argv)
    or return $status
    test (count $dir) -eq 1
    or return 1
    cd $dir
end

function __sheaf_scd_complete
    set -l words (commandline -opc)
    set -e words[1]
    # the current token is as typed (unlike -opc): unquote it
    set -l cur (commandline -ct | string unescape --style=script)
    set -l args
    set -l prev ''
    for a in $words "$cur"
        if test "$prev" = --bundle; and string match -q -r -- '^~(/|$)' "$a"
            set a (string replace -- '~' "$HOME" "$a")
        else if string match -q -r -- '^--bundle=~(/|$)' "$a"
            set a (string replace -- '--bundle=~' "--bundle=$HOME" "$a")
        end
        set -a args "$a"
        set prev "$a"
    end
    set -l out (command sheaf __complete path $args 2>/dev/null)
    test (count $out) -gt 0
    or return
    set -l d (string sub -s 2 -- $out[-1])
    set -l n 0
    for l in $out
        string match -q -r -- '^:[0-9]+$' "$l"
        and continue
        set l (string replace -r -- '\t.*$' '' "$l")
        test -n "$l"
        or continue
        contains -- $l --jobs -j --no-tui
        and continue
        set n (math $n + 1)
        echo $l
    end
    if test $n -eq 0; and string match -q -r -- '^(0|16)$' "$d"
        __fish_complete_directories "$cur"
    end
end
complete -c scd -f -a '(__sheaf_scd_complete)'
`,
}

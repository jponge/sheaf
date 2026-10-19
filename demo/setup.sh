#!/usr/bin/env bash
# Creates the throwaway bundle recorded by demo/sheaf.tape, offline:
#
#   demo/setup.sh DIR
#
# DIR/remotes/acme/*.git  bare "remote" repositories, reached through file://
# DIR/work                the bundle: api, docs and webapp, adopted by sheaf
# DIR/home                HOME, XDG_CONFIG_HOME, the git and gh configuration
# DIR/bin/gh              a stub that fails (and logs to DIR/gh-calls.log), so
#                         that nothing reaches GitHub
# DIR/env.sh              sourced by the recording shell (bash): isolated
#                         environment, sheaf shell function, prompt
#
# A DIR left by an earlier run of this user (it contains .sheaf-demo) is
# replaced; a symbolic link or any other existing DIR is refused, and DIR
# itself is created with mkdir (not -p), so a directory that appears in the
# meantime is never reused. The sheaf binary is the one on PATH. Commit
# dates are relative to now, so that AGE shows minutes, hours, days, weeks.
# Prints the bundle path.
set -euo pipefail

[ $# -eq 1 ] || { echo "usage: $0 DIR" >&2; exit 2; }
dir=$1
while [ "$dir" != / ] && [ "${dir%/}" != "$dir" ]; do dir=${dir%/}; done
case $dir in '' | /) echo "$0: refusing to use $1" >&2; exit 1 ;; esac
if [ -L "$dir" ]; then
  echo "$0: $dir is a symbolic link; refusing to use it" >&2
  exit 1
fi
if [ -e "$dir" ]; then
  if [ ! -d "$dir" ] || [ ! -O "$dir" ] || [ ! -e "$dir/.sheaf-demo" ]; then
    echo "$0: $dir exists and was not created by $0; refusing to replace it" >&2
    exit 1
  fi
  chmod -R u+w "$dir"
  rm -rf "$dir"
fi
mkdir -p "$(dirname "$dir")"
mkdir -m 700 "$dir" # fails if it appeared since the checks above
dir=$(cd "$dir" && pwd -P)
touch "$dir/.sheaf-demo"

remotes=$dir/remotes/acme
work=$dir/work
scratch=$dir/scratch
mkdir -p "$dir/home/.config/gh" "$dir/bin" "$remotes" "$scratch"

# Nothing from the user's environment: every GIT_* variable (configuration,
# hooks, templates, repository), the gh credentials and configuration, and
# gh itself, replaced by a stub first on PATH.
for v in $(compgen -e); do
  case $v in GIT_*) unset "$v" ;; esac
done
unset GH_TOKEN GITHUB_TOKEN GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN GH_HOST
cat >"$dir/bin/gh" <<'EOF'
#!/bin/sh
# gh stub of the sheaf demo (demo/setup.sh): nothing reaches GitHub.
echo "gh $*" >>"$(dirname "$0")/../gh-calls.log"
echo "gh is disabled in the sheaf demo (offline)" >&2
exit 1
EOF
chmod +x "$dir/bin/gh"
export PATH=$dir/bin:$PATH
export HOME=$dir/home XDG_CONFIG_HOME=$dir/home/.config GH_CONFIG_DIR=$dir/home/.config/gh
export GIT_CONFIG_GLOBAL=$dir/home/.gitconfig GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0
export GIT_ALLOW_PROTOCOL=file # no network: git refuses other transports
git config --global user.name Demo
git config --global user.email demo@example.com
git config --global init.defaultBranch main
git config --global advice.detachedHead false

now=$(date +%s)

# commit REPO AGE FILE TEXT MESSAGE: commits FILE (appending TEXT) in REPO,
# authored and committed AGE seconds ago.
commit() {
  local repo=$1 age=$2 file=$3 text=$4 msg=$5
  mkdir -p "$(dirname "$repo/$file")"
  printf '%s\n' "$text" >>"$repo/$file"
  git -C "$repo" add -- "$file"
  GIT_AUTHOR_DATE="$((now - age)) +0000" GIT_COMMITTER_DATE="$((now - age)) +0000" \
    git -C "$repo" commit -q -m "$msg"
}

min=60 hour=3600 day=86400 week=604800

# remote NAME: creates the bare remote NAME and a scratch clone to fill it.
remote() {
  git init -q --bare "$remotes/$1.git"
  git clone -q "file://$remotes/$1.git" "$scratch/$1" 2>/dev/null
}

remote api
commit "$scratch/api" $((5 * week)) README.md "# api" "Initial commit"
commit "$scratch/api" $((3 * week)) server.go "package main" "Add the HTTP server"
commit "$scratch/api" $((4 * day)) server.go "// timeouts" "Configure timeouts"
git -C "$scratch/api" push -q origin main
git -C "$scratch/api" switch -q -c fix/timeout
commit "$scratch/api" $((2 * day)) server.go "// read timeout" "Raise the read timeout"
git -C "$scratch/api" push -q origin fix/timeout

remote docs
commit "$scratch/docs" $((20 * week)) README.md "# docs" "Initial commit"
commit "$scratch/docs" $((10 * week)) guide.md "# Guide" "Write the guide"
git -C "$scratch/docs" push -q origin main

remote webapp
commit "$scratch/webapp" $((8 * week)) README.md "# webapp" "Initial commit"
commit "$scratch/webapp" $((6 * day)) app.js "render();" "Render the dashboard"
git -C "$scratch/webapp" push -q origin main

# The bundle: plain clones adopted by sheaf (moved to <repo>/main).
sheaf create "$work" >/dev/null
for r in api docs webapp; do
  git clone -q "file://$remotes/$r.git" "$work/$r"
done
out=$(cd "$work" && sheaf adopt --no-tui api docs webapp 2>&1) || {
  printf '%s\n' "$out" >&2
  exit 1
}

# api: a worktree of fix/timeout with a commit to push (clean, ↑1).
git -C "$work/api/main" worktree add -q "$work/api/fix-timeout" fix/timeout
commit "$work/api/fix-timeout" $((3 * hour)) server.go "// write timeout" "Raise the write timeout"

# webapp: a commit fetched but not merged and local changes (dirty, ↓1),
# and a worktree of a local branch (no upstream).
commit "$scratch/webapp" $((5 * hour)) app.js "theme();" "Add a theme switch"
git -C "$scratch/webapp" push -q origin main
git -C "$work/webapp/main" fetch -q
printf 'TODO: dark mode\n' >>"$work/webapp/main/README.md"
printf 'scratch\n' >"$work/webapp/main/notes.txt"
git -C "$work/webapp/main" worktree add -q -b feat/dark-mode "$work/webapp/feat-dark-mode"
commit "$work/webapp/feat-dark-mode" $((40 * min)) theme.css "body { background: #111; }" "Start the dark theme"

# api: new commits on the remote, not fetched yet: `f` shows ↓2, `u` merges.
git -C "$scratch/api" switch -q main
commit "$scratch/api" $((2 * hour)) server.go "// graceful shutdown" "Shut down gracefully"
commit "$scratch/api" $((25 * min)) server.go "// health" "Add a health endpoint"
git -C "$scratch/api" push -q origin main

rm -rf "$scratch"

# The environment of the recording shell. Values are quoted for bash.
q() { printf '%q' "$1"; }
cat >"$dir/env.sh" <<EOF
# Sourced by the shell of demo/sheaf.tape (generated by demo/setup.sh).
for v in \$(compgen -e); do
  case \$v in GIT_*) unset "\$v" ;; esac
done
unset v GH_TOKEN GITHUB_TOKEN GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN GH_HOST
unset NO_COLOR CLICOLOR CLICOLOR_FORCE
export PATH=$(q "$dir/bin"):"\$PATH"
export HOME=$(q "$dir/home") XDG_CONFIG_HOME=$(q "$dir/home/.config")
export GH_CONFIG_DIR=$(q "$dir/home/.config/gh")
export GIT_CONFIG_GLOBAL=$(q "$dir/home/.gitconfig") GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0
export GIT_ALLOW_PROTOCOL=file
export TERM=xterm-256color COLORTERM=truecolor
SHEAF_DEMO_BUNDLE=$(q "$work")
_sheaf_demo_prompt() {
  case \$PWD in
    "\$SHEAF_DEMO_BUNDLE") _sheaf_demo_dir=work ;;
    "\$SHEAF_DEMO_BUNDLE"/*) _sheaf_demo_dir=\${PWD#"\$SHEAF_DEMO_BUNDLE"/} ;;
    *) _sheaf_demo_dir=\$PWD ;;
  esac
}
PROMPT_COMMAND=_sheaf_demo_prompt
PS1='\[\e[38;2;90;86;224m\]\${_sheaf_demo_dir} >\[\e[0m\] '
eval "\$(command sheaf shell-init bash)"
cd "\$SHEAF_DEMO_BUNDLE"
EOF

echo "$work"

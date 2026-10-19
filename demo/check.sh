#!/usr/bin/env bash
# Checks demo/setup.sh without vhs (`make demo-check`): the sheaf on PATH
# runs, in plain mode, the steps of demo/sheaf.tape against a fresh demo
# bundle, offline, and the states the recording shows are asserted. Runs in
# a temporary directory, removed afterwards.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT

fail() { echo "demo-check: FAIL: $*" >&2; exit 1; }

# expect_line TEXT REGEX: TEXT has a line matching the extended REGEX.
expect_line() {
  grep -Eq -- "$2" <<<"$1" || { printf '%s\n' "$1" >&2; fail "no line matching /$2/"; }
}

# Refuses to replace a directory it did not create.
mkdir "$tmp/foreign"
if "$here/setup.sh" "$tmp/foreign" >/dev/null 2>&1; then
  fail "setup.sh replaced a directory it did not create"
fi
[ -d "$tmp/foreign" ] || fail "setup.sh removed a directory it did not create"

# Refuses a symbolic link, even to a demo directory, trailing slash or not.
"$here/setup.sh" "$tmp/target" >/dev/null
touch "$tmp/target/keep"
ln -s "$tmp/target" "$tmp/link"
for arg in "$tmp/link" "$tmp/link/" "$tmp/link//"; do
  if "$here/setup.sh" "$arg" >/dev/null 2>&1; then
    fail "setup.sh accepted the symbolic link $arg"
  fi
done
[ -e "$tmp/target/keep" ] || fail "setup.sh changed the target of a symbolic link"

# Ignores the user's GIT_* variables (here a hook directory through
# GIT_CONFIG_COUNT) and their gh credentials, and never runs their gh: a
# hook and a gh that leave a mark, first on PATH.
mkdir "$tmp/hooks" "$tmp/usergh"
printf '#!/bin/sh\ntouch "%s/hook-ran"\n' "$tmp" >"$tmp/hooks/post-commit"
printf '#!/bin/sh\ntouch "%s/user-gh-ran"\nexit 1\n' "$tmp" >"$tmp/usergh/gh"
cp "$tmp/hooks/post-commit" "$tmp/hooks/post-checkout"
chmod +x "$tmp/hooks/post-commit" "$tmp/hooks/post-checkout" "$tmp/usergh/gh"
export PATH=$tmp/usergh:$PATH GH_TOKEN=demo-check-secret GITHUB_TOKEN=demo-check-secret
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0=$tmp/hooks

# Replaces its own output (trailing slash normalized): run twice, check the
# second bundle.
"$here/setup.sh" "$tmp/demo/" >/dev/null
[ ! -e "$tmp/hook-ran" ] || fail "a hook of the user's git configuration ran during setup"
touch "$tmp/demo/stale"
bundle=$("$here/setup.sh" "$tmp/demo")
[ ! -e "$tmp/demo/stale" ] || fail "a second run kept the previous demo directory"
[ "$bundle" = "$(cd "$tmp/demo" && pwd -P)/work" ] || fail "unexpected bundle path $bundle"

# The environment of the recording shell (it also changes to the bundle).
# shellcheck source=/dev/null
. "$tmp/demo/env.sh"
[ "$PWD" = "$bundle" ] || fail "env.sh did not change to the bundle"
[ "$(type -t sheaf)" = function ] || fail "env.sh did not define the sheaf function"
[ "$(git config --global user.name)" = Demo ] || fail "git configuration not isolated"
[ -z "${GIT_CONFIG_COUNT:-}${GH_TOKEN:-}${GITHUB_TOKEN:-}" ] || fail "env.sh kept GIT_* or gh variables"
[ "$(command -v gh)" = "$tmp/demo/bin/gh" ] || fail "the gh stub is not first on PATH"

while read -r url; do
  case $url in file://*) ;; *) fail "remote URL $url is not a file:// URL" ;; esac
done < <(sed -n 's/^ *url: //p' sheaf.yaml)

# What the overview shows when it opens.
st=$(sheaf status)
expect_line "$st" '^api +main +main +origin/main +0 +0 .* clean$'
expect_line "$st" '^api +fix-timeout +fix/timeout +origin/fix/timeout +1 +0 .* clean$'
expect_line "$st" '^docs +main +main +origin/main +0 +0 .* clean$'
expect_line "$st" '^webapp +main +main +origin/main +0 +1 .* dirty$'
expect_line "$st" '^webapp +feat-dark-mode +feat/dark-mode +- +- +- .* clean$'

# f on the primaries: the new commits of api show.
sheaf fetch --no-tui >/dev/null 2>&1 || fail "sheaf fetch failed"
st=$(sheaf status)
expect_line "$st" '^api +main +main +origin/main +0 +2 .* clean$'

# u on the primaries: api fast-forwards, webapp is skipped (dirty).
out=$(sheaf update --no-tui --primary-only 2>&1) || { printf '%s\n' "$out" >&2; fail "sheaf update failed"; }
expect_line "$out" '^fast-forwarded +api/main '
expect_line "$out" '^skipped +webapp/main +uncommitted changes'
st=$(sheaf status)
expect_line "$st" '^api +main +main +origin/main +0 +0 .* clean$'
expect_line "$st" '^webapp +main +main +origin/main +0 +1 .* dirty$'

# w on api: a new branch from origin/main in api/feat-search.
path=$(sheaf wt add --no-tui api feat/search 2>/dev/null) || fail "sheaf wt add failed"
[ "$path" = "$bundle/api/feat-search" ] || fail "wt add printed $path"
[ -d "$path" ] || fail "$path does not exist"

# enter on it, then `sheaf status` from there.
cd "$path"
st=$(sheaf status)
expect_line "$st" '^api +feat-search +feat/search +'
[ "$(sheaf wt list --porcelain | wc -l)" -eq 6 ] || fail "expected 6 checkouts"

# doctor runs gh (auth status): the stub answers, the user's gh never runs.
sheaf doctor --no-tui >/dev/null 2>&1 || fail "sheaf doctor reported a failure"
grep -q '^gh auth status' "$tmp/demo/gh-calls.log" || fail "doctor did not run the gh stub"
[ ! -e "$tmp/user-gh-ran" ] || fail "the user's gh ran"
[ ! -e "$tmp/hook-ran" ] || fail "a hook of the user's git configuration ran"

echo "demo-check: OK"

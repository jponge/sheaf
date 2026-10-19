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

# Replaces its own output: run twice, check the second bundle.
"$here/setup.sh" "$tmp/demo" >/dev/null
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
sheaf doctor --no-tui >/dev/null || fail "sheaf doctor reported a failure"

echo "demo-check: OK"

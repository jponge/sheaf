# The demo of the README, rendered by `make demo` into demo/sheaf.gif.
# demo/setup.sh first creates the demo bundle in $SHEAF_DEMO_DIR, offline
# (see the Makefile); its env.sh isolates the shell from the user's
# configuration and defines the sheaf shell function.
#
# Waits (Wait+Screen on the alternate screen, where no scrollback shifts the
# lines it reads; Wait on the prompt line) let
# the work finish whatever the machine; the Sleeps that follow set how long
# each step stays on screen.

Output demo/sheaf.gif

Require sheaf
Require git

Set Shell "bash"
Set FontSize 16
Set Columns 140
Set Rows 22
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 60ms
Set CursorBlink false
Set WaitTimeout 30s

Hide
Type `source "$SHEAF_DEMO_DIR/env.sh" && clear`
Enter
Wait /work >$/
Show

# The overview of every checkout of the bundle.
Sleep 1s
Type "sheaf"
Sleep 400ms
Enter
Wait+Screen /REPO/
Sleep 2s

# Move around.
Down@500ms 4
Sleep 400ms
Up@300ms 4
Sleep 600ms

# Filter the primary checkouts and select them all.
Type "/"
Sleep 400ms
Type@120ms "main"
Sleep 600ms
Enter
Sleep 800ms
Type "a"
Sleep 1s

# Fetch them: progress view, then the report view; api is now behind.
Type "f"
Wait+Screen /sheaf fetch:/
Sleep 2.5s
Escape
Wait+Screen /↓2/
Sleep 2s

# Update them: api fast-forwards, webapp (dirty) is skipped.
Type "a"
Sleep 600ms
Type "u"
Wait+Screen /sheaf update:/
Sleep 3.5s
Escape
Wait+Screen /REPO/
Sleep 1.5s

# Add a worktree of a new branch to api.
Type "/"
Sleep 300ms
Ctrl+U
Type@120ms "api"
Sleep 500ms
Enter
Sleep 800ms
Type "w"
Wait+Screen /New worktree of api/
Sleep 800ms
Type@90ms "feat/search"
Sleep 600ms
Enter
Wait+Screen /sheaf add:/
Sleep 2.5s
Escape
Wait+Screen /REPO/
Sleep 1.5s

# Change to it: enter on its row, through the sheaf shell function.
Type "/"
Sleep 300ms
Ctrl+U
Type@120ms "search"
Sleep 500ms
Enter
Sleep 800ms
Enter
Wait /feat-search >$/
Sleep 1s

# The status of every checkout, from the new worktree.
Type "sheaf status"
Sleep 500ms
Enter
Wait /feat-search >$/
Sleep 4s

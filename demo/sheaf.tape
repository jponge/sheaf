# The demo of the README, rendered by `make demo` into demo/sheaf.gif.
# demo/setup.sh first creates the demo bundle in $SHEAF_DEMO_DIR, offline
# (see demo/record.sh); its env.sh isolates the shell from the user's
# configuration and defines the sheaf shell function.
#
# Waits let the work finish whatever the machine; the Sleeps that follow set
# how long each step stays on screen. Wait+Screen reads the lines 0 to Rows-1
# of the active buffer, scrollback included (vhs v0.12.1, testing.go:70), so
# it is only used on the alternate screen (overview, report views), which has
# no scrollback. The prompt of `w` is drawn on the normal screen below what
# the earlier actions printed, possibly past those lines: it gets a Sleep
# (nothing runs between `w` and the prompt), and the Wait for the report of
# `add` that follows fails the recording if the branch was not entered. Wait
# (current line) is used for the shell prompt.

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
Sleep 1.2s
Type@90ms "feat/search"
Sleep 600ms
Enter
Wait+Screen /sheaf wt add:/
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

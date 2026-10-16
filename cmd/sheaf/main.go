// Command sheaf manages a group of git repositories checked out side by side
// in one directory, the bundle, each with a primary checkout and linked
// worktrees as siblings. The commands are implemented in internal/cli.
package main

import (
	"os"

	"github.com/jponge/sheaf/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}

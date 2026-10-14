package main

import (
	"os"

	"github.com/jponge/sheaf/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}

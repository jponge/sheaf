package testutil

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv(skipMainVar) == "1" { // see TestFixturesRequireMain
		os.Exit(m.Run())
	}
	Main(m)
}

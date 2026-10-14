package gitx

import (
	"testing"

	"github.com/jponge/sheaf/internal/testutil"
)

// TestMain isolates git for the test process (see testutil.Main).
func TestMain(m *testing.M) { testutil.Main(m) }

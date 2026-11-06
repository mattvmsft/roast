package cli

import (
	"os"
	"testing"

	"github.com/bluesky-social/roast/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.ClearAmbientEnvironment()
	os.Exit(m.Run())
}

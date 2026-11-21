package notify_test

import (
	"os"
	"testing"
)

// TestMain keeps the operator's real notification settings out of tests: NewDispatcher
// auto-detects NTFY_URL / NTFY_TOPIC / NOTIFY_CMD, which would post to a live topic.
func TestMain(m *testing.M) {
	for _, k := range []string{"NTFY_URL", "NTFY_TOPIC", "NOTIFY_CMD"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

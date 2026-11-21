package slots_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/erinwnash/Shepherd/internal/slots"
)

func TestWrapPriority(t *testing.T) {
	cmd := []string{"go", "test", "./..."}
	wrapped := slots.WrapPriority(cmd)

	if runtime.GOOS == "windows" {
		if len(wrapped) != len(cmd) {
			t.Errorf("expected no priority wrapping on windows")
		}
		return
	}

	joined := strings.Join(wrapped, " ")
	if !strings.Contains(joined, "nice -n 10") {
		t.Errorf("expected wrapped command to include 'nice -n 10', got: %v", wrapped)
	}

	if runtime.GOOS == "linux" {
		if !strings.Contains(joined, "ionice -c 3") {
			t.Logf("note: ionice might not be present, got: %v", wrapped)
		}
	}
}

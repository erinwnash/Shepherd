package slots

import (
	"os/exec"
	"runtime"
)

// WrapPriority wraps command argv with reduced CPU and I/O scheduling priorities
// to safeguard the developer's interactive desktop, editor, and terminal from lag.
// On Linux/macOS: prepends `nice -n 10`.
// On Linux: prepends `ionice -c 3` (idle I/O class) if available.
func WrapPriority(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}

	if runtime.GOOS == "windows" {
		return argv
	}

	var prefix []string

	// Check for ionice (Linux idle I/O priority)
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("ionice"); err == nil {
			prefix = append(prefix, "ionice", "-c", "3")
		}
	}

	// Check for nice (CPU priority)
	if _, err := exec.LookPath("nice"); err == nil {
		prefix = append(prefix, "nice", "-n", "10")
	}

	return append(prefix, argv...)
}

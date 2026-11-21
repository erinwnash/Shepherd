package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/erinwnash/Shepherd/internal/eventlog"
)

// GetTopSnapshot returns a text snapshot from 'top'.
func GetTopSnapshot() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "top", "-b", "-n", "1")
	out, err := cmd.Output()
	if err != nil {
		return []string{fmt.Sprintf("Error fetching top snapshot: %v", err)}
	}

	lines := strings.Split(string(out), "\n")
	var result []string
	for i, l := range lines {
		if i >= 18 {
			break
		}
		if strings.TrimSpace(l) != "" {
			result = append(result, l)
		}
	}
	return result
}

// FindLlamaGpuTopScript searches for llamagputop.py in the repository or fallback paths.
func FindLlamaGpuTopScript() string {
	candidates := []string{
		filepath.Join("tools", "llamagputop", "llamagputop.py"),
		filepath.Join("..", "tools", "llamagputop", "llamagputop.py"),
		"/mnt/storage/utils/llamagputopOLD/llamagputop.py",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// GetLlamaGpuSnapshot runs llamagputop.py --probe or returns discovery status.
func GetLlamaGpuSnapshot() []string {
	script := FindLlamaGpuTopScript()
	if script == "" {
		return []string{"llamagputop.py not found in tools/llamagputop/ or /mnt/storage/utils/llamagputopOLD"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", script, "--probe")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return []string{fmt.Sprintf("llamagputop probe error: %v", err), string(out)}
	}

	lines := strings.Split(string(out), "\n")
	var result []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			result = append(result, l)
		}
	}
	return result
}

// GetNvtopSnapshot queries nvidia-smi or checks nvtop presence.
func GetNvtopSnapshot() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi")
	out, err := cmd.CombinedOutput()
	if err != nil {
		nvPath, nErr := exec.LookPath("nvtop")
		if nErr == nil {
			return []string{fmt.Sprintf("nvtop found at %s. Press [x] or [Enter] to launch interactive view.", nvPath)}
		}
		return []string{"Neither nvidia-smi nor nvtop is available on this system."}
	}

	lines := strings.Split(string(out), "\n")
	var result []string
	for i, l := range lines {
		if i >= 18 {
			break
		}
		result = append(result, l)
	}
	return result
}

// FormatLiveLogLines formats audit events and daemon logs for the live log monitor dashboard.
func FormatLiveLogLines(events []eventlog.Event) []string {
	var lines []string

	// If there are audit events, format up to 22 latest events
	if len(events) > 0 {
		start := 0
		if len(events) > 22 {
			start = len(events) - 22
		}
		for i := start; i < len(events); i++ {
			ev := events[i]
			ts := ev.Timestamp.Format("15:04:05")

			typeColor := FgHiCyan
			switch {
			case strings.Contains(ev.Type, "passed") || strings.Contains(ev.Type, "done"):
				typeColor = FgHiGreen
			case strings.Contains(ev.Type, "blocked") || strings.Contains(ev.Type, "error") || strings.Contains(ev.Type, "fail"):
				typeColor = FgHiRed
			case strings.Contains(ev.Type, "held") || strings.Contains(ev.Type, "pause"):
				typeColor = FgHiYellow
			case strings.Contains(ev.Type, "worker"):
				typeColor = FgHiMagenta
			case strings.Contains(ev.Type, "daemon"):
				typeColor = FgHiBlue
			}

			shortHash := ""
			if len(ev.Hash) >= 8 {
				shortHash = ev.Hash[:8]
			}

			hashStr := ""
			if shortHash != "" {
				hashStr = " (" + shortHash + ")"
			}

			line := fmt.Sprintf("[%s] #%-3d %s%-14s%s %s%-9s%s %s%s",
				ts,
				ev.Seq,
				typeColor, ev.Type, Reset,
				Bold, ev.Actor, Reset,
				ev.Payload,
				hashStr,
			)
			lines = append(lines, line)
		}
	}

	// Check if .shepherd/shepherd.log exists on disk to append trailing daemon stderr/stdout lines
	for _, p := range []string{filepath.Join(".shepherd", "shepherd.log"), "shepherd.log"} {
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
			fileLines := strings.Split(string(data), "\n")
			start := 0
			if len(fileLines) > 10 {
				start = len(fileLines) - 10
			}
			for _, fl := range fileLines[start:] {
				if strings.TrimSpace(fl) != "" {
					lines = append(lines, fmt.Sprintf("%s[log]%s %s", Dim, Reset, fl))
				}
			}
			break
		}
	}

	if len(lines) == 0 {
		lines = append(lines, fmt.Sprintf("%sWaiting for daemon activity. No events recorded yet.%s", Dim, Reset))
	}

	return lines
}

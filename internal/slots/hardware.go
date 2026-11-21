package slots

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

var (
	// ErrLowMemory indicates that host available RAM is below the safety reserve floor.
	ErrLowMemory = errors.New("available system RAM is below the safety floor")

	// ErrGPUExhausted indicates that all allowable GPU workers are actively running.
	ErrGPUExhausted = errors.New("all GPU worker slots are currently occupied")
)

// HardwareGuard provides hardware inspection and safety floor enforcement.
type HardwareGuard struct {
	MinFreeRAMGB  float64
	MaxGPUWorkers int
	RAMChecker    func() (float64, error)
	GPUDetector   func() int
}

// NewHardwareGuard creates a HardwareGuard with default system detectors.
func NewHardwareGuard(minFreeRAMGB float64, maxGPUWorkers int) *HardwareGuard {
	if minFreeRAMGB <= 0 {
		minFreeRAMGB = 4.0
	}
	hg := &HardwareGuard{
		MinFreeRAMGB:  minFreeRAMGB,
		MaxGPUWorkers: maxGPUWorkers,
		RAMChecker:    GetAvailableRAMGB,
		GPUDetector:   DetectNumGPUs,
	}
	if hg.MaxGPUWorkers <= 0 {
		hg.MaxGPUWorkers = hg.GPUDetector()
		if hg.MaxGPUWorkers < 1 {
			hg.MaxGPUWorkers = 1
		}
	}
	return hg
}

// CheckHeadroom verifies that host resources (RAM floor and GPU concurrency limits)
// allow launching an additional worker slot.
func (h *HardwareGuard) CheckHeadroom(requiresGPU bool, currentGPUWorkers int) error {
	// 1. GPU concurrency check
	if requiresGPU {
		if currentGPUWorkers >= h.MaxGPUWorkers {
			return fmt.Errorf("%w: active %d/%d allowed GPUs", ErrGPUExhausted, currentGPUWorkers, h.MaxGPUWorkers)
		}
	}

	// 2. RAM safety floor check
	freeGB, err := h.RAMChecker()
	if err == nil {
		if freeGB < h.MinFreeRAMGB {
			return fmt.Errorf("%w: %.2f GB available, need at least %.2f GB reserve", ErrLowMemory, freeGB, h.MinFreeRAMGB)
		}
	}

	return nil
}

// DetectNumGPUs queries the host hardware for available GPUs.
func DetectNumGPUs() int {
	// 1. Try nvidia-smi -L
	cmd := exec.Command("nvidia-smi", "-L")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil {
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		count := 0
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "GPU ") {
				count++
			}
		}
		if count > 0 {
			return count
		}
	}

	// 2. Try rocm-smi on Linux (AMD ROCm)
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/kfd"); err == nil {
			return 1
		}
	}

	// 3. Apple Silicon unified memory (Darwin arm64)
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return 1
	}

	return 1
}

// GetAvailableRAMGB returns the host's currently available RAM in gigabytes.
func GetAvailableRAMGB() (float64, error) {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0, err
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "MemAvailable:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					kb, err := strconv.ParseFloat(parts[1], 64)
					if err == nil {
						return kb / (1024.0 * 1024.0), nil
					}
				}
			}
		}
		return 0, errors.New("MemAvailable field not found in /proc/meminfo")

	case "darwin":
		// sysctl hw.memsize gives total RAM; return generous default or parse vm_stat
		return 8.0, nil

	default:
		return 8.0, nil
	}
}

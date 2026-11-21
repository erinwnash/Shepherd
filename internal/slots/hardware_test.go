package slots_test

import (
	"errors"
	"testing"

	"github.com/erinwnash/Shepherd/internal/slots"
)

func TestHardwareGuard_RAMFloor(t *testing.T) {
	guard := slots.NewHardwareGuard(4.0, 2)

	// Case 1: Adequate RAM (8.0 GB available)
	guard.RAMChecker = func() (float64, error) {
		return 8.0, nil
	}
	if err := guard.CheckHeadroom(false, 0); err != nil {
		t.Errorf("expected CheckHeadroom to succeed with 8GB available, got: %v", err)
	}

	// Case 2: Insufficient RAM (2.5 GB available < 4.0 GB floor)
	guard.RAMChecker = func() (float64, error) {
		return 2.5, nil
	}
	err := guard.CheckHeadroom(false, 0)
	if err == nil {
		t.Fatalf("expected ErrLowMemory when available RAM is 2.5 GB, got nil")
	}
	if !errors.Is(err, slots.ErrLowMemory) {
		t.Errorf("expected ErrLowMemory, got: %v", err)
	}
}

func TestHardwareGuard_GPUConcurrency(t *testing.T) {
	guard := slots.NewHardwareGuard(4.0, 1) // Only 1 GPU available
	guard.RAMChecker = func() (float64, error) {
		return 16.0, nil
	}

	// 0 active GPU workers -> should succeed
	if err := guard.CheckHeadroom(true, 0); err != nil {
		t.Fatalf("expected headroom check to pass for first GPU worker, got: %v", err)
	}

	// 1 active GPU worker -> second GPU worker must be rejected
	err := guard.CheckHeadroom(true, 1)
	if err == nil {
		t.Fatalf("expected ErrGPUExhausted when 1/1 GPUs in use, got nil")
	}
	if !errors.Is(err, slots.ErrGPUExhausted) {
		t.Errorf("expected ErrGPUExhausted, got: %v", err)
	}

	// Non-GPU worker can still launch even if GPU is busy
	if err := guard.CheckHeadroom(false, 1); err != nil {
		t.Fatalf("expected non-GPU worker to pass headroom check, got: %v", err)
	}
}

func TestDetectNumGPUs_Host(t *testing.T) {
	count := slots.DetectNumGPUs()
	if count < 1 {
		t.Errorf("expected at least 1 GPU detected on host or fallback, got %d", count)
	}
}

func TestGetAvailableRAMGB_Host(t *testing.T) {
	gb, err := slots.GetAvailableRAMGB()
	if err != nil {
		t.Fatalf("GetAvailableRAMGB returned error: %v", err)
	}
	if gb <= 0 {
		t.Errorf("expected positive available RAM, got %f", gb)
	}
}

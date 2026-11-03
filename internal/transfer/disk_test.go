package transfer

import (
	"errors"
	"math"
	"path/filepath"
	"testing"
)

func TestCheckAvailableSpaceNonexistentSubdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent", "subdir")

	if err := CheckAvailableSpace(dir, 1024); err != nil {
		t.Fatalf("expected 1KB to fit on existing ancestor of %q, got %v", dir, err)
	}

	err := CheckAvailableSpace(dir, math.MaxInt64)
	if !errors.Is(err, ErrInsufficientDiskSpace) {
		t.Fatalf("expected ErrInsufficientDiskSpace on nonexistent subdir %q, got %v", dir, err)
	}
}

func TestFreeSpaceOverflowClamping(t *testing.T) {
	if got := freeSpace(0, 4096); got != 0 {
		t.Errorf("freeSpace(0, 4096) = %d, want 0", got)
	}
	if got := freeSpace(1<<52, 4096); got != math.MaxInt64 {
		t.Errorf("freeSpace(1<<52, 4096) = %d, want MaxInt64 (%d)", got, math.MaxInt64)
	}
	if got := freeSpace(math.MaxUint64, math.MaxUint64); got != math.MaxInt64 {
		t.Errorf("freeSpace(MaxUint64, MaxUint64) = %d, want MaxInt64", got)
	}
	if got := freeSpace(100, 4096); got != 409600 {
		t.Errorf("freeSpace(100, 4096) = %d, want 409600", got)
	}
}

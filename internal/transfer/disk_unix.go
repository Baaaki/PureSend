//go:build !windows

package transfer

import (
	"fmt"
	"golang.org/x/sys/unix"
)

// CheckAvailableSpace verifies that dir has at least required bytes of free space.
func CheckAvailableSpace(dir string, required int64) error {
	if required <= 0 {
		return nil
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		// If statfs fails (e.g. filesystem query not supported on this mount), don't block
		return nil
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free < required {
		return fmt.Errorf("%w: %s needed, %s available", ErrInsufficientDiskSpace, formatBytes(required), formatBytes(free))
	}
	return nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

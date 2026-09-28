//go:build windows

package transfer

import (
	"fmt"
	"golang.org/x/sys/windows"
)

// CheckAvailableSpace verifies that dir has at least required bytes of free space.
func CheckAvailableSpace(dir string, required int64) error {
	if required <= 0 {
		return nil
	}
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil
	}
	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64
	err = windows.GetDiskFreeSpaceEx(dirPtr, &freeBytesAvailable, &totalNumberOfBytes, &totalNumberOfFreeBytes)
	if err != nil {
		return nil
	}
	if int64(freeBytesAvailable) < required {
		return fmt.Errorf("%w: %s needed, %s available", ErrInsufficientDiskSpace, formatBytes(required), formatBytes(int64(freeBytesAvailable)))
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

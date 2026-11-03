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
	dirPtr, err := windows.UTF16PtrFromString(existingDir(dir))
	if err != nil {
		return nil //nolint:nilerr // optional check; proceed if conversion fails
	}
	var freeBytesAvailable, totalNumberOfBytes, totalNumberOfFreeBytes uint64
	if windows.GetDiskFreeSpaceEx(dirPtr, &freeBytesAvailable, &totalNumberOfBytes, &totalNumberOfFreeBytes) != nil {
		return nil
	}
	free := freeSpace(freeBytesAvailable, 1)
	if free < required {
		return fmt.Errorf("%w: %s needed, %s available", ErrInsufficientDiskSpace, formatBytes(required), formatBytes(free))
	}
	return nil
}

//go:build linux

package transfer

import (
	"fmt"
	"golang.org/x/sys/unix"
)

// CheckAvailableSpace verifies that dir has at least required bytes of free space.
func CheckAvailableSpace(dir string, required int64) error {
	if required <= 0 {
		return nil //nolint:nilerr // non-fatal fallback if statfs unsupported
	}
	var stat unix.Statfs_t
	if unix.Statfs(dir, &stat) != nil {
		return nil //nolint:nilerr // non-fatal fallback if statfs unsupported
	}
	free := int64(stat.Bavail) * stat.Bsize
	if free < required {
		return fmt.Errorf("%w: %s needed, %s available", ErrInsufficientDiskSpace, formatBytes(required), formatBytes(free))
	}
	return nil //nolint:nilerr // non-fatal fallback if statfs unsupported
}

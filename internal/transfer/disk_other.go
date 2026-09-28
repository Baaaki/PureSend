//go:build !linux && !darwin && !windows

package transfer

// CheckAvailableSpace verifies that dir has at least required bytes of free space.
func CheckAvailableSpace(dir string, required int64) error {
	return nil
}

package transfer

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
)

func existingDir(dir string) string {
	cur := filepath.Clean(dir)
	for {
		if _, err := os.Stat(cur); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		cur = parent
	}
}

func freeSpace(bavail, bsize uint64) int64 {
	if bavail == 0 || bsize == 0 {
		return 0
	}
	if bavail > math.MaxUint64/bsize {
		return math.MaxInt64
	}
	avail := bavail * bsize
	if avail > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(avail)
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

package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDestinationResolvesSymlinkWithNonexistentSubdir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}

	tmp := t.TempDir()
	sshLink := filepath.Join(tmp, "ssh_link")
	if err := os.Symlink(filepath.Join(home, ".ssh"), sshLink); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}

	target := filepath.Join(sshLink, "nonexistent_subdir")
	if err := CheckDestination(target); !errors.Is(err, ErrUnsafeDestination) {
		t.Errorf("CheckDestination(%q) = %v, want ErrUnsafeDestination", target, err)
	}

	parentLink := filepath.Join(tmp, "parent_link")
	if err := os.Symlink(filepath.Dir(home), parentLink); err == nil {
		viaLink := filepath.Join(parentLink, filepath.Base(home))
		if err := CheckDestination(viaLink); !errors.Is(err, ErrUnsafeDestination) {
			t.Errorf("CheckDestination(%q) = %v, want ErrUnsafeDestination", viaLink, err)
		}
	}

	safeSub := filepath.Join(tmp, "new_folder", "sub")
	if err := CheckDestination(safeSub); err != nil {
		t.Errorf("CheckDestination(%q) = %v, want nil", safeSub, err)
	}
}

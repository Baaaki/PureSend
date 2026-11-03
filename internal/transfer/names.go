package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"puresend/internal/safetext"
)

// maxNameBytes is the longest single file or folder name any common
// filesystem will store.
const maxNameBytes = 255

// checkWindowsNames decides whether names Windows cannot store are refused.
// It is a variable only so the tests can exercise the Windows rules on the
// machines they actually run on.
var checkWindowsNames = runtime.GOOS == "windows"

// checkManifest refuses a manifest before anything in it is used, and
// returns where on disk each file would land. Every field arrives from the
// other side and is treated accordingly: a path that climbs out of the
// target folder, a digest that is not a digest, a size that is negative or
// that overflows the total — none of them has an honest explanation, so
// each is a reason to stop, not something to repair.
//
// The digest matters more than it looks. It names the partial download a
// resume continues from, so an unchecked one — "../../../.bashrc" — would
// be a path, and the promise that nothing lands outside the chosen folder
// would not hold for it.
func checkManifest(outDir string, m Manifest) ([]string, error) {
	invalid := func(why string) ([]string, error) {
		return nil, fmt.Errorf("the other side sent an invalid file list: %s", why)
	}
	if len(m.Files) == 0 {
		return invalid("it is empty")
	}
	if len(m.Files) > maxFiles {
		return nil, fmt.Errorf("too many files: %d, at most %d can be received at once", len(m.Files), maxFiles)
	}

	targets := make([]string, len(m.Files))
	var total int64
	for i, f := range m.Files {
		if f.Size < 0 || f.Size > math.MaxInt64-total {
			return invalid("impossible file size")
		}
		total += f.Size
		if !validDigest(f.SHA256) {
			return invalid("malformed checksum")
		}
		t, err := safeJoin(outDir, f.Path)
		if err != nil {
			return nil, err
		}
		targets[i] = t
	}
	return targets, nil
}

// validDigest accepts exactly what the sender produces: a SHA-256 in
// lowercase hex.
func validDigest(s string) bool {
	if len(s) != 2*32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// safeJoin turns a manifest path into a real one inside outDir, refusing
// anything that could escape it. The sender is not trusted to be honest
// here: "../../.bashrc", "/etc/passwd" and "C:\Windows" all have to lose.
//
// Anything suspicious is rejected rather than quietly repaired. Cleaning
// "../evil.txt" into "evil.txt" would save the file somewhere the sender
// did not ask for and the receiver was never told about; an honest sender
// never produces such a path, so one is reason to stop, not to guess.
func safeJoin(outDir, rel string) (string, error) {
	unsafe := func() (string, error) {
		return "", fmt.Errorf("the other side sent an unsafe file name: %q", rel)
	}
	// Windows separators and drive letters are rejected rather than
	// normalised: on Linux `C:\x` is a legal single file name, and
	// accepting it would create bizarre files instead of saying what is
	// wrong.
	if rel == "" || strings.ContainsAny(rel, `\:`) || strings.HasPrefix(rel, "/") {
		return unsafe()
	}
	// Control characters and bidirectional marks never belong in a name,
	// and this one is about to be printed on the approval screen. An escape
	// sequence there could redraw the very list the user is approving.
	if strings.IndexFunc(rel, safetext.IsUnsafe) >= 0 {
		return unsafe()
	}

	var parts []string
	for part := range strings.SplitSeq(rel, "/") {
		switch part {
		case "", ".":
			continue // harmless noise: "a//b", "a/./b"
		case "..":
			return unsafe()
		}
		if len(part) > maxNameBytes {
			return unsafe()
		}
		if checkWindowsNames && !windowsCanStore(part) {
			return "", fmt.Errorf("the other side sent a file name this computer cannot store: %q", rel)
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return unsafe()
	}
	// Folded: on the case-insensitive filesystems macOS and Windows use by
	// default, ".PureSend-Partial" is the very same folder.
	if strings.EqualFold(parts[0], partialDir) {
		return "", fmt.Errorf("the other side sent a reserved file name: %q", rel)
	}
	return filepath.Join(append([]string{outDir}, parts...)...), nil
}

// ErrUnsafeDestination refuses a download folder a sender could use to
// plant files where programs look for their settings.
var ErrUnsafeDestination = errors.New("files cannot be saved straight into your home folder or a folder above it; choose a folder inside it")

// CheckDestination refuses to receive into the home folder itself, any
// folder above it, or the root of a drive.
//
// A sender decides the paths inside the transfer, and safeJoin only keeps
// them inside the destination. Inside the home folder that is not enough:
// ".config/autostart/x.desktop", ".ssh/authorized_keys" or
// "Library/LaunchAgents/x.plist" are all inside it, none of them exists on
// many machines — so nothing is overwritten — and each runs something, or
// lets someone in, the next time the user logs in. No list of dangerous
// names can be complete, but a folder of its own, such as the default
// Downloads/PureSend, holds none of them.
func CheckDestination(outDir string) error {
	dest, err := canonical(outDir)
	if err != nil {
		return fmt.Errorf("could not resolve the download folder: %w", err)
	}
	if filepath.Dir(dest) == dest || atOrAboveHome(dest) {
		return fmt.Errorf("%w (%s)", ErrUnsafeDestination, outDir)
	}
	return nil
}

// atOrAboveHome reports whether dest, already canonical, is the home folder
// or a folder above it. Without a home folder there is nothing to protect.
func atOrAboveHome(dest string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	if home, err = canonical(home); err != nil {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		// Case-insensitive by default: /users/ali is /Users/ali.
		dest, home = strings.ToLower(dest), strings.ToLower(home)
	}
	// dest is at or above home exactly when home is at or below dest.
	rel, err := filepath.Rel(dest, home)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	// Hidden folders inside home (such as ~/.ssh or ~/.config) are equally unsafe.
	if fromHome, err := filepath.Rel(home, dest); err == nil {
		first, _, _ := strings.Cut(fromHome, string(filepath.Separator))
		if strings.HasPrefix(first, ".") && first != "." && first != ".." {
			return true
		}
	}
	return false
}

// canonical is p made absolute, with any symbolic links in it resolved as
// far as they exist, so that a link to the home folder is the home folder.
func canonical(p string) (string, error) {
	return canonicalDepth(p, 0)
}

func canonicalDepth(p string, depth int) (string, error) {
	if depth > 40 {
		return "", errors.New("too many levels of symbolic links")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	cur := abs
	var tail []string
	for {
		if target, err := os.Readlink(cur); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			base, err := canonicalDepth(target, depth+1)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				base = filepath.Join(base, tail[i])
			}
			return base, nil
		}
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// checkNoLinks refuses a target whose folders below outDir include a
// symbolic link. safeJoin keeps the path inside outDir as written, but a
// link already sitting in the destination — "docs" pointing at /etc —
// would carry the file wherever it points. The transfer never creates
// links, so an honest sender has no use for one.
func checkNoLinks(outDir, target string) error {
	rel, err := filepath.Rel(outDir, filepath.Dir(target))
	if err != nil || rel == "." {
		return err
	}
	dir := outDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		st, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // everything from here down is ours to create
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			// Named from the download folder down: the message also goes to
			// the sender, who has no business learning the rest of the path.
			link, _ := filepath.Rel(outDir, dir)
			return fmt.Errorf("refusing to save %s through the link %s in the download folder",
				filepath.Base(target), filepath.ToSlash(link))
		}
	}
	return nil
}

// windowsReserved are the device names Windows will not let a file take,
// with or without an extension: "nul.txt" is the null device too.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"CONIN$": true, "CONOUT$": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"COM¹": true, "COM²": true, "COM³": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"LPT¹": true, "LPT²": true, "LPT³": true,
}

// windowsCanStore reports whether Windows would store a file or folder
// under this name as it is. A name from a Mac or Linux sender can be
// perfectly ordinary there and still fail here, and it is better to say
// so before the transfer than to fail on the last rename with an error
// nobody can read.
func windowsCanStore(name string) bool {
	if strings.ContainsAny(name, `<>"|?*`) {
		return false
	}
	// Windows drops a trailing dot or space, so "a." and "a" would be the
	// same file — and "a." cannot be created at all.
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	base, _, _ := strings.Cut(name, ".")
	return !windowsReserved[strings.ToUpper(strings.TrimRight(base, " "))]
}

// maxNameTries bounds the search for a free name, so a folder with every
// "photo (n).jpg" taken ends in an error instead of a loop.
const maxNameTries = 10_000

// place moves a finished download to its target, or to a free name like
// "photo (1).jpg" when the target is taken; an existing file is never
// replaced.
//
// It hard-links rather than renames where it can. Checking that a name is
// free and then renaming onto it leaves a moment in which something else
// can appear there, and a rename replaces whatever it finds; creating a
// link fails if the name exists, so the check and the move are one step.
// Where links are not supported (FAT, exFAT, some network shares) it falls
// back to checking and renaming, and that moment is back.
func place(src, target string) (string, error) {
	for i := range maxNameTries {
		candidate := numbered(target, i)
		err := os.Link(src, candidate)
		if err == nil {
			_ = os.Remove(src)
			return candidate, nil
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		// No hard links here.
		if _, err := os.Lstat(candidate); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := os.Rename(src, candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("no free name left for %s", filepath.Base(target))
}

// numbered is target for i == 0, and "name (i).ext" after that.
func numbered(target string, i int) string {
	if i == 0 {
		return target
	}
	dir, name := filepath.Split(target)
	ext := filepath.Ext(name)
	return filepath.Join(dir, fmt.Sprintf("%s (%d)%s", name[:len(name)-len(ext)], i, ext))
}

package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one file the sender will transmit: where it lives on this
// machine, and the relative path the receiver should recreate.
type Entry struct {
	Local string // path on the sending machine
	Rel   string // slash-separated path relative to the transfer root
}

// Collect expands the user's selection into the flat list the protocol
// moves. A chosen file becomes one entry; a chosen folder becomes one entry
// per file inside it, each keeping its path relative to the folder, so the
// receiver can rebuild the tree.
//
// Symbolic links are skipped rather than followed: a link pointing outside
// the chosen folder would quietly widen what the user agreed to send, and
// one pointing back into it would loop forever. Empty folders carry no
// files and so do not survive the trip.
func Collect(paths []string) ([]Entry, error) {
	var entries []Entry
	seen := make(map[string]bool)
	usedTop := make(map[string]bool)

	for _, p := range paths {
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true

		info, err := os.Stat(clean)
		if err != nil {
			return nil, fmt.Errorf("could not read %s: %w", filepath.Base(clean), err)
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s is not an ordinary file", filepath.Base(clean))
			}
			rel := uniqueTopName(filepath.Base(clean), usedTop)
			usedTop[rel] = true
			entries = append(entries, Entry{Local: clean, Rel: rel})
			continue
		}

		base := filepath.Base(clean)
		root := uniqueTopName(base, usedTop)
		before := len(entries)
		err = filepath.WalkDir(clean, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(clean, p)
			if err != nil {
				return err
			}
			entries = append(entries, Entry{
				Local: p,
				Rel:   path.Join(root, filepath.ToSlash(rel)),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("could not read the folder %s: %w", base, err)
		}
		if len(entries) > before {
			usedTop[root] = true
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("no files to send")
	}
	if len(entries) > maxFiles {
		return nil, fmt.Errorf("too many files: %d, at most %d can be sent at once", len(entries), maxFiles)
	}
	return entries, nil
}

func uniqueTopName(base string, used map[string]bool) string {
	name := base
	for i := 1; used[name]; i++ {
		name = numbered(base, i)
	}
	return name
}

// SourceError wraps a failure to read the sender's own files. Unlike a
// dropped connection, trying the same transfer again will not help: the
// file is gone, or unreadable, and the person who chose it has to know.
type SourceError struct{ Err error }

func (e SourceError) Error() string { return e.Err.Error() }
func (e SourceError) Unwrap() error { return e.Err }

// IsSourceError reports whether err came from reading the files being sent.
func IsSourceError(err error) bool {
	var se SourceError
	return errors.As(err, &se)
}

// Offer is a sender's selection: the files it will send, and — once they
// have been read — their sizes and digests.
//
// Reading a large folder takes real time, and it used to happen after the
// receiver had connected, on every attempt, while the other person stared
// at "preparing". An Offer is read once, in the background, while the
// sender is still reading the code out; by the time anyone connects the
// manifest is usually ready. It stays correct if the files change in the
// meantime: every file is checked again before its digest is handed out,
// and read again if it was touched.
type Offer struct {
	entries []Entry

	startOnce sync.Once
	ready     chan struct{} // closed once the first read has finished

	mu    sync.Mutex
	files []digested // valid once ready is closed and err is nil
	err   error
	done  int // files read so far, for receivers who arrive early
}

// digested is one file's manifest entry plus what it looked like when it
// was read, so a later change can be noticed.
type digested struct {
	info    FileInfo
	modTime time.Time
}

// NewOffer checks the selection and returns an Offer for it. Nothing is
// read yet; that starts with Start, or with the first Send.
func NewOffer(paths []string) (*Offer, error) {
	entries, err := Collect(paths)
	if err != nil {
		return nil, err
	}
	return &Offer{entries: entries, ready: make(chan struct{})}, nil
}

// Len is how many files the offer holds.
func (o *Offer) Len() int { return len(o.entries) }

// Start begins reading the files in the background, reporting through
// hooks. Calling it again does nothing. Cancelling ctx abandons the read,
// and the offer then fails with the context's error.
func (o *Offer) Start(ctx context.Context, hooks Hooks) {
	o.startOnce.Do(func() { go o.read(ctx, hooks) })
}

// Ready is closed once the files have been read, successfully or not.
func (o *Offer) Ready() <-chan struct{} { return o.ready }

// Err is the reason the read failed, once Ready is closed.
func (o *Offer) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// Progress is how many files have been read, out of how many.
func (o *Offer) Progress() (done, total int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.done, len(o.entries)
}

func (o *Offer) read(ctx context.Context, hooks Hooks) {
	files := make([]digested, len(o.entries))
	var err error
	for i, e := range o.entries {
		hooks.prepare(e.Rel, i+1, len(o.entries))
		if files[i], err = digest(ctx, e); err != nil {
			break
		}
		o.mu.Lock()
		o.done = i + 1
		o.mu.Unlock()
	}

	o.mu.Lock()
	if err == nil {
		o.files = files
	}
	o.err = err
	o.mu.Unlock()
	close(o.ready)
}

// manifest returns the digests, first reading again any file that changed
// since it was read. Call it only once Ready is closed.
func (o *Offer) manifest(hooks Hooks) (Manifest, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return Manifest{}, o.err
	}

	m := Manifest{Files: make([]FileInfo, len(o.files))}
	for i, e := range o.entries {
		st, err := os.Stat(e.Local)
		if err != nil {
			return Manifest{}, SourceError{fmt.Errorf("could not read %s: %w", path.Base(e.Rel), err)}
		}
		if st.Size() != o.files[i].info.Size || !st.ModTime().Equal(o.files[i].modTime) {
			hooks.prepare(e.Rel, i+1, len(o.entries))
			if o.files[i], err = digest(context.Background(), e); err != nil {
				return Manifest{}, err
			}
		}
		m.Files[i] = o.files[i].info
	}
	return m, nil
}

// digest reads a file to compute its size and SHA-256. The file is looked
// at *before* it is read, so a change made while the read is under way
// leaves a stamp that no longer matches and is caught next time.
func digest(ctx context.Context, e Entry) (digested, error) {
	f, err := os.Open(e.Local)
	if err != nil {
		return digested{}, SourceError{fmt.Errorf("could not open file: %w", err)}
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return digested{}, SourceError{fmt.Errorf("could not read %s: %w", path.Base(e.Rel), err)}
	}
	if !st.Mode().IsRegular() {
		return digested{}, SourceError{fmt.Errorf("%s is not an ordinary file", path.Base(e.Rel))}
	}

	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		if ctx.Err() != nil {
			return digested{}, ctx.Err()
		}
		return digested{}, SourceError{fmt.Errorf("could not read %s: %w", path.Base(e.Rel), err)}
	}
	return digested{
		info:    FileInfo{Path: e.Rel, Size: st.Size(), SHA256: hex.EncodeToString(h.Sum(nil))},
		modTime: st.ModTime(),
	}, nil
}

// ctxReader stops a long read when its context is cancelled, so a user
// who gives up on a 50 GB folder does not leave it being read to the end.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

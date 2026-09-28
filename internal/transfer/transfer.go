// Package transfer implements the protocol that moves files directly
// between two peers. The rendezvous server plays no part in this stage.
//
// The flow: the receiver opens a stream to the sender. Both sides first
// prove they hold the same room code (see auth.go) — until that succeeds
// nothing else is said. The sender then either turns the receiver away
// (someone else already holds the room) or offers its files: a few
// progress notes if it is still reading them, then a manifest of relative
// paths, sizes and SHA-256 digests. The receiver checks the manifest,
// shows it to its user, and accepts or declines. The acceptance carries,
// for each file, how many bytes the receiver already has from an
// interrupted attempt, so a resumed transfer picks up where it stopped.
// Only then does the sender stream raw bytes in manifest order. The
// receiver verifies each digest while writing to disk and sends a final
// acknowledgement.
//
// Neither side trusts the other to keep the conversation moving: every
// wait has a deadline, so a peer that connects and goes quiet costs
// seconds, not the whole session.
package transfer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"puresend/internal/safetext"
)

// ProtocolID identifies the file transfer protocol on libp2p.
//
//	1.1.0 added the accept/decline step after the manifest.
//	2.0.0 added the room-code handshake, folder paths, resume, and the
//	      offer that lets a sender say "busy" or "still reading".
const ProtocolID = "/puresend/transfer/2.0.0"

const (
	// maxManifestBytes caps the manifest line so a malicious sender cannot
	// exhaust the receiver's memory before the manifest is even parsed.
	maxManifestBytes = 1 << 20

	// maxAuthBytes caps a single handshake message.
	maxAuthBytes = 16 << 10

	// maxAckBytes caps one ack. Resume offsets make it grow with the file
	// count, so allow room for each of them on top of the fixed part.
	maxAckBytes = 4<<10 + 32*maxFiles

	// maxFiles bounds one transfer. A folder with more files than this is
	// refused with an explanation rather than silently truncated — and it
	// keeps the manifest comfortably under maxManifestBytes.
	maxFiles = 5000

	// chunkSize is how much is read and written at a time.
	chunkSize = 32 * 1024

	// maxChunkWireSize bounds any chunk read off the wire during compressed transfer.
	maxChunkWireSize = 256 * 1024

	// compressionFlag is set on the 4-byte chunk header to indicate the chunk is compressed.
	compressionFlag uint32 = 0x80000000

	// maxRemoteText bounds a message from the other side before it is
	// shown to anyone.
	maxRemoteText = 200
)

// timeouts bound every wait on the other side. They are variables only so
// the tests can shorten them.
var timeouts = struct {
	// auth covers the whole handshake. Four small messages; a peer that
	// cannot finish them in this long is not going to.
	auth time.Duration
	// offerGap is the longest a receiver waits between two messages while
	// the sender is still reading its files. The sender speaks every
	// heartbeat, so a longer silence means it is gone.
	offerGap  time.Duration
	heartbeat time.Duration
	// approval is how long the sender waits for the receiver's answer to
	// the manifest — a person reading a list, so minutes, not seconds.
	approval time.Duration
	// idle is the longest no byte may move while files are flowing.
	idle time.Duration
	// linger is how long the receiver waits for the sender to hang up
	// after the final acknowledgement.
	linger time.Duration
}{
	auth:      30 * time.Second,
	offerGap:  30 * time.Second,
	heartbeat: time.Second,
	approval:  5 * time.Minute,
	idle:      2 * time.Minute,
	linger:    10 * time.Second,
}

// partialDir holds the half-finished downloads that make resume possible,
// inside the target directory so a resumed transfer finds them again.
const partialDir = ".puresend-partial"

// partialTTL is how long an abandoned partial download is kept before a
// later transfer sweeps it away.
const partialTTL = 7 * 24 * time.Hour

// ErrWrongCode is what every failure of the security handshake reduces
// to. How exactly it failed is deliberately not reported: the detail would
// help an attacker tell a wrong guess from a wrong protocol, and it means
// nothing to a user, who only ever needs to hear "that is not the code".
var ErrWrongCode = errors.New("the room code does not match the other side")

// ErrBusy is what a receiver hears when it holds the right code but
// someone else already has the room: the sender serves one transfer at a
// time.
var ErrBusy = errors.New("the other side is already sending these files to someone else")

// ErrChecksumMismatch is returned when the downloaded file's SHA-256 does not match the manifest.
var ErrChecksumMismatch = errors.New("checksum mismatch, the file may be corrupted")

// ErrTransferDeclined is returned when the receiver declines the transfer.
var ErrTransferDeclined = errors.New("transfer declined by receiver")

// ErrFileModified is returned when a file's size or content changes while being sent.
var ErrFileModified = errors.New("file modified while sending")

// ErrInsufficientDiskSpace is returned when the destination disk does not have enough free space.
var ErrInsufficientDiskSpace = errors.New("not enough free disk space")

// ErrConnectionLost is returned when the network connection drops during transfer.
var ErrConnectionLost = errors.New("connection lost during transfer")

// errStalled is what an expired deadline means to a person.
var errStalled = errors.New("the other side stopped responding")

// FileInfo describes a single file in the manifest.
type FileInfo struct {
	// Path is where the file belongs relative to the transfer root, always
	// slash-separated. A single chosen file is just "photo.jpg"; a file
	// inside a chosen folder keeps its structure, "holiday/2024/photo.jpg".
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Name is the part a person reads: the file's own name, without folders.
func (f FileInfo) Name() string { return path.Base(f.Path) }

// Manifest lists the files about to be sent.
type Manifest struct {
	Files      []FileInfo `json:"files"`
	Compressed bool       `json:"compressed,omitempty"`
}

// TotalSize is how many bytes the transfer will move.
func (m Manifest) TotalSize() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// offerMsg is what the sender says after the handshake. Exactly one field
// is set: zero or more progress notes while it is still reading its files,
// then the manifest — or the reason there will not be one.
type offerMsg struct {
	Preparing *preparing `json:"preparing,omitempty"`
	Manifest  *Manifest  `json:"manifest,omitempty"`
	Busy      bool       `json:"busy,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type preparing struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// ack is the receiver's answer to the manifest (accept/decline) and its
// confirmation at the end of the transfer.
type ack struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`

	// Offsets accompanies an acceptance: one entry per manifest file,
	// saying how many leading bytes the receiver already holds from an
	// earlier attempt. All zeroes for a fresh transfer.
	Offsets []int64 `json:"offsets,omitempty"`

	// Compressed confirms that the receiver supports and enables on-the-fly
	// stream compression.
	Compressed bool `json:"compressed,omitempty"`
}

// Progress describes where a transfer has got to. Overall totals are
// carried alongside the per-file ones so the interface can show a single
// honest bar for a folder of a thousand files — and work out a speed and a
// remaining time from it.
type Progress struct {
	Name         string // relative path of the file currently moving
	Index, Files int    // 1-based position of that file in the manifest
	Done, Total  int64  // bytes of this file
	OverallDone  int64
	OverallTotal int64
}

// ProgressFunc is called as bytes move.
type ProgressFunc func(Progress)

// Hooks are the optional callbacks a caller can use to follow a transfer.
type Hooks struct {
	// Prepare fires while a side reads its own files to compute or verify
	// digests, before any byte moves. A large folder can spend a while
	// here, and the interface should say so rather than look frozen.
	Prepare func(name string, index, total int)

	// Waiting fires on the receiving side while the sender is still
	// reading its files: done of total so far.
	Waiting func(done, total int)

	// Progress fires as bytes move.
	Progress ProgressFunc
}

func (h Hooks) prepare(name string, index, total int) {
	if h.Prepare != nil {
		h.Prepare(name, index, total)
	}
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// SendOptions tune Send.
type SendOptions struct {
	Hooks Hooks

	// Claim is asked once the receiver has proven it holds the code, and
	// before anything about the files is revealed. Returning false turns
	// the receiver away as busy. Nil claims unconditionally.
	//
	// Claiming only *after* the handshake is what keeps a stranger from
	// blocking the room: connecting costs nothing, but only someone with
	// the code can take the room away from the friend it was meant for.
	Claim func() bool

	// Judge, when set, is where the receiver's proof of the code is
	// judged: it is handed the comparison and returns its verdict — or
	// false without running it, to refuse the guess. A refused guess is
	// answered exactly like a wrong one. Nil judges every proof.
	//
	// A room that allows only a few guesses has to count them here, as
	// they are judged, and not once a handshake has ended: a guesser can
	// hold several handshakes open at once, each past the exchange and
	// waiting only on its verdict, and a count kept at the end would let
	// every one of them be judged before the first wrong one was counted.
	Judge func(proves func() bool) bool
}

// Send serves an offer on the stream. It runs inside the sending side's
// stream handler, which fires when the receiver opens the stream. The
// stream is any bidirectional byte pipe (a libp2p stream in practice, a
// net.Pipe in tests).
func Send(s io.ReadWriteCloser, offer *Offer, creds Credentials, opts SendOptions) error {
	defer s.Close()
	hooks := opts.Hooks
	dl := deadlinesOf(s)

	// The receiver only ever sends JSON on this stream, so a decoder may
	// safely buffer ahead — unlike the receiving side, which has raw file
	// bytes arriving behind the last JSON line. The overall budget is
	// bounded so a hostile receiver cannot make us hold an endless message
	// in memory: two handshake messages and two acks is all it ever sends.
	dec := json.NewDecoder(io.LimitReader(s, 2*maxAuthBytes+2*maxAckBytes))
	enc := json.NewEncoder(s)

	// Prove we hold the room code before revealing anything. A stranger who
	// learned our address must not get so much as the file list out of us.
	dl.both(timeouts.auth)
	if _, err := authenticate(roleSender, creds,
		func(m *authMsg) error { return dec.Decode(m) },
		func(m authMsg) error { return enc.Encode(m) },
		opts.Judge,
	); err != nil {
		return err
	}

	if opts.Claim != nil && !opts.Claim() {
		dl.write(timeouts.idle)
		_ = enc.Encode(offerMsg{Busy: true})
		return ErrBusy
	}

	// Usually the files were read while the code was being read out; if
	// not, keep the receiver company until they are.
	offer.Start(context.Background(), hooks)
	if err := waitForOffer(offer, enc, dl); err != nil {
		return err
	}
	manifest, err := offer.manifest(hooks)
	if err != nil {
		dl.write(timeouts.idle)
		_ = enc.Encode(offerMsg{Error: "could not read the files being sent"})
		return err
	}
	manifest.Compressed = true
	dl.write(timeouts.idle)
	if err := enc.Encode(offerMsg{Manifest: &manifest}); err != nil {
		return fmt.Errorf("could not send manifest: %w", describe(err))
	}

	// The receiver inspects the manifest and explicitly accepts before a
	// single file byte is sent.
	dl.read(timeouts.approval)
	var goAhead ack
	if err := dec.Decode(&goAhead); err != nil {
		return fmt.Errorf("no answer from receiver: %w", describe(err))
	}
	if !goAhead.OK {
		cleaned := safetext.Clean(goAhead.Error, maxRemoteText)
		if strings.Contains(cleaned, "declined") {
			return fmt.Errorf("%w: %s", ErrTransferDeclined, cleaned)
		}
		if strings.Contains(cleaned, "disk space") {
			return fmt.Errorf("%w: %s", ErrInsufficientDiskSpace, cleaned)
		}
		return fmt.Errorf("receiver declined: %s", cleaned)
	}
	offsets, err := checkOffsets(goAhead.Offsets, manifest)
	if err != nil {
		return err
	}
	useCompression := manifest.Compressed && goAhead.Compressed

	// Send the bytes of each file, in manifest order.
	w := bufio.NewWriter(s)
	total := manifest.TotalSize()
	var base int64
	for i, info := range manifest.Files {
		report := progressReporter(hooks, i, len(manifest.Files), base, total)
		if err := sendFile(w, dl, offer.entries[i].Local, info, offsets[i], report, useCompression); err != nil {
			return err
		}
		base += info.Size
	}
	dl.write(timeouts.idle)
	if err := w.Flush(); err != nil {
		return fmt.Errorf("connection lost while sending: %w", describe(err))
	}

	// Wait for the receiver's acknowledgement so we don't declare success
	// before the files are safely on disk over there.
	dl.read(timeouts.idle)
	var a ack
	if err := dec.Decode(&a); err != nil {
		return fmt.Errorf("no acknowledgement from receiver: %w", describe(err))
	}
	if !a.OK {
		return fmt.Errorf("error on the receiving side: %s", safetext.Clean(a.Error, maxRemoteText))
	}
	return nil
}

// SendPaths offers the given files and folders on the stream straight
// away, without reading them ahead of time or claiming anything — the
// whole of Send, for a caller that serves exactly one receiver.
func SendPaths(s io.ReadWriteCloser, paths []string, creds Credentials, hooks Hooks) error {
	offer, err := NewOffer(paths)
	if err != nil {
		s.Close()
		return err
	}
	return Send(s, offer, creds, SendOptions{Hooks: hooks})
}

// waitForOffer blocks until the offer has been read, telling the receiver
// how far along it is every heartbeat so it knows the sender is alive.
func waitForOffer(o *Offer, enc *json.Encoder, dl deadlines) error {
	tick := time.NewTicker(timeouts.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-o.Ready():
			return nil
		default:
		}
		done, total := o.Progress()
		dl.write(timeouts.idle)
		if err := enc.Encode(offerMsg{Preparing: &preparing{Done: done, Total: total}}); err != nil {
			return fmt.Errorf("connection lost while preparing: %w", describe(err))
		}
		select {
		case <-o.Ready():
			return nil
		case <-tick.C:
		}
	}
}

// checkOffsets validates the resume offsets the receiver asked for. They
// come from the other side, so they are treated as untrusted input: an
// offset past the end of a file would otherwise make the sender seek into
// nowhere and stall the transfer.
func checkOffsets(offsets []int64, m Manifest) ([]int64, error) {
	if len(offsets) == 0 {
		return make([]int64, len(m.Files)), nil
	}
	if len(offsets) != len(m.Files) {
		return nil, fmt.Errorf("receiver asked to resume %d files but %d were offered", len(offsets), len(m.Files))
	}
	for i, off := range offsets {
		if off < 0 || off > m.Files[i].Size {
			return nil, fmt.Errorf("receiver asked to resume %s at an impossible position", m.Files[i].Name())
		}
	}
	return offsets, nil
}

// progressReporter adapts the per-file callback used inside sendFile and
// receiveFile to the manifest-wide Progress the caller sees.
func progressReporter(hooks Hooks, index, files int, base, overallTotal int64) func(name string, done, total int64) {
	if hooks.Progress == nil {
		return nil
	}
	return func(name string, done, total int64) {
		hooks.Progress(Progress{
			Name:         name,
			Index:        index + 1,
			Files:        files,
			Done:         done,
			Total:        total,
			OverallDone:  base + done,
			OverallTotal: overallTotal,
		})
	}
}

// sendFile writes a single file's bytes in chunks, starting at offset,
// reporting progress after each chunk. When compressed is true, chunks
// are framed and compressed on-the-fly using Huffman coding.
func sendFile(w io.Writer, dl deadlines, local string, info FileInfo, offset int64, onProgress func(string, int64, int64), compressed bool) error {
	if onProgress != nil {
		onProgress(info.Path, offset, info.Size)
	}
	if offset == info.Size {
		return nil // the receiver has all of it already
	}

	f, err := os.Open(local)
	if err != nil {
		return SourceError{fmt.Errorf("could not open file: %w", err)}
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return SourceError{fmt.Errorf("could not continue %s where it stopped: %w", info.Name(), err)}
		}
	}

	buf := make([]byte, chunkSize)
	var compBuf []byte
	var hdr [4]byte
	sent := offset
	for sent < info.Size {
		// Never read past the size the manifest promised. A file that grew
		// since it was read would otherwise put its extra bytes on the
		// wire, where the receiver takes them for the start of the next
		// file. The first info.Size bytes are what the digest covers.
		n, err := f.Read(buf[:min(int64(len(buf)), info.Size-sent)])
		if n > 0 {
			dl.write(timeouts.idle)
			if compressed {
				compBuf = CompressChunk(compBuf, buf[:n])
				if len(compBuf) < n {
					binary.BigEndian.PutUint32(hdr[:], uint32(len(compBuf))|compressionFlag)
					if _, werr := w.Write(hdr[:]); werr != nil {
						return fmt.Errorf("connection lost while sending %s: %w", info.Name(), describe(werr))
					}
					if _, werr := w.Write(compBuf); werr != nil {
						return fmt.Errorf("connection lost while sending %s: %w", info.Name(), describe(werr))
					}
				} else {
					binary.BigEndian.PutUint32(hdr[:], uint32(n))
					if _, werr := w.Write(hdr[:]); werr != nil {
						return fmt.Errorf("connection lost while sending %s: %w", info.Name(), describe(werr))
					}
					if _, werr := w.Write(buf[:n]); werr != nil {
						return fmt.Errorf("connection lost while sending %s: %w", info.Name(), describe(werr))
					}
				}
			} else {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return fmt.Errorf("connection lost while sending %s: %w", info.Name(), describe(werr))
				}
			}
			sent += int64(n)
			if onProgress != nil {
				onProgress(info.Path, sent, info.Size)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return SourceError{fmt.Errorf("could not read %s: %w", info.Name(), err)}
		}
	}
	if sent != info.Size {
		return fmt.Errorf("%w: size of %s changed while sending (expected %d, read %d)", ErrFileModified, info.Name(), info.Size, sent)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Receiving
// ---------------------------------------------------------------------------

// Receive saves the files arriving on the stream into outDir and returns
// the paths of the saved files. Before anything is written, confirm is
// called with the manifest; returning false declines the transfer (a nil
// confirm accepts everything).
func Receive(s io.ReadWriteCloser, outDir string, creds Credentials, confirm func(Manifest) bool, hooks Hooks) ([]string, error) {
	defer s.Close()
	if err := CheckDestination(outDir); err != nil {
		return nil, err
	}
	dl := deadlinesOf(s)

	// Careful: json.Decoder cannot be used on this side. It buffers extra
	// data from the stream internally and would swallow the beginning of
	// the file bytes that follow the last JSON line.
	r := bufio.NewReader(s)
	enc := json.NewEncoder(s)
	refuse := func(err error) error {
		dl.write(timeouts.idle)
		_ = enc.Encode(ack{OK: false, Error: peerMessage(err)})
		return err
	}

	dl.both(timeouts.auth)
	if _, err := authenticate(roleReceiver, creds,
		func(m *authMsg) error { return readJSONLine(r, maxAuthBytes, m) },
		func(m authMsg) error { return enc.Encode(m) },
		nil,
	); err != nil {
		return nil, err
	}

	manifest, err := readOffer(r, dl, hooks)
	if err != nil {
		return nil, err
	}
	// Check everything before showing the list to the user: a transfer that
	// would have to be refused halfway through should be refused now.
	targets, err := checkManifest(outDir, manifest)
	if err != nil {
		return nil, refuse(err)
	}

	if err := CheckAvailableSpace(outDir, manifest.TotalSize()); err != nil {
		return nil, refuse(err)
	}

	// Let the user inspect what is coming — name and size of every file —
	// and answer the sender before any disk space is used. No deadline of
	// ours while a person decides; the sender has its own.
	dl.both(0)
	if confirm != nil && !confirm(manifest) {
		return nil, refuse(fmt.Errorf("%w: transfer declined", ErrTransferDeclined))
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, refuse(fmt.Errorf("could not create target directory: %w", err))
	}
	sweepPartials(outDir)

	// Work out what we already have. Anything left behind by an earlier
	// interrupted attempt is picked up here instead of downloaded again.
	partials, err := resumeFrom(outDir, manifest, targets, hooks)
	if err != nil {
		return nil, refuse(err)
	}
	offsets := make([]int64, len(partials))
	for i, p := range partials {
		offsets[i] = p.offset
	}
	useCompression := manifest.Compressed
	dl.write(timeouts.idle)
	if err := enc.Encode(ack{OK: true, Offsets: offsets, Compressed: useCompression}); err != nil {
		return nil, fmt.Errorf("could not answer the sender: %w", describe(err))
	}

	total := manifest.TotalSize()
	var saved []string
	var base int64
	for i, f := range manifest.Files {
		final, err := receiveFile(r, dl, outDir, targets[i], f, partials[i],
			progressReporter(hooks, i, len(manifest.Files), base, total), useCompression)
		if err != nil {
			// Let the sender know, then propagate the error.
			return saved, refuse(err)
		}
		saved = append(saved, final)
		base += f.Size
	}

	dl.write(timeouts.idle)
	if err := enc.Encode(ack{OK: true}); err != nil {
		return saved, fmt.Errorf("could not send acknowledgement: %w", describe(err))
	}
	// Everything arrived, so there is nothing left to resume.
	forgetFinished(outDir, manifest)

	// Linger until the sender closes the stream — it only does that after
	// reading our ack. Returning (and closing) right away lets the process
	// exit before the ack is actually transmitted, and the sender would
	// wait for it in vain.
	dl.read(timeouts.linger)
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 1))
	return saved, nil
}

// readOffer reads what the sender says after the handshake, passing on any
// progress notes, until the manifest arrives or the sender says why it
// will not.
func readOffer(r *bufio.Reader, dl deadlines, hooks Hooks) (Manifest, error) {
	for {
		dl.read(timeouts.offerGap)
		var msg offerMsg
		if err := readJSONLine(r, maxManifestBytes, &msg); err != nil {
			return Manifest{}, fmt.Errorf("could not read manifest: %w", describe(err))
		}
		switch {
		case msg.Manifest != nil:
			return *msg.Manifest, nil
		case msg.Busy:
			return Manifest{}, ErrBusy
		case msg.Error != "":
			return Manifest{}, fmt.Errorf("the sender could not prepare its files: %s",
				safetext.Clean(msg.Error, maxRemoteText))
		case msg.Preparing != nil:
			if hooks.Waiting != nil {
				hooks.Waiting(msg.Preparing.Done, msg.Preparing.Total)
			}
		default:
			return Manifest{}, fmt.Errorf("could not read manifest: unexpected message")
		}
	}
}

// partial is what the receiver already holds of one manifest file.
type partial struct {
	path   string    // the .part file backing it, inside partialDir
	offset int64     // how many bytes of the file it already contains
	hasher hash.Hash // digest of those bytes, ready to continue

	// complete is set when the whole file is already in place at its
	// target, from an earlier attempt that got further than this one.
	complete string
}

// resumeFrom looks for leftovers from an earlier attempt at the same
// files. A partial download is keyed by the file's digest rather than its
// name, so it is picked up again no matter what the file was called or
// where in the tree it sat — and two different files can never be mistaken
// for one another.
//
// Two *identical* files can, though: a folder holding the same photo
// twice. Each copy after the first gets a partial of its own, numbered by
// its place among the copies, so the first one finishing does not pull the
// ground out from under the second.
//
// Files an earlier, interrupted attempt finished are not fetched again: if
// that attempt left a mark for the file and the target still holds exactly
// it, it is kept as it is.
//
// Only files this program finished count, never any file that happens to
// be at the target. The offsets go back to the sender, and "I already have
// this one" for an arbitrary file would let a sender ask, file by file and
// digest by digest, what the receiver's folder holds. The mark limits that
// to what an interrupted transfer into this folder left there — and a
// transfer that completes clears its marks.
//
// The digest of a partial cannot be checked against the manifest until the
// file is complete, so a partial that does not actually match (the sender
// edited the file between attempts, say) is only caught by the final digest
// check, which then throws it away. That costs one repeated download in a
// rare case, and never yields a wrong file.
func resumeFrom(outDir string, m Manifest, targets []string, hooks Hooks) ([]partial, error) {
	dir := filepath.Join(outDir, partialDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("could not prepare the download folder: %w", err)
	}

	out := make([]partial, len(m.Files))
	copies := make(map[string]int, len(m.Files))
	for i, f := range m.Files {
		name := f.SHA256 + ".part"
		if n := copies[f.SHA256]; n > 0 {
			name = fmt.Sprintf("%s.%d.part", f.SHA256, n)
		}
		copies[f.SHA256]++
		p := partial{path: filepath.Join(dir, name), hasher: sha256.New()}

		if finishedBefore(outDir, f.SHA256) && checkNoLinks(outDir, targets[i]) == nil &&
			haveAlready(targets[i], f, func() { hooks.prepare(f.Path, i+1, len(m.Files)) }) {
			p.complete, p.offset = targets[i], f.Size
			os.Remove(p.path)
			out[i] = p
			continue
		}

		st, err := os.Stat(p.path)
		switch {
		case err != nil || !st.Mode().IsRegular() || st.Size() == 0:
			// Nothing usable; start from the beginning.
		case st.Size() > f.Size:
			// Longer than the file it claims to be: not a prefix of it.
			os.Remove(p.path)
		default:
			hooks.prepare(f.Path, i+1, len(m.Files))
			if err := hashInto(p.hasher, p.path); err != nil {
				// Unreadable leftovers are not worth failing over.
				os.Remove(p.path)
				p.hasher = sha256.New()
			} else {
				p.offset = st.Size()
			}
		}
		out[i] = p
	}
	return out, nil
}

// haveAlready reports whether target already holds exactly this file.
func haveAlready(target string, f FileInfo, announce func()) bool {
	st, err := os.Stat(target)
	if err != nil || !st.Mode().IsRegular() || st.Size() != f.Size {
		return false
	}
	announce()
	h := sha256.New()
	if err := hashInto(h, target); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == f.SHA256
}

// finishedMark is the file an attempt leaves behind in partialDir for each
// file it finished, keyed by digest like the partial downloads themselves.
func finishedMark(outDir, digest string) string {
	return filepath.Join(outDir, partialDir, digest+".done")
}

// finishedBefore reports whether an earlier attempt finished a file with
// this digest.
func finishedBefore(outDir, digest string) bool {
	st, err := os.Lstat(finishedMark(outDir, digest))
	return err == nil && st.Mode().IsRegular()
}

// forgetFinished clears the marks of a transfer that completed, and the
// folder they were kept in if nothing else is waiting there.
func forgetFinished(outDir string, m Manifest) {
	for _, f := range m.Files {
		os.Remove(finishedMark(outDir, f.SHA256))
	}
	os.Remove(filepath.Join(outDir, partialDir)) // fails, harmlessly, unless empty
}

// hashInto feeds a file's whole content into h.
func hashInto(h hash.Hash, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(h, f)
	return err
}

// sweepPartials removes abandoned downloads, so a folder that once saw a
// failed 4 GB transfer does not keep the space forever.
func sweepPartials(outDir string) {
	dir := filepath.Join(outDir, partialDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < partialTTL {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}

// receiveFile writes a single file to disk and verifies its SHA-256 digest.
// Bytes land in a .part file first; the final name only appears once the
// digest matches, so an interrupted transfer can never leave a corrupt file
// that looks complete — and what it does leave behind is exactly what the
// next attempt resumes from.
func receiveFile(r io.Reader, dl deadlines, outDir, target string, info FileInfo, p partial, onProgress func(string, int64, int64), compressed bool) (string, error) {
	if p.complete != "" {
		if onProgress != nil {
			onProgress(info.Path, info.Size, info.Size)
		}
		return p.complete, nil
	}

	f, err := os.OpenFile(p.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", fmt.Errorf("could not create file: %w", err)
	}
	defer f.Close()

	// Drop anything past the point our digest state accounts for.
	if err := f.Truncate(p.offset); err != nil {
		return "", fmt.Errorf("could not prepare %s: %w", info.Name(), err)
	}
	if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
		return "", fmt.Errorf("could not prepare %s: %w", info.Name(), err)
	}

	buf := make([]byte, chunkSize)
	var wireBuf []byte
	var decompBuf []byte
	var hdr [4]byte
	received := p.offset
	if onProgress != nil {
		onProgress(info.Path, received, info.Size)
	}
	for received < info.Size {
		if compressed {
			dl.read(timeouts.idle)
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return "", fmt.Errorf("connection lost while receiving %s: %w", info.Name(), describe(err))
			}
			hdrVal := binary.BigEndian.Uint32(hdr[:])
			isComp := (hdrVal & compressionFlag) != 0
			wireLen := int(hdrVal & ^compressionFlag)
			if wireLen <= 0 || wireLen > maxChunkWireSize {
				return "", fmt.Errorf("invalid chunk length (%d) for %s", wireLen, info.Name())
			}

			if cap(wireBuf) < wireLen {
				wireBuf = make([]byte, wireLen)
			} else {
				wireBuf = wireBuf[:wireLen]
			}

			dl.read(timeouts.idle)
			if _, err := io.ReadFull(r, wireBuf); err != nil {
				return "", fmt.Errorf("connection lost while receiving %s: %w", info.Name(), describe(err))
			}

			var plain []byte
			if isComp {
				var err error
				plain, err = DecompressChunk(decompBuf, wireBuf, chunkSize*2)
				if err != nil {
					return "", fmt.Errorf("decompression error for %s: %w", info.Name(), err)
				}
				decompBuf = plain
			} else {
				plain = wireBuf
			}

			// A chunk that runs past the end of the file is a lie about its
			// size; the digest would catch it at the end, after the disk
			// had taken all of it.
			if int64(len(plain)) > info.Size-received {
				return "", fmt.Errorf("the other side sent more of %s than it said it would", info.Name())
			}

			if _, werr := f.Write(plain); werr != nil {
				return "", fmt.Errorf("could not write to disk: %w", werr)
			}
			p.hasher.Write(plain)
			received += int64(len(plain))
			if onProgress != nil {
				onProgress(info.Path, received, info.Size)
			}
		} else {
			chunk := min(int64(len(buf)), info.Size-received)
			// io.ReadFull tolerates a Read that returns the final bytes
			// together with io.EOF; a bare Read loop would misreport that as a
			// lost connection.
			dl.read(timeouts.idle)
			n, err := io.ReadFull(r, buf[:chunk])
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					return "", fmt.Errorf("could not write to disk: %w", werr)
				}
				p.hasher.Write(buf[:n])
				received += int64(n)
				if onProgress != nil {
					onProgress(info.Path, received, info.Size)
				}
			}
			if err != nil {
				// Keep the .part file: this is precisely the case resume exists
				// for, and the next attempt will continue from here.
				return "", fmt.Errorf("connection lost while receiving %s: %w", info.Name(), describe(err))
			}
		}
	}

	if got := hex.EncodeToString(p.hasher.Sum(nil)); got != info.SHA256 {
		// The leftovers are poisoned — whatever they are, they are not this
		// file — so do not let the next attempt resume from them.
		os.Remove(p.path)
		return "", fmt.Errorf("%w: checksum mismatch for %s, the file may be corrupted", ErrChecksumMismatch, info.Name())
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("could not finish writing %s: %w", info.Name(), err)
	}
	if err := checkNoLinks(outDir, target); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("could not create the folder for %s: %w", info.Name(), err)
	}

	// The very same file already at the target — the same folder sent
	// twice — is kept rather than joined by a numbered copy of itself. The
	// bytes came over the wire all the same, so the sender cannot tell.
	final := target
	if haveAlready(target, info, func() {}) {
		os.Remove(p.path)
	} else if final, err = place(p.path, target); err != nil {
		return "", fmt.Errorf("could not finalize %s: %w", info.Name(), err)
	}
	// Mark it finished, so that if a later file fails, the retry does not
	// fetch this one again. Best effort: without the mark it is fetched
	// again, which costs time, not correctness.
	_ = os.WriteFile(finishedMark(outDir, info.SHA256), nil, 0o600)
	return final, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// deadlines arms a stream's read and write deadlines, when it has them.
// Every wait on the other side needs an end: a peer that connects and then
// says nothing must not hold a transfer — or the sender's room — forever.
// A stream without deadlines (only ever a test double) never times out.
type deadlines struct {
	s interface {
		SetReadDeadline(time.Time) error
		SetWriteDeadline(time.Time) error
	}
}

func deadlinesOf(s any) deadlines {
	var d deadlines
	d.s, _ = s.(interface {
		SetReadDeadline(time.Time) error
		SetWriteDeadline(time.Time) error
	})
	return d
}

// at turns "this long from now" into a deadline; zero clears it.
func at(after time.Duration) time.Time {
	if after <= 0 {
		return time.Time{}
	}
	return time.Now().Add(after)
}

func (d deadlines) read(after time.Duration) {
	if d.s != nil {
		_ = d.s.SetReadDeadline(at(after))
	}
}

func (d deadlines) write(after time.Duration) {
	if d.s != nil {
		_ = d.s.SetWriteDeadline(at(after))
	}
}

func (d deadlines) both(after time.Duration) {
	d.read(after)
	d.write(after)
}

// peerMessage is what the other side is told about a failure here: the
// same words, less the local paths a filesystem error carries. Where the
// receiver keeps its downloads — and the account name in that path — is
// nothing the sender needs; "no space left on device" is.
func peerMessage(err error) string {
	msg := err.Error()
	var pe *os.PathError
	if errors.As(err, &pe) {
		msg = strings.Replace(msg, pe.Error(), pe.Err.Error(), 1)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		msg = strings.Replace(msg, le.Error(), le.Err.Error(), 1)
	}
	return msg
}

// describe turns an expired deadline into what it means to a person: the
// other side went quiet. Anything else passes through.
func describe(err error) error {
	var t interface{ Timeout() bool }
	if errors.As(err, &t) && t.Timeout() {
		return errStalled
	}
	return err
}

// readJSONLine reads one \n-terminated JSON message of at most max bytes.
func readJSONLine(r *bufio.Reader, max int, v any) error {
	line, err := readLimitedLine(r, max)
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// readLimitedLine reads one \n-terminated line of at most max bytes,
// unlike bufio.Reader.ReadBytes which buffers without any bound.
func readLimitedLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return nil, fmt.Errorf("message exceeds %d bytes", max)
		}
		if err == nil {
			return line, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

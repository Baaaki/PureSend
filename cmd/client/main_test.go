package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"puresend/internal/p2p"
	"puresend/internal/rendezvous"
	"puresend/internal/transfer"
)

func TestDetermineExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitCodeSuccess},
		{"canceled", context.Canceled, ExitCodeSignal},
		{"declined", transfer.ErrTransferDeclined, ExitCodeCanceled},
		{"checksum", transfer.ErrChecksumMismatch, ExitCodeIO},
		{"disk space", transfer.ErrInsufficientDiskSpace, ExitCodeIO},
		{"permission", os.ErrPermission, ExitCodeIO},
		{"wrong code", transfer.ErrWrongCode, ExitCodeAuth},
		{"too many wrong", p2p.ErrTooManyWrongCodes, ExitCodeAuth},
		{"room expired", p2p.ErrRoomExpired, ExitCodeAuth},
		{"room not found", rendezvous.ErrRoomNotFound, ExitCodeAuth},
		{"in use", rendezvous.ErrInUse, ExitCodeAuth},
		{"rendezvous unreachable", p2p.ErrRendezvousUnreachable, ExitCodeNetwork},
		{"peer unreachable", p2p.ErrPeerUnreachable, ExitCodeNetwork},
		{"connection lost", transfer.ErrConnectionLost, ExitCodeNetwork},
		{"code error", &p2p.CodeError{Code: "bad"}, ExitCodeUsage},
		{"source error", transfer.SourceError{Err: errors.New("missing")}, ExitCodeUsage},
		{"unsafe dest", transfer.ErrUnsafeDestination, ExitCodeUsage},
		{"not exist", fmt.Errorf("open: %w", os.ErrNotExist), ExitCodeUsage},
		{"generic", errors.New("something unexpected"), ExitCodeGeneral},
	}

	for _, tc := range cases {
		if got := determineExitCode(tc.err); got != tc.want {
			t.Errorf("determineExitCode(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestSendFlagSupportsCommasAndRepeats(t *testing.T) {
	dir := t.TempDir()
	commaFile := filepath.Join(dir, "Invoice, 2026.pdf")
	if err := os.WriteFile(commaFile, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	normalFile := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(normalFile, []byte("txt"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Literal file containing a comma must not be split.
	var f1 sendFlag
	_ = f1.Set(commaFile)
	if got := f1.paths(); !reflect.DeepEqual(got, []string{commaFile}) {
		t.Errorf("comma filename split unexpectedly: %v", got)
	}

	// Comma-separated list of multiple files still splits for backwards compatibility.
	var f2 sendFlag
	_ = f2.Set(normalFile + "," + filepath.Join(dir, "other.txt"))
	want2 := []string{normalFile, filepath.Join(dir, "other.txt")}
	if got := f2.paths(); !reflect.DeepEqual(got, want2) {
		t.Errorf("comma-separated paths = %v, want %v", got, want2)
	}

	// Repeatable -send flags combine cleanly, even with commas in names.
	var f3 sendFlag
	_ = f3.Set(commaFile)
	_ = f3.Set(normalFile)
	if got := f3.paths(); !reflect.DeepEqual(got, []string{commaFile, normalFile}) {
		t.Errorf("repeated flags = %v, want [%q %q]", got, commaFile, normalFile)
	}
}

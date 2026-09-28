// Package headless drives a transfer without a terminal interface.
//
// It exists for three reasons. Scripts and servers with no terminal need a
// way in. End-to-end tests need one too — before this, the only way to
// exercise the real program was to drive the TUI through a pseudo-terminal
// and read room codes off the screen, which is as brittle as it sounds.
// And it keeps the promise the p2p package makes in its own doc comment:
// that the same logic works under a TUI, a CLI or a test.
//
// Output is deliberately plain and line-oriented, so a shell script can
// read it: the room code goes to stdout on its own line, everything else
// to stderr.
package headless

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"puresend/internal/i18n"
	"puresend/internal/p2p"
	"puresend/internal/transfer"
)

// Send publishes the given files and folders under a fresh room code,
// prints the code, and blocks until one transfer has completed.
//
// Like the interface, it keeps the room open when an attempt fails: a
// dropped connection is the receiver's cue to try the same code again,
// and a script that exited at the first hiccup would throw the code away
// with it. It gives up only when the code itself stops working.
func Send(servers []string, paths []string, lang string, opts ...p2p.Option) error {
	l := i18n.Normalize(lang)

	// Eager validation: verify files and collect entries before touching the network.
	if len(paths) == 0 {
		return fmt.Errorf("no files to send")
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return transfer.SourceError{Err: fmt.Errorf("could not read %s: %w", filepath.Base(p), err)}
		}
	}
	if _, err := transfer.Collect(paths); err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	node, err := p2p.New(ctx, servers, opts...)
	if err != nil {
		return err
	}
	defer node.Close() //nolint:errcheck // shutting down; nothing to recover

	room, err := node.Host(ctx, paths)
	if err != nil {
		return err
	}
	// stdout, alone on its line: this is the one piece of output another
	// program is meant to parse.
	fmt.Println(room)
	if l == i18n.TR {
		logf("alıcı bekleniyor, kod %s", room)
	} else {
		logf("waiting for the receiver, code %s", room)
	}

	return pump(ctx, node, nil, true, l)
}

// Receive downloads the given room code into outDir. Unless autoAccept is
// set, the file list is printed and confirmed on the terminal first.
func Receive(servers []string, room, outDir string, autoAccept bool, lang string, opts ...p2p.Option) error {
	l := i18n.Normalize(lang)

	// A malformed code fails here, before anything touches the network.
	room, err := p2p.CheckCode(room)
	if err != nil {
		return err
	}

	// Eager validation: refuse unsafe destination folder before touching the network.
	if err := transfer.CheckDestination(outDir); err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	node, err := p2p.New(ctx, servers, opts...)
	if err != nil {
		return err
	}
	defer node.Close() //nolint:errcheck // shutting down; nothing to recover

	go node.Fetch(ctx, room, outDir)
	return pump(ctx, node, func(m transfer.Manifest) bool {
		if l == i18n.TR {
			logf("gelen: %d dosya, %s", len(m.Files), formatBytes(m.TotalSize()))
		} else {
			logf("incoming: %d file(s), %s", len(m.Files), formatBytes(m.TotalSize()))
		}
		for _, f := range m.Files {
			logf("  %s (%s)", f.Path, formatBytes(f.Size))
		}
		if autoAccept {
			return true
		}
		return askYesNo(l)
	}, false, l)
}

// pump consumes the node's events until the transfer ends, reporting
// progress on stderr. confirm answers the manifest question; a nil confirm
// declines, which is what a sender should do if it is ever asked. A host
// keeps going after a failed attempt, since its room is still open.
func pump(ctx context.Context, node *p2p.Node, confirm func(transfer.Manifest) bool, host bool, l i18n.Lang) error {
	var lastPrinted, lastPrep time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-node.Done():
			if l == i18n.TR {
				return fmt.Errorf("transfer tamamlanmadan durduruldu")
			}
			return fmt.Errorf("stopped before the transfer finished")
		case ev := <-node.Events():
			switch e := ev.(type) {
			case p2p.StatusEvent:
				if l == i18n.TR {
					switch e.Text {
					case p2p.StatusLookingUp:
						logf("kod sorgulanıyor")
					case p2p.StatusConnecting:
						logf("karşı bilgisayara bağlanılıyor")
					case p2p.StatusDirect:
						logf("doğrudan bağlantı yolu açılıyor")
					default:
						logf("%s", e.Text)
					}
				} else {
					logf("%s", e.Text)
				}

			case p2p.ConnectedEvent:
				logf("%s", describeConnection(e, l))

			case p2p.PreparingEvent:
				// A folder of thousands would otherwise print thousands of
				// lines; the first, the last and one a second is plenty.
				if e.Index == 1 || e.Index == e.Files || time.Since(lastPrep) >= time.Second {
					lastPrep = time.Now()
					if l == i18n.TR {
						logf("okunuyor: %s (%d/%d)", e.Name, e.Index, e.Files)
					} else {
						logf("reading %s (%d/%d)", e.Name, e.Index, e.Files)
					}
				}

			case p2p.PreparedEvent:
				if l == i18n.TR {
					logf("dosyalar hazır")
				} else {
					logf("files ready")
				}

			case p2p.RemotePreparingEvent:
				if time.Since(lastPrep) >= time.Second {
					lastPrep = time.Now()
					if l == i18n.TR {
						logf("gönderen hâlâ dosyalarını hazırlıyor (%d/%d)", e.Done, e.Total)
					} else {
						logf("the sender is still reading its files (%d/%d)", e.Done, e.Total)
					}
				}

			case p2p.RejectedEvent:
				if l == i18n.TR {
					logf("biri yanlış bir kod denedi; hiçbir şey gösterilmedi (%d deneme sonra kod kapanır)", e.Left)
				} else {
					logf("someone tried a wrong code; nothing was shown to them (%d more closes the code)", e.Left)
				}

			case p2p.ServerLostEvent:
				if l == i18n.TR {
					logf("buluşma noktasıyla bağlantı kesildi, yeniden bağlanılıyor; kod değişmedi")
				} else {
					logf("lost the meeting point, reconnecting; the code stays the same")
				}

			case p2p.ServerBackEvent:
				if l == i18n.TR {
					logf("yeniden bağlandı, kod tekrar geçerli")
				} else {
					logf("reconnected, the code works again")
				}

			case p2p.RoomLostEvent:
				return e.Err

			case p2p.ManifestEvent:
				ok := confirm != nil && confirm(e.Manifest)
				e.Reply <- ok
				if !ok {
					return transfer.ErrTransferDeclined
				}

			case p2p.ProgressEvent:
				// One line per second is plenty for a log file.
				if time.Since(lastPrinted) < time.Second && e.OverallDone < e.OverallTotal {
					continue
				}
				lastPrinted = time.Now()
				logf("%s  %s / %s", e.Name,
					formatBytes(e.OverallDone), formatBytes(e.OverallTotal))

			case p2p.DoneEvent:
				if e.Err != nil && host {
					if l == i18n.TR {
						logf("deneme başarısız: %v", e.Err)
						logf("hâlâ bekleniyor, aynı kod tekrar çalışır")
					} else {
						logf("attempt failed: %v", e.Err)
						logf("still waiting, the same code works again")
					}
					continue
				}
				if e.Err != nil {
					return e.Err
				}
				for _, p := range e.Paths {
					if l == i18n.TR {
						logf("kaydedildi: %s", p)
					} else {
						logf("saved %s", p)
					}
				}
				if l == i18n.TR {
					logf("tamamlandı")
				} else {
					logf("done")
				}
				return nil
			}
		}
	}
}

// describeConnection says how the two peers are connected. Only the
// receiver learns the relay's limit, from its lookup; the sender's event
// carries none, and "limit 0 B" would read as a relay that carries nothing.
func describeConnection(e p2p.ConnectedEvent, langs ...i18n.Lang) string {
	l := i18n.EN
	if len(langs) > 0 {
		l = langs[0]
	}
	if l == i18n.TR {
		switch {
		case e.Direct:
			return "doğrudan bağlandı"
		case e.RelayLimit > 0:
			return fmt.Sprintf("yedek aktarıcı üzerinden bağlandı (bağlantı başına sınır %s)", formatBytes(e.RelayLimit))
		default:
			return "yedek aktarıcı üzerinden bağlandı"
		}
	}
	switch {
	case e.Direct:
		return "connected directly"
	case e.RelayLimit > 0:
		return fmt.Sprintf("connected through the fallback relay (limit %s per connection)", formatBytes(e.RelayLimit))
	default:
		return "connected through the fallback relay"
	}
}

// signalContext returns a context cancelled by Ctrl+C or SIGTERM, so an
// interrupted transfer shuts the node down instead of being killed
// mid-write.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func askYesNo(l i18n.Lang) bool {
	if l == i18n.TR {
		fmt.Fprint(os.Stderr, "kabul ediyor musun? [e/H] ")
	} else {
		fmt.Fprint(os.Stderr, "accept? [y/N] ")
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "e", "evet":
		return true
	}
	return false
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
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

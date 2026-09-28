// Client: the app people download and run.
//
// It normally takes no arguments at all — the meeting point's address is
// baked in at build time so a user can double-click the binary:
//
//	go build -ldflags "-X main.defaultServer=/dns4/rendezvous.example.com/tcp/443/tls/ws/p2p/12D3Koo..." ./cmd/client
//
// For local testing, point it somewhere else:
//
//	go run ./cmd/client -server /ip4/127.0.0.1/tcp/4001/p2p/12D3Koo...
//
// Releases also bake in the address of a server list (see
// internal/p2p/serverlist.go): if the meeting point ever moves, copies
// already downloaded find it there instead of dying with the old address.
//
// There is also a headless mode for scripts, servers without a terminal,
// and the end-to-end tests:
//
//	puresend -send holiday.zip        # prints a room code
//	puresend -receive kiraz-liman-42  # downloads into -out
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"puresend/internal/headless"
	"puresend/internal/i18n"
	"puresend/internal/p2p"
	"puresend/internal/rendezvous"
	"puresend/internal/transfer"
	"puresend/internal/tui"
	"puresend/internal/update"

	"github.com/charmbracelet/x/term"
)

// Standard exit codes for PureSend CLI.
const (
	ExitCodeSuccess  = 0
	ExitCodeGeneral  = 1
	ExitCodeUsage    = 2   // Invalid flags, unexpected arguments, file not found locally, bad room code format
	ExitCodeNetwork  = 3   // Rendezvous unreachable, peer unreachable, connection dropped
	ExitCodeAuth     = 4   // Room code does not match, too many wrong attempts, code expired, room not found
	ExitCodeIO       = 5   // Disk full, permission denied, checksum mismatch
	ExitCodeCanceled = 6   // Transfer declined by user
	ExitCodeSignal   = 130 // Interrupted by SIGINT/Ctrl+C
)

// Build information, filled in at build time with -ldflags -X. Releases
// ship with all of it set; a plain `go build` leaves defaultServer empty
// and the -server flag becomes required.
var (
	defaultServer     = ""
	defaultServerList = ""
	version           = "dev"
	commit            = "unknown"
	date              = "unknown"
)

func main() {
	var (
		server = flag.String("server", envOr("FT_SERVER", defaultServer),
			"comma-separated multiaddrs of the meeting point, tried in order")
		serverList = flag.String("server-list", envOr("FT_SERVER_LIST", defaultServerList),
			"URL of a list of meeting point addresses, used when none of -server answers")
		out = flag.String("out", os.Getenv("FT_OUT"),
			"folder to save incoming files in (default: your downloads folder)")
		send        = flag.String("send", "", "headless: send these files or folders (comma-separated) and print a room code")
		receive     = flag.String("receive", "", "headless: download the given room code and exit")
		yes         = flag.Bool("yes", false, "headless: accept the incoming file list without asking")
		showVersion = flag.Bool("version", false, "print version information and exit")
		doUpdate    = flag.Bool("update", false, "check for updates and update puresend to the latest release")
		stun        = flag.String("stun", envOr("FT_STUN", ""),
			"comma-separated STUN servers used to discover WAN IP (default: Cloudflare and Google)")
		flagLang = flag.String("lang", envOr("FT_LANG", ""),
			"display language: tr, en, or auto (default: auto-detected OS language)")
	)
	flag.Parse()

	userLang := *flagLang
	if userLang == "" || userLang == "auto" {
		userLang = string(i18n.DetectOS())
	}

	if *doUpdate {
		lang := i18n.Normalize(userLang)
		if err := update.Apply(version, lang, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", i18n.Get(lang).UpdateFailed, err)
			os.Exit(ExitCodeGeneral)
		}
		return
	}

	if *showVersion {
		fmt.Printf("puresend %s\n", version)
		fmt.Printf("  commit:  %s\n", commit)
		fmt.Printf("  built:   %s\n", date)
		fmt.Printf("  go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		if defaultServer != "" {
			fmt.Printf("  server:  %s\n", defaultServer)
		}
		if defaultServerList != "" {
			fmt.Printf("  list:    %s\n", defaultServerList)
		}
		return
	}

	// Detect unexpected positional arguments early to help users who forget flag dashes.
	if flag.NArg() > 0 {
		args := flag.Args()
		if i18n.Normalize(userLang) == i18n.EN {
			fmt.Fprintf(os.Stderr, "Error: Unexpected arguments: %s\n", strings.Join(args, " "))
			fmt.Fprintln(os.Stderr, "Flags must begin with a dash (-). For example: puresend -send <file> or puresend -receive <code>")
		} else {
			fmt.Fprintf(os.Stderr, "Hata: Beklenmeyen argümanlar algılandı: %s\n", strings.Join(args, " "))
			fmt.Fprintln(os.Stderr, "Bayraklar tire (-) ile başlamalıdır. Örneğin: puresend -send <dosya> veya puresend -receive <kod>")
		}
		os.Exit(ExitCodeUsage)
	}

	if *send != "" && *receive != "" {
		if i18n.Normalize(userLang) == i18n.EN {
			fmt.Fprintln(os.Stderr, "Error: -send and -receive cannot be used together.")
		} else {
			fmt.Fprintln(os.Stderr, "Hata: -send ve -receive aynı anda kullanılamaz.")
		}
		os.Exit(ExitCodeUsage)
	}

	if *send != "" && *yes {
		if i18n.Normalize(userLang) == i18n.EN {
			fmt.Fprintln(os.Stderr, "Error: -yes is only valid with -receive.")
		} else {
			fmt.Fprintln(os.Stderr, "Hata: -yes yalnızca -receive ile birlikte kullanılabilir.")
		}
		os.Exit(ExitCodeUsage)
	}

	if *send != "" && *out != "" {
		if i18n.Normalize(userLang) == i18n.EN {
			fmt.Fprintln(os.Stderr, "Error: -out is used for downloading files (-receive), not sending.")
		} else {
			fmt.Fprintln(os.Stderr, "Hata: -out yalnızca dosya indirme (-receive) için kullanılır, gönderim için değil.")
		}
		os.Exit(ExitCodeUsage)
	}

	servers := p2p.SplitServers(*server)
	if len(servers) == 0 && *serverList == "" {
		if i18n.Normalize(userLang) == i18n.EN {
			fmt.Fprintln(os.Stderr, "Meeting point server address is not configured.")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "This binary was compiled without an embedded server address.")
			fmt.Fprintln(os.Stderr, "You can specify an address manually:")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "  puresend -server /dns4/<domain>/tcp/443/tls/ws/p2p/<PeerID>")
		} else {
			fmt.Fprintln(os.Stderr, "Buluşma noktası adresi ayarlanmamış.")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "Bu ikili, sunucu adresi gömülmeden derlenmiş.")
			fmt.Fprintln(os.Stderr, "Adresi elle vererek çalıştırabilirsin:")
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "  puresend -server /dns4/<alan-adi>/tcp/443/tls/ws/p2p/<PeerID>")
		}
		os.Exit(ExitCodeUsage)
	}

	outDir := *out
	if outDir == "" {
		outDir = tui.DefaultOutDir()
	}

	var stunList []string
	if *stun != "" {
		stunList = splitList(*stun)
	}
	list := p2p.WithServerList(*serverList)
	stunOpt := p2p.WithSTUNServers(stunList)

	switch {
	case *send != "":
		run(headless.Send(servers, splitList(*send), userLang, list, stunOpt), userLang)
	case *receive != "":
		run(headless.Receive(servers, *receive, outDir, *yes, userLang, list, stunOpt), userLang)
	default:
		maybeSpawnTerminal()
		run(tui.Run(tui.Config{
			Servers:     servers,
			ServerList:  *serverList,
			STUNServers: stunList,
			OutDir:      *out,
			Version:     version,
			Lang:        userLang,
		}), userLang)
	}
}

// maybeSpawnTerminal launches a terminal window if the user double-clicked
// the binary from a graphical desktop (Linux Mint Nemo, Ubuntu Nautilus, etc.).
func maybeSpawnTerminal() {
	if runtime.GOOS != "linux" {
		return
	}
	if os.Getenv("FT_IN_TERMINAL") != "" {
		return
	}
	// If DISPLAY and WAYLAND_DISPLAY are empty, we are on a purely headless server without desktop.
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return
	}
	// If already running in a terminal emulator, do nothing.
	if term.IsTerminal(os.Stdin.Fd()) || term.IsTerminal(os.Stdout.Fd()) {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	// Supported terminal emulators on Linux desktop environments
	type termChoice struct {
		bin  string
		args []string
	}
	choices := []termChoice{
		{"x-terminal-emulator", []string{"-e"}},
		{"gnome-terminal", []string{"--"}},
		{"mate-terminal", []string{"-e"}},
		{"xfce4-terminal", []string{"-e"}},
		{"konsole", []string{"-e"}},
		{"alacritty", []string{"-e"}},
		{"kitty", nil},
		{"xterm", []string{"-e"}},
	}

	for _, c := range choices {
		path, err := exec.LookPath(c.bin)
		if err != nil {
			continue
		}
		var cmdArgs []string
		cmdArgs = append(cmdArgs, c.args...)
		cmdArgs = append(cmdArgs, exe)
		cmdArgs = append(cmdArgs, os.Args[1:]...)
		cmd := exec.CommandContext(context.Background(), path, cmdArgs...)
		cmd.Env = append(os.Environ(), "FT_IN_TERMINAL=1")
		if err := cmd.Start(); err == nil {
			os.Exit(0)
		}
	}
}

func run(err error, lang string) {
	if err != nil {
		norm := i18n.Normalize(lang)
		headline, hints := i18n.Explain(err, norm)

		if norm == i18n.EN {
			fmt.Fprintf(os.Stderr, "Error: %s\n", headline)
			if len(hints) > 0 {
				fmt.Fprintln(os.Stderr, "\nWhat you can do:")
				for _, h := range hints {
					fmt.Fprintf(os.Stderr, "  • %s\n", h)
				}
			}
		} else {
			fmt.Fprintf(os.Stderr, "Hata: %s\n", headline)
			if len(hints) > 0 {
				fmt.Fprintln(os.Stderr, "\nNe yapabilirsin:")
				for _, h := range hints {
					fmt.Fprintf(os.Stderr, "  • %s\n", h)
				}
			}
		}
		os.Exit(determineExitCode(err))
	}
}

func determineExitCode(err error) int {
	if err == nil {
		return ExitCodeSuccess
	}
	if errors.Is(err, context.Canceled) {
		return ExitCodeSignal
	}
	if errors.Is(err, transfer.ErrTransferDeclined) {
		return ExitCodeCanceled
	}
	if errors.Is(err, transfer.ErrChecksumMismatch) ||
		errors.Is(err, transfer.ErrInsufficientDiskSpace) ||
		errors.Is(err, os.ErrPermission) {
		return ExitCodeIO
	}
	if errors.Is(err, transfer.ErrWrongCode) ||
		errors.Is(err, p2p.ErrTooManyWrongCodes) ||
		errors.Is(err, p2p.ErrRoomExpired) ||
		errors.Is(err, rendezvous.ErrRoomNotFound) ||
		errors.Is(err, rendezvous.ErrInUse) {
		return ExitCodeAuth
	}
	if errors.Is(err, p2p.ErrRendezvousUnreachable) ||
		errors.Is(err, p2p.ErrPeerUnreachable) ||
		errors.Is(err, transfer.ErrConnectionLost) {
		return ExitCodeNetwork
	}
	var ce *p2p.CodeError
	var se transfer.SourceError
	if errors.As(err, &ce) || errors.As(err, &se) ||
		errors.Is(err, transfer.ErrUnsafeDestination) ||
		errors.Is(err, os.ErrNotExist) {
		return ExitCodeUsage
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "not a room code") || strings.Contains(s, "closed after too many") ||
		strings.Contains(s, "expired") || strings.Contains(s, "room code"):
		return ExitCodeAuth
	case strings.Contains(s, "could not reach") || strings.Contains(s, "dial") ||
		strings.Contains(s, "connection"):
		return ExitCodeNetwork
	case strings.Contains(s, "no space left") || strings.Contains(s, "checksum") ||
		strings.Contains(s, "permission"):
		return ExitCodeIO
	case strings.Contains(s, "declined"):
		return ExitCodeCanceled
	case strings.Contains(s, "canceled") || strings.Contains(s, "interrupt"):
		return ExitCodeSignal
	default:
		return ExitCodeGeneral
	}
}

// splitList parses a comma-separated list of paths.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

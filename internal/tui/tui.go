// Package tui is the terminal interface a non-technical person uses.
//
// The guiding rule: never show a technical term. The user is walked
// through one question at a time, in plain Turkish, and is told what is
// happening and what to do next on every screen. Words like "multiaddr",
// "peer", "NAT" or "relay" never reach the screen.
package tui

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"puresend/internal/i18n"
	"puresend/internal/p2p"
	"puresend/internal/transfer"
	"puresend/internal/update"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type screen int

const (
	screenWelcome screen = iota
	screenConnecting
	screenPickFiles
	screenWaiting
	screenEnterCode
	screenFinding
	screenConfirm
	screenTransfer
	screenDone
	screenError
	screenOutDir
)

// mode is what the client was asked to do on the welcome screen.
type mode int

const (
	modeNone mode = iota
	modeSend
	modeReceive
)

type pickedFile struct {
	path  string
	name  string
	size  int64
	isDir bool
	files int
}

// Config starts the interface with baked-in defaults.
type Config struct {
	// Servers are the meeting point addresses, tried in order.
	Servers []string
	// ServerList is where to look for more addresses when none of
	// Servers answers. Empty disables the fallback.
	ServerList string
	// OutDir overrides where incoming files are saved. Empty picks the
	// friendliest default.
	OutDir string
	// Version is the current version of the application.
	Version string
	// STUNServers overrides the default STUN servers used to discover WAN IP.
	STUNServers []string
	// Lang sets the initial interface language ("tr" or "en"). If empty, defaults to Turkish for compatibility or FT_LANG.
	Lang string
}

// Model is the whole application state.
type Model struct {
	servers     []string
	serverList  string
	stunServers []string
	version     string
	lang        i18n.Lang
	screen      screen
	mode        mode
	width       int
	height      int
	st          styles

	ctx    context.Context
	cancel context.CancelFunc
	node   *p2p.Node

	// welcome / confirm menus
	menuIndex    int
	confirmIndex int

	// sending
	picker     filepicker.Model
	picked     []pickedFile
	room       string
	prepared   bool // every file has been read and the offer is ready
	serverLost bool // the meeting point dropped; the room is being put back
	retrying   bool // an attempt broke off; the room waits for another
	wrongLeft  int  // wrong codes the room still survives; 0 until one is tried

	// updateTag is the newer release on offer, if any. Notices are kept as
	// facts like this and put into words when drawn, so they follow the
	// language the user switches to.
	updateTag string

	// receiving
	codeErr     string // what is wrong with the code as typed
	remoteDone  int    // the sender is still reading its files: done of total
	remoteTotal int

	codeInput textinput.Model
	outDir    string

	// choosing the download folder
	dirPicker  filepicker.Model
	outDirErr  string
	backScreen screen

	// transfer state
	direct     bool
	haveConn   bool
	relayLimit int64
	prep       string
	prepIndex  int
	prepFiles  int
	prog       transfer.Progress
	meter      rateMeter
	manifest   transfer.Manifest
	reply      chan<- bool
	savedPaths []string

	status  string
	err     error
	frame   int
	quitted bool
}

// New builds the initial model.
func New(cfg Config) Model {
	home, _ := os.UserHomeDir()

	cleanKeyMap := filepicker.KeyMap{
		Down:     key.NewBinding(key.WithKeys("down")),
		Up:       key.NewBinding(key.WithKeys("up")),
		PageUp:   key.NewBinding(key.WithKeys("pgup")),
		PageDown: key.NewBinding(key.WithKeys("pgdown")),
		Back:     key.NewBinding(key.WithKeys("backspace", "left")),
		Open:     key.NewBinding(key.WithKeys("enter")),
		Select:   key.NewBinding(key.WithKeys("enter")),
	}

	fp := filepicker.New()
	fp.CurrentDirectory = home
	fp.DirAllowed = false // Enter walks into a folder; "f" sends the whole thing
	fp.FileAllowed = true
	fp.ShowPermissions = false
	fp.SetHeight(10)
	fp.KeyMap = cleanKeyMap

	dp := filepicker.New()
	dp.CurrentDirectory = home
	dp.DirAllowed = false // Enter walks in; "s" picks where we already are
	dp.FileAllowed = false
	dp.ShowPermissions = false
	dp.ShowSize = false
	dp.SetHeight(10)
	dp.KeyMap = cleanKeyMap

	l := i18n.Normalize(cfg.Lang)
	if cfg.Lang == "" {
		if env := os.Getenv("FT_LANG"); env != "" {
			l = i18n.Normalize(env)
		} else {
			l = i18n.TR
		}
	}

	ti := textinput.New()
	ti.Placeholder = i18n.Get(l).EnterPlaceholder
	ti.CharLimit = 64
	ti.Prompt = "  ➜  "
	ti.SetStyles(inputStyles(true))

	outDir := cfg.OutDir
	if outDir == "" {
		outDir = DefaultOutDir()
	}

	ctx, cancel := context.WithCancel(context.Background())

	return Model{
		servers:     cfg.Servers,
		serverList:  cfg.ServerList,
		stunServers: cfg.STUNServers,
		version:     cfg.Version,
		lang:        l,
		screen:      screenWelcome,
		st:          newStyles(true),
		ctx:         ctx,
		cancel:      cancel,
		picker:      fp,
		dirPicker:   dp,
		codeInput:   ti,
		outDir:      outDir,
	}
}

// Run starts the interface and blocks until the user quits.
func Run(cfg Config) error {
	// The terminal belongs to the interface while it runs. Anything a
	// library logs — quic-go warns about UDP buffer sizes, for one — would
	// land on top of the screen, and the renderer, which only redraws the
	// cells it changed, would never paint over it.
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	m := New(cfg)
	p := tea.NewProgram(m)
	final, err := p.Run()
	m.cancel()
	if fm, ok := final.(Model); ok && fm.node != nil {
		_ = fm.node.Close()
	}
	return err
}

// ---------------------------------------------------------------------------
// Messages and commands
// ---------------------------------------------------------------------------

type nodeReadyMsg struct{ node *p2p.Node }
type roomReadyMsg struct{ room string }
type eventMsg struct{ ev p2p.Event }
type errMsg struct{ err error }
type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// connectCmd starts the network layer and reaches the meeting point.
func connectCmd(ctx context.Context, servers []string, serverList string, stunServers []string) tea.Cmd {
	return func() tea.Msg {
		node, err := p2p.New(ctx, servers, p2p.WithServerList(serverList), p2p.WithSTUNServers(stunServers))
		if err != nil {
			return errMsg{err}
		}
		return nodeReadyMsg{node}
	}
}

// hostCmd claims a room code and starts waiting for the receiver.
func hostCmd(ctx context.Context, node *p2p.Node, files []pickedFile) tea.Cmd {
	return func() tea.Msg {
		paths := make([]string, len(files))
		for i, f := range files {
			paths[i] = f.path
		}
		room, err := node.Host(ctx, paths)
		if err != nil {
			return errMsg{err}
		}
		return roomReadyMsg{room}
	}
}

// fetchCmd downloads from a room; its progress arrives as events.
func fetchCmd(ctx context.Context, node *p2p.Node, room, outDir string) tea.Cmd {
	return func() tea.Msg {
		go node.Fetch(ctx, room, outDir)
		return nil
	}
}

// waitEvent pulls the next event off the node's channel.
//
// Exactly one of these may be in flight at a time: it is armed once when
// the node comes up and re-armed by handleEvent after every event. Two
// readers on one channel would each take a different event and hand them
// to the update loop in whatever order they happened to win the race —
// enough to make the progress bar jump backwards, or to let the final
// "done" overtake a straggling progress event and leave the user staring
// at a transfer screen that never finishes.
func waitEvent(node *p2p.Node) tea.Cmd {
	if node == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case ev := <-node.Events():
			return eventMsg{ev}
		case <-node.Done():
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// Bubble Tea plumbing
// ---------------------------------------------------------------------------

type updateAvailableMsg struct{ tag string }

func checkUpdateCmd(version string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		info, err := update.CheckLatest(ctx, version)
		if err != nil || info == nil || !info.HasUpdate {
			return nil
		}
		return updateAvailableMsg{tag: info.TagName}
	}
}

func (m Model) Init() tea.Cmd {
	// The terminal's background decides between the light and dark
	// colours; the answer arrives as a BackgroundColorMsg.
	return tea.Batch(tea.RequestBackgroundColor, m.picker.Init(), tick(), checkUpdateCmd(m.version))
}

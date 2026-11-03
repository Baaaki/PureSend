package tui

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"puresend/internal/i18n"
	"puresend/internal/p2p"
	"puresend/internal/transfer"

	tea "charm.land/bubbletea/v2"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case updateAvailableMsg:
		m.updateTag = msg.tag
		return m, nil

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.picker.SetHeight(max(msg.Height-16, 5))
		m.dirPicker.SetHeight(max(msg.Height-16, 5))
		return m, nil

	case tickMsg:
		m.frame++
		return m, tick()

	case tea.BackgroundColorMsg:
		dark := msg.IsDark()
		m.st = newStyles(dark)
		m.codeInput.SetStyles(inputStyles(dark))
		return m, nil

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m.quit()
		}
		return m.handleKey(msg)

	case tea.PasteMsg:
		// A paste arrives as one message rather than as keys, and pasting
		// is how most codes get typed.
		if m.screen != screenEnterCode {
			return m, nil
		}
		var cmd tea.Cmd
		m.codeInput, cmd = m.codeInput.Update(msg)
		m.codeErr = ""
		return m, cmd

	case nodeReadyMsg:
		if m.screen == screenWelcome {
			if msg.node != nil {
				_ = msg.node.Close()
			}
			return m, nil
		}
		if m.node != nil {
			_ = m.node.Close()
		}
		m.node = msg.node
		next, cmd := m.beginMode()
		// The one place the event reader is armed; handleEvent keeps it
		// going from here on.
		return next, tea.Batch(cmd, waitEvent(msg.node))

	case roomReadyMsg:
		m.room = msg.room
		m.screen = screenWaiting
		return m, nil

	case eventMsg:
		return m.handleEvent(msg.ev)

	case errMsg:
		m.err = msg.err
		m.screen = screenError
		return m, nil
	}

	// Anything else goes to the active component.
	switch m.screen {
	case screenPickFiles:
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		return m, cmd
	case screenOutDir:
		var cmd tea.Cmd
		m.dirPicker, cmd = m.dirPicker.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleEvent folds a p2p event into the model.
func (m Model) handleEvent(ev p2p.Event) (tea.Model, tea.Cmd) {
	if m.node == nil {
		return m, nil
	}
	switch e := ev.(type) {

	case p2p.StatusEvent:
		m.status = e.Text
		return m, waitEvent(m.node)

	case p2p.ConnectedEvent:
		m.direct = e.Direct
		if e.Direct {
			m.relayLimit = 0
		} else {
			m.relayLimit = e.RelayLimit
		}
		m.haveConn = true
		m.retrying = false
		if m.screen != screenConfirm && m.screen != screenDone {
			m.screen = screenTransfer
		}
		return m, waitEvent(m.node)

	case p2p.PreparingEvent:
		m.prep, m.prepIndex, m.prepFiles = e.Name, e.Index, e.Files
		// A sender reads its files in the background while the code is on
		// screen; that must not take the code away.
		if m.mode != modeSend || m.haveConn {
			m.screen = screenTransfer
		}
		return m, waitEvent(m.node)

	case p2p.PreparedEvent:
		m.prepared = true
		m.prep = ""
		return m, waitEvent(m.node)

	case p2p.RemotePreparingEvent:
		m.remoteDone, m.remoteTotal = max(e.Done, 0), max(e.Total, 0)
		return m, waitEvent(m.node)

	case p2p.RejectedEvent:
		m.wrongLeft = e.Left
		return m, waitEvent(m.node)

	case p2p.ServerLostEvent:
		m.serverLost = true
		return m, waitEvent(m.node)

	case p2p.ServerBackEvent:
		m.serverLost = false
		return m, waitEvent(m.node)

	case p2p.RoomLostEvent:
		m.err = e.Err
		m.screen = screenError
		return m, waitEvent(m.node)

	case p2p.ManifestEvent:
		m.manifest = e.Manifest
		m.reply = e.Reply
		m.confirmIndex = 0
		m.screen = screenConfirm
		return m, waitEvent(m.node)

	case p2p.ProgressEvent:
		if m.prog.OverallTotal == 0 {
			m.meter.reset(e.OverallDone)
		}
		m.prog = e.Progress
		m.prep = ""
		m.meter.observe(e.OverallDone)
		if m.screen != screenTransfer {
			m.screen = screenTransfer
		}
		return m, waitEvent(m.node)

	case p2p.DoneEvent:
		if e.Err != nil {
			// While hosting, a failed attempt is not fatal: the room is
			// still registered, so go back to waiting and let the
			// receiver try the same code again.
			if m.mode == modeSend && m.room != "" {
				m.retrying = true
				m.haveConn = false
				m.prog = transfer.Progress{}
				m.meter = rateMeter{}
				m.prep = ""
				m.screen = screenWaiting
				return m, waitEvent(m.node)
			}
			m.err = e.Err
			m.screen = screenError
			return m, nil
		}
		m.savedPaths = e.Paths
		m.screen = screenDone
		return m, nil
	}
	return m, waitEvent(m.node)
}

// handleKey routes a keypress to the active screen.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if (key == "l" || key == "L") && m.screen.switchesLanguage() {
		m.lang = i18n.Toggle(m.lang)
		m.codeInput.Placeholder = i18n.Get(m.lang).EnterPlaceholder
		return m, nil
	}

	switch m.screen {
	case screenWelcome:
		return m.welcomeKey(key)
	case screenPickFiles:
		return m.pickFilesKey(msg)
	case screenOutDir:
		return m.outDirKey(msg)
	case screenEnterCode:
		return m.enterCodeKey(msg)
	case screenConfirm:
		return m.confirmKey(key)
	case screenDone, screenError:
		return m.finishedKey(key)
	}
	// The rest only show progress; Ctrl+C, handled in Update, is the one
	// key they answer to.
	return m, nil
}

// switchesLanguage reports whether [L] changes the language on s. On the
// code entry screen it is a letter of the code, and the screens that only
// show progress take no keys at all.
func (s screen) switchesLanguage() bool {
	switch s {
	case screenWelcome, screenPickFiles, screenOutDir, screenWaiting,
		screenConfirm, screenDone, screenError:
		return true
	}
	return false
}

func (m Model) welcomeKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up":
		m.menuIndex = max(m.menuIndex-1, 0)
	case "down":
		m.menuIndex = min(m.menuIndex+1, 2)
	case "enter":
		switch m.menuIndex {
		case 0:
			m.mode = modeSend
		case 1:
			m.mode = modeReceive
		default:
			// Changing the download folder needs no network at all.
			return m.openOutDir(screenWelcome)
		}
		m.screen = screenConnecting
		return m, connectCmd(m.ctx, m.servers, m.serverList, m.stunServers)
	case "q", "Q":
		return m.quit()
	}
	return m, nil
}

func (m Model) pickFilesKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return m.reset(), nil
	case "s", "S":
		if len(m.picked) > 0 {
			m.screen = screenConnecting
			return m, hostCmd(m.ctx, m.node, m.picked)
		}
	case "f", "F":
		m.addPath(m.picker.CurrentDirectory)
	case "x", "X":
		if len(m.picked) > 0 {
			m.picked = m.picked[:len(m.picked)-1]
		}
	case "up", "down", "pgup", "pgdown", "enter", "backspace", "left":
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		if ok, path := m.picker.DidSelectFile(msg); ok {
			m.addPath(path)
		}
		return m, cmd
	}
	// Any other key is ignored rather than handed to the picker, whose
	// defaults include vim bindings a non-technical user would trip over.
	return m, nil
}

func (m Model) outDirKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "s", "S":
		if err := transfer.CheckDestination(m.dirPicker.CurrentDirectory); err != nil {
			m.outDirErr = i18n.Get(m.lang).OutDirUnsafe
			return m, nil
		}
		m.outDirErr = ""
		m.outDir = m.dirPicker.CurrentDirectory
		m.screen = m.backScreen
	case "esc", "q", "Q":
		m.outDirErr = ""
		m.screen = m.backScreen
	case "backspace", "left":
		m.outDirErr = ""
		parent := filepath.Dir(m.dirPicker.CurrentDirectory)
		if parent != "" && parent != m.dirPicker.CurrentDirectory {
			m.dirPicker.CurrentDirectory = parent
			return m, m.dirPicker.Init()
		}
	case "up", "down", "pgup", "pgdown", "enter":
		m.outDirErr = ""
		var cmd tea.Cmd
		m.dirPicker, cmd = m.dirPicker.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) enterCodeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.codeInput.Reset()
		return m.reset(), nil
	case "enter":
		if strings.TrimSpace(m.codeInput.Value()) == "" {
			return m, nil
		}
		// Check the code here, before the server sees it: a typo caught
		// now costs nothing, one caught by the server costs one of the
		// few tries it allows.
		code, err := p2p.CheckCode(m.codeInput.Value())
		if err != nil {
			m.codeErr = codeProblem(err, m.lang)
			return m, nil
		}
		m.codeInput.SetValue(code)
		m.codeErr = ""
		m.screen = screenFinding
		// Until the node reports its first step, the screen just says it
		// is looking for the friend.
		m.status = ""
		// The event reader is already running; only start the fetch.
		return m, fetchCmd(m.ctx, m.node, code, m.outDir)
	case "ctrl+o":
		return m.openOutDir(screenEnterCode)
	}
	var cmd tea.Cmd
	m.codeInput, cmd = m.codeInput.Update(msg)
	m.codeErr = ""
	return m, cmd
}

func (m Model) confirmKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "left", "right":
		m.confirmIndex = 1 - m.confirmIndex
	case "y", "Y":
		m.confirmIndex = 0
		return m.answerConfirm(true)
	case "n", "N":
		return m.answerConfirm(false)
	case "enter":
		return m.answerConfirm(m.confirmIndex == 0)
	}
	return m, nil
}

// finishedKey handles the done and error screens.
func (m Model) finishedKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "enter":
		return m.reset(), nil
	case "q", "Q":
		return m.quit()
	}
	return m, nil
}

// openOutDir shows the folder picker, returning to back when it closes.
func (m Model) openOutDir(back screen) (tea.Model, tea.Cmd) {
	m.backScreen = back
	m.screen = screenOutDir
	m.outDirErr = ""
	m.dirPicker.CurrentDirectory = m.outDir
	return m, m.dirPicker.Init()
}

func (m Model) quit() (tea.Model, tea.Cmd) {
	m.quitted = true
	m.cancel()
	return m, tea.Quit
}

// beginMode opens the first screen of the chosen mode, once the node is
// connected.
func (m Model) beginMode() (Model, tea.Cmd) {
	switch m.mode {
	case modeSend:
		m.screen = screenPickFiles
		return m, m.picker.Init()
	case modeReceive:
		m.screen = screenEnterCode
		return m, m.codeInput.Focus()
	}
	return m, nil
}

// answerConfirm sends the user's decision back to the transfer.
func (m Model) answerConfirm(ok bool) (tea.Model, tea.Cmd) {
	if m.reply != nil {
		m.reply <- ok
		m.reply = nil
	}
	if !ok {
		// Declining ends the session. Tear it down rather than just
		// walking back to the menu: the refused transfer is about to
		// report itself as failed, and the user who pressed "no" should
		// not then be shown an error about it. Let the refusal frame
		// flush before closing the node.
		if node := m.node; node != nil {
			m.node = nil
			ctx := m.ctx
			go func() {
				defer func() { _ = node.Close() }()
				timer := time.NewTimer(time.Second)
				defer timer.Stop()
				for {
					select {
					case ev := <-node.Events():
						if _, done := ev.(p2p.DoneEvent); done {
							return
						}
					case <-node.Done():
						return
					case <-ctx.Done():
						return
					case <-timer.C:
						return
					}
				}
			}()
		}
		return m.reset(), nil
	}
	m.screen = screenTransfer
	// No waitEvent here: handling the manifest event already re-armed it.
	return m, nil
}

// addPath appends a chosen file or folder, skipping duplicates.
func (m *Model) addPath(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	for _, f := range m.picked {
		if f.path == path {
			return
		}
	}
	pick := pickedFile{path: path, name: info.Name(), size: info.Size()}
	if info.IsDir() {
		if err := transfer.CheckDestination(path); err != nil {
			return
		}
		size, count := folderSize(path)
		if count == 0 {
			return // an empty folder would carry nothing across
		}
		pick.isDir, pick.size, pick.files = true, size, count
	} else if !info.Mode().IsRegular() {
		return
	}
	m.picked = append(m.picked, pick)
}

const (
	maxFolderWalkEntries  = 10_000
	maxFolderWalkDuration = 500 * time.Millisecond
)

// folderSize adds up what sending a folder would actually move. Symbolic
// links are skipped here for the same reason the transfer skips them.
func folderSize(root string) (int64, int) {
	var total int64
	var count, seen int
	deadline := time.Now().Add(maxFolderWalkDuration)
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		seen++
		if seen > maxFolderWalkEntries || time.Now().After(deadline) {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil //nolint:nilerr // an unreadable corner should not stop the count
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
			count++
		}
		return nil
	})
	return total, count
}

// reset ends the session and returns to the main menu. The network layer
// goes down with it and the next transfer builds a fresh one, which
// guarantees no leftover event can bleed into the next transfer.
// Reconnecting costs a second, behind the spinner the user already sees.
func (m Model) reset() Model {
	if m.node != nil {
		_ = m.node.Close()
		m.node = nil
	}
	m.screen = screenWelcome
	m.mode = modeNone
	m.menuIndex = 0
	m.picked = nil
	m.room = ""
	m.prepared = false
	m.serverLost = false
	m.retrying = false
	m.wrongLeft = 0
	m.codeErr = ""
	m.remoteDone, m.remoteTotal = 0, 0
	m.direct = false
	m.haveConn = false
	m.relayLimit = 0
	m.prog = transfer.Progress{}
	m.prep, m.prepIndex, m.prepFiles = "", 0, 0
	m.meter = rateMeter{}
	m.manifest = transfer.Manifest{}
	m.savedPaths = nil
	m.err = nil
	m.status = ""
	m.codeInput.SetValue("")
	return m
}

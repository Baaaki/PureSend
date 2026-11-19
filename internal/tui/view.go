package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"puresend/internal/i18n"
	"puresend/internal/p2p"
	"puresend/internal/transfer"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// View draws the current screen on the terminal's alternate screen, so
// quitting gives the user their terminal back as it was.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

// render is the text of the current screen.
func (m Model) render() string {
	if m.quitted {
		return i18n.Get(m.lang).Goodbye + "\n"
	}
	var body string
	switch m.screen {
	case screenWelcome:
		body = m.viewWelcome()
	case screenConnecting:
		body = m.viewConnecting()
	case screenPickFiles:
		body = m.viewPickFiles()
	case screenWaiting:
		body = m.viewWaiting()
	case screenEnterCode:
		body = m.viewEnterCode()
	case screenFinding:
		body = m.viewFinding()
	case screenConfirm:
		body = m.viewConfirm()
	case screenTransfer:
		body = m.viewTransfer()
	case screenDone:
		body = m.viewDone()
	case screenError:
		body = m.viewError()
	case screenOutDir:
		body = m.viewOutDir()
	}
	return m.st.app.Render(body)
}

func (m Model) spinner() string {
	return m.st.spinner.Render(spinnerFrames[m.frame%len(spinnerFrames)])
}

func (m Model) viewWelcome() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render("📦  PureSend") + "\n\n")
	b.WriteString(m.st.body.Render(t.WelcomeHeadline) + "\n")
	b.WriteString(m.st.help.Render(t.WelcomeHelp1) + "\n")
	b.WriteString(m.st.help.Render(t.WelcomeHelp2) + "\n\n")
	b.WriteString(m.st.body.Render(t.WelcomeQuestion) + "\n\n")

	opts := []string{
		t.WelcomeSend,
		t.WelcomeRecv,
		t.WelcomeChangeDir,
	}
	for i, o := range opts {
		if i == m.menuIndex {
			b.WriteString(m.st.choiceSel.Render("▸ "+o) + "\n")
		} else {
			b.WriteString(m.st.choice.Render(o) + "\n")
		}
	}
	b.WriteString("\n" + m.st.help.Render(t.WelcomeSavingTo) + "\n")
	b.WriteString(m.st.file.Render(m.outDir) + "\n")
	if m.updateTag != "" {
		b.WriteString("\n" + m.st.updateNotice.Render("✨ "+t.UpdateAvailable(m.updateTag)) + "\n")
	}
	b.WriteString("\n" + m.st.formatFooter(t.WelcomeFooter))
	return b.String()
}

func (m Model) viewConnecting() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render(t.ConnectingTitle) + "\n\n")
	b.WriteString(m.spinner() + " " + m.st.body.Render(t.ConnectingStatus) + "\n\n")
	b.WriteString(m.st.help.Render(t.ConnectingHelp1) + "\n")
	b.WriteString(m.st.help.Render(t.ConnectingHelp2) + "\n")
	b.WriteString(m.st.help.Render(t.ConnectingHelp3) + "\n\n")
	b.WriteString(m.st.formatFooter(t.ConnectingFooter))
	return b.String()
}

func (m Model) viewPickFiles() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render(t.PickTitle) + "\n\n")
	b.WriteString(m.st.help.Render(t.PickHelp1) + "\n")
	b.WriteString(m.st.help.Render(t.PickHelp2) + "\n")
	b.WriteString(m.st.help.Render(t.PickHelp3) + "\n\n")
	b.WriteString(m.picker.View() + "\n")

	if len(m.picked) > 0 {
		var total int64
		b.WriteString(m.st.ok.Render(t.PickSelected(len(m.picked))) + "\n")
		for _, f := range m.picked {
			label := "• " + f.name
			detail := formatBytes(f.size)
			if f.isDir {
				label = "• 📁 " + f.name
				detail = t.PickFolderFiles(f.files, formatBytes(f.size))
			}
			b.WriteString(m.st.file.Render(label) + " " + m.st.size.Render("("+detail+")") + "\n")
			total += f.size
		}
		b.WriteString(m.st.size.Render(t.PickTotal(formatBytes(total))) + "\n\n")
		b.WriteString(m.st.buttonSel.Render(t.PickStartBtn) + "\n\n")
		b.WriteString(m.st.formatFooter(t.PickFooterSelected))
	} else {
		b.WriteString("\n" + m.st.help.Render(t.PickEmpty) + "\n\n")
		b.WriteString(m.st.formatFooter(t.PickFooterEmpty))
	}
	return b.String()
}

func (m Model) viewOutDir() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render(t.OutDirTitle) + "\n\n")
	b.WriteString(m.st.help.Render(t.OutDirHelp1) + "\n")
	b.WriteString(m.st.help.Render(t.OutDirHelp2) + "\n")
	b.WriteString(m.st.help.Render(t.OutDirHelp3) + "\n\n")
	b.WriteString(m.st.body.Render(t.OutDirCurrent) + "\n")
	b.WriteString(m.st.file.Render(m.dirPicker.CurrentDirectory) + "\n\n")
	if m.outDirErr != "" {
		b.WriteString(m.st.warn.Render("! "+m.outDirErr) + "\n\n")
	}
	b.WriteString(m.dirPicker.View() + "\n")
	b.WriteString(m.st.buttonSel.Render(t.OutDirSelectBtn(filepath.Base(m.dirPicker.CurrentDirectory))) + "\n\n")
	b.WriteString(m.st.formatFooter(t.OutDirFooter))
	return b.String()
}

func (m Model) viewWaiting() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render(t.WaitingTitle) + "\n\n")
	b.WriteString(m.spinner() + " " + m.st.body.Render(t.WaitingStatus) + "\n\n")
	if m.retrying {
		b.WriteString(m.st.warn.Render("! "+t.HostRetry) + "\n\n")
	}
	b.WriteString(m.st.code.Render(m.room) + "\n\n")

	if len(m.picked) > 0 {
		var total int64
		b.WriteString(m.st.body.Render(t.WaitingFiles(len(m.picked))) + "\n")
		limit := 5
		for i, f := range m.picked {
			if i >= limit {
				remaining := len(m.picked) - limit
				b.WriteString(m.st.help.Render(t.WaitingMoreFiles(remaining)) + "\n")
				break
			}
			label := "• " + f.name
			detail := formatBytes(f.size)
			if f.isDir {
				label = "• 📁 " + f.name
				detail = t.PickFolderFiles(f.files, formatBytes(f.size))
			}
			b.WriteString(m.st.file.Render(label) + " " + m.st.size.Render("("+detail+")") + "\n")
		}
		for _, f := range m.picked {
			total += f.size
		}
		b.WriteString(m.st.size.Render(t.PickTotal(formatBytes(total))) + "\n\n")
	}

	b.WriteString(m.hostingNotes())
	if t.WaitingHelp1 != "" {
		b.WriteString(m.st.help.Render(t.WaitingHelp1) + "\n")
	}
	if t.WaitingHelp2 != "" {
		b.WriteString(m.st.help.Render(t.WaitingHelp2) + "\n\n")
	}
	if t.WaitingHelp3 != "" {
		b.WriteString(m.st.help.Render(t.WaitingHelp3) + "\n\n")
	}
	b.WriteString(m.st.formatFooter(t.WaitingFooter))
	return b.String()
}

// hostingNotes is what the sender needs to know while the code is out:
// how far along reading the files is, whether the meeting point is
// reachable, and whether anyone tried a wrong code.
func (m Model) hostingNotes() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	switch {
	case m.prepared:
		b.WriteString(m.st.ok.Render(t.HostReady) + "\n\n")
	case m.prepFiles > 0:
		b.WriteString(m.spinner() + " " + m.st.help.Render(t.HostPreparing(m.prepIndex, m.prepFiles)) + "\n\n")
	}
	if m.serverLost {
		b.WriteString(m.st.warn.Render(t.HostLost) + "\n")
		b.WriteString(m.st.help.Render(t.HostLostHelp1) + "\n")
		b.WriteString(m.st.help.Render(t.HostLostHelp2) + "\n\n")
	}
	if m.wrongLeft > 0 {
		b.WriteString(m.st.help.Render("• "+t.HostRejected(m.wrongLeft)) + "\n\n")
	}
	return b.String()
}

func (m Model) viewEnterCode() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render(t.EnterTitle) + "\n\n")
	b.WriteString(m.st.help.Render(t.EnterHelp1) + "\n")
	b.WriteString(m.st.help.Render(t.EnterHelp2) + "\n\n")
	b.WriteString(m.codeInput.View() + "\n\n")
	if m.codeErr != "" {
		b.WriteString(m.st.warn.Render("! "+m.codeErr) + "\n\n")
	}
	b.WriteString(m.st.help.Render(t.EnterSavingTo) + "\n")
	b.WriteString(m.st.file.Render(m.outDir) + "\n\n")
	b.WriteString(m.st.formatFooter(t.EnterFooter))
	return b.String()
}

func (m Model) viewFinding() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render(t.FindingTitle) + "\n\n")
	b.WriteString(m.spinner() + " " + m.st.body.Render(m.statusText()) + "\n\n")
	b.WriteString(m.st.help.Render(t.FindingHelp1) + "\n")
	b.WriteString(m.st.help.Render(t.FindingHelp2) + "\n\n")
	b.WriteString(m.st.formatFooter(t.FindingFooter))
	return b.String()
}

// statusText turns the internal status into a friendly sentence.
func (m Model) statusText() string {
	t := i18n.Get(m.lang)
	switch m.status {
	case p2p.StatusLookingUp:
		return t.StatusLookingUp
	case p2p.StatusConnecting:
		return t.StatusConnecting
	case p2p.StatusDirect:
		return t.StatusDirect
	default:
		return t.StatusDefault
	}
}

func (m Model) viewConfirm() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	b.WriteString(m.st.title.Render(t.ConfirmTitle) + "\n\n")

	// A folder of a thousand files would bury the question. A short list
	// is shown whole; a long one is shown as what it would put directly
	// into the download folder — a few folders, usually — since that is
	// where a file could matter, with hidden ones first so that none can
	// sit unseen at the end of a list cut short.
	const maxListed = 12
	files := m.manifest.Files
	entries := topLevel(m.manifest)
	if len(files) <= maxListed {
		for _, f := range files {
			b.WriteString(m.st.file.Render("• "+f.Path) + " " +
				m.st.size.Render("("+formatBytes(f.Size)+")") + "\n")
		}
	} else {
		for i, e := range entries {
			if i == maxListed {
				b.WriteString(m.st.help.Render(t.ConfirmMoreEntries(len(entries)-maxListed)) + "\n")
				break
			}
			if e.dir {
				b.WriteString(m.st.file.Render("• "+e.name+"/") + " " +
					m.st.size.Render("("+t.ConfirmFolder(e.files, formatBytes(e.size))+")") + "\n")
			} else {
				b.WriteString(m.st.file.Render("• "+e.name) + " " +
					m.st.size.Render("("+formatBytes(e.size)+")") + "\n")
			}
		}
	}
	if hidden := hiddenNames(entries, files); hidden != "" {
		b.WriteString("\n" + m.st.warn.Render(t.ConfirmHidden(hidden)) + "\n")
	}

	total := m.manifest.TotalSize()
	b.WriteString("\n" + m.st.size.Render(t.ConfirmTotal(len(files), formatBytes(total))) + "\n\n")

	if warning := m.relayWarning(total); warning != "" {
		b.WriteString(m.st.warn.Render(warning) + "\n\n")
	}

	b.WriteString(m.st.help.Render(t.ConfirmDest) + "\n")
	b.WriteString(m.st.file.Render(m.outDir) + "\n\n")
	b.WriteString(m.st.body.Render(t.ConfirmQuestion) + "\n\n")

	yes, no := m.st.button.Render(t.ConfirmYes), m.st.button.Render(t.ConfirmNo)
	if m.confirmIndex == 0 {
		yes = m.st.buttonSel.Render(t.ConfirmYes)
	} else {
		no = m.st.buttonSel.Render(t.ConfirmNo)
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, yes, no) + "\n\n")
	b.WriteString(m.st.formatFooter(t.ConfirmFooter))
	return b.String()
}

// topEntry is one thing a transfer would create directly in the download
// folder: a file, or a folder with everything under it.
type topEntry struct {
	name  string
	dir   bool
	files int
	size  int64
}

// topLevel groups a manifest by what it would create directly in the
// download folder: hidden entries first, manifest order otherwise.
func topLevel(m transfer.Manifest) []topEntry {
	index := make(map[string]int)
	var out []topEntry
	for _, f := range m.Files {
		// The first real part, the way the receiver will read the path:
		// "./.config/x" lands in ".config".
		var name string
		nested := false
		for part := range strings.SplitSeq(f.Path, "/") {
			if part == "" || part == "." {
				continue
			}
			if name != "" {
				nested = true
				break
			}
			name = part
		}
		i, ok := index[name]
		if !ok {
			i = len(out)
			index[name] = i
			out = append(out, topEntry{name: name})
		}
		out[i].dir = out[i].dir || nested
		out[i].files++
		out[i].size += f.Size
	}
	sort.SliceStable(out, func(a, b int) bool {
		return isHidden(out[a].name) && !isHidden(out[b].name)
	})
	return out
}

func isHidden(name string) bool { return strings.HasPrefix(name, ".") }

// hiddenNames lists hidden entries for the warning, a few at most: both
// top-level hidden files/folders and hidden entries nested inside folders.
func hiddenNames(entries []topEntry, files []transfer.FileInfo) string {
	const most = 5
	var names []string
	seen := make(map[string]bool)
	add := func(label string) bool {
		if seen[label] {
			return false
		}
		if len(names) == most {
			names = append(names, "…")
			return true
		}
		seen[label] = true
		names = append(names, label)
		return false
	}

	for _, e := range entries {
		if !isHidden(e.name) {
			break // they come first
		}
		label := e.name
		if e.dir {
			label += "/"
		}
		if add(label) {
			return strings.Join(names, ", ")
		}
	}
	for _, f := range files {
		var parts []string
		for part := range strings.SplitSeq(f.Path, "/") {
			if part == "" || part == "." {
				continue
			}
			parts = append(parts, part)
		}
		for i := 1; i < len(parts); i++ {
			if isHidden(parts[i]) {
				label := strings.Join(parts[:i+1], "/")
				if i < len(parts)-1 {
					label += "/"
				}
				if add(label) {
					return strings.Join(names, ", ")
				}
				break
			}
		}
	}
	return strings.Join(names, ", ")
}

// relayWarning is the one thing a user cannot work out for themselves: on
// the fallback route there is a hard size limit, and hitting it means the
// transfer breaks off near the end. Better to say so before it starts.
func (m Model) relayWarning(total int64) string {
	if m.direct || !m.haveConn || m.relayLimit <= 0 || total <= m.relayLimit {
		return ""
	}
	t := i18n.Get(m.lang)
	return t.RelayWarning(formatBytes(m.relayLimit))
}

func (m Model) viewTransfer() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	if m.mode == modeSend {
		b.WriteString(m.st.title.Render(t.TransferSendTitle) + "\n\n")
	} else {
		b.WriteString(m.st.title.Render(t.TransferRecvTitle) + "\n\n")
	}

	if m.haveConn {
		if m.direct {
			b.WriteString(m.st.ok.Render(t.TransferDirectOk) + "\n")
			b.WriteString(m.st.help.Render(t.TransferDirectHelp) + "\n\n")
		} else {
			b.WriteString(m.st.warn.Render(t.TransferRelayWarn) + "\n")
			b.WriteString(m.st.help.Render(t.TransferRelayHelp1) + "\n")
			b.WriteString(m.st.help.Render(t.TransferRelayHelp2) + "\n\n")
		}
	}

	switch {
	case m.prog.Total > 0 || m.prog.OverallTotal > 0:
		b.WriteString(m.viewProgress())
	case m.prep != "":
		b.WriteString(m.spinner() + " " + m.st.help.Render(t.TransferPrepFiles(m.prepIndex, m.prepFiles)) + "\n")
		b.WriteString(m.st.file.Render(truncate(m.prep, 46)) + "\n\n")
	case m.mode == modeReceive && m.remoteTotal > 0:
		// A large folder takes the sender a while to read; say so, with
		// numbers, rather than an open-ended "preparing".
		b.WriteString(m.spinner() + " " + m.st.help.Render(t.TransferRemotePrep(
			min(m.remoteDone, m.remoteTotal), m.remoteTotal)) + "\n\n")
	default:
		b.WriteString(m.spinner() + " " + m.st.help.Render(t.TransferPreparing) + "\n\n")
	}
	b.WriteString(m.st.formatFooter(t.TransferFooter))
	return b.String()
}

// viewProgress draws the bar, and under it the two numbers people actually
// watch: how fast it is going and how long is left.
func (m Model) viewProgress() string {
	t := i18n.Get(m.lang)
	p := m.prog
	var b strings.Builder

	if p.Files > 1 {
		b.WriteString(m.st.help.Render(t.TransferFileIndex(p.Index, p.Files)) + "\n")
	}
	b.WriteString(m.st.file.Render(truncate(p.Name, 46)) + "\n")

	done, total := p.OverallDone, p.OverallTotal
	if total == 0 {
		done, total = p.Done, p.Total
	}
	pct := int64(0)
	if total > 0 {
		pct = done * 100 / total
	}
	b.WriteString("  " + m.st.progressBar(done, total, 30) +
		fmt.Sprintf("  %3d%%  ", pct) +
		m.st.size.Render(formatBytes(done)+" / "+formatBytes(total)) + "\n")

	if rate := m.meter.rate(); rate > 0 {
		line := formatRateLang(rate, m.lang)
		if eta, ok := m.meter.eta(total - done); ok {
			line += "  ·  " + t.TransferEta(formatDurationLang(eta, m.lang))
		}
		b.WriteString("  " + m.st.size.Render(line) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

func (m Model) viewDone() string {
	t := i18n.Get(m.lang)
	var b strings.Builder
	if m.mode == modeSend {
		b.WriteString(m.st.ok.Render(t.DoneSendTitle) + "\n\n")
		b.WriteString(m.st.body.Render(t.DoneSendBody) + "\n\n")
		b.WriteString(m.st.help.Render(t.DoneSendHelp) + "\n\n")
	} else {
		b.WriteString(m.st.ok.Render(t.DoneRecvTitle) + "\n\n")
		b.WriteString(m.st.body.Render(t.DoneRecvBody) + "\n\n")
		const maxListed = 12
		for i, p := range m.savedPaths {
			if i == maxListed {
				b.WriteString(m.st.help.Render(t.DoneMoreFiles(len(m.savedPaths)-maxListed)) + "\n")
				break
			}
			b.WriteString(m.st.file.Render("• "+p) + "\n")
		}
		b.WriteString("\n" + m.st.help.Render(t.DoneVerified) + "\n\n")
	}
	b.WriteString(m.st.formatFooter(t.DoneFooter))
	return b.String()
}

func (m Model) viewError() string {
	t := i18n.Get(m.lang)
	headline, hints := i18n.Explain(m.err, m.lang)

	var b strings.Builder
	b.WriteString(m.st.err.Render(t.ErrorTitle) + "\n\n")
	b.WriteString(m.st.body.Render(headline) + "\n\n")
	if len(hints) > 0 {
		b.WriteString(m.st.help.Render(t.ErrorWhatCan) + "\n")
		for _, h := range hints {
			b.WriteString(m.st.help.Render("  • "+h) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(m.st.formatFooter(t.ErrorFooter))
	return b.String()
}

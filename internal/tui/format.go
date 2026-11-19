package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"puresend/internal/i18n"
	"puresend/internal/p2p"
)

// explain turns a technical error into a plain-language headline and a
// short list of things the user can actually do about it.
func explain(err error) (string, []string) {
	return i18n.Explain(err, i18n.TR)
}

// codeProblem says what is wrong with a typed code, in words the user can
// act on without leaving the screen.
func codeProblem(err error, lang i18n.Lang) string {
	var ce *p2p.CodeError
	if errors.As(err, &ce) && ce.Word != "" {
		if lang == i18n.EN {
			return fmt.Sprintf("\"%s\" is not a recognized word in room codes — check the spelling.", ce.Word)
		}
		return fmt.Sprintf("\"%s\" kodlarda geçen bir kelime değil — yazımını kontrol et.", ce.Word)
	}
	if lang == i18n.EN {
		return "Room code consists of two words and a number, e.g. kiraz-liman-42."
	}
	return "Kod iki kelime ve bir sayıdan oluşur, örneğin kiraz-liman-42."
}

// DefaultOutDir picks the friendliest place to save incoming files.
func DefaultOutDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "received"
	}
	for _, name := range []string{"Downloads", "İndirilenler", "Indirilenler"} {
		p := filepath.Join(home, name)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return filepath.Join(p, "PureSend")
		}
	}
	return filepath.Join(home, "PureSend")
}

func truncate(s string, max int) string {
	// Slice by runes, not bytes: cutting a multi-byte character in half
	// (e.g. "fotoğraf.jpg") would print garbage.
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
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

func formatRate(bytesPerSecond float64) string {
	return formatRateLang(bytesPerSecond, i18n.TR)
}

func formatRateLang(bytesPerSecond float64, lang i18n.Lang) string {
	sec := "sn"
	if lang == i18n.EN {
		sec = "s"
	}
	switch {
	case bytesPerSecond >= 1<<20:
		return fmt.Sprintf("%.1f MB/%s", bytesPerSecond/(1<<20), sec)
	case bytesPerSecond >= 1<<10:
		return fmt.Sprintf("%.0f KB/%s", bytesPerSecond/(1<<10), sec)
	default:
		return fmt.Sprintf("%.0f B/%s", bytesPerSecond, sec)
	}
}

// formatDuration rounds hard on purpose: "yaklaşık 3 dakika" is what
// someone wants to know, not "3 dakika 07 saniye".
func formatDuration(d time.Duration) string {
	return formatDurationLang(d, i18n.TR)
}

func formatDurationLang(d time.Duration, lang i18n.Lang) string {
	if lang == i18n.EN {
		switch {
		case d >= time.Hour:
			h := int(d.Hours())
			return fmt.Sprintf("~%d hr %d min", h, int(d.Minutes())-h*60)
		case d >= time.Minute:
			return fmt.Sprintf("~%d min", int(d.Minutes())+1)
		case d >= 10*time.Second:
			return fmt.Sprintf("~%d sec", int(d.Seconds()))
		default:
			return "a few seconds"
		}
	}
	switch {
	case d >= time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("~%d saat %d dakika", h, int(d.Minutes())-h*60)
	case d >= time.Minute:
		return fmt.Sprintf("~%d dakika", int(d.Minutes())+1)
	case d >= 10*time.Second:
		return fmt.Sprintf("~%d saniye", int(d.Seconds()))
	default:
		return "birkaç saniye"
	}
}

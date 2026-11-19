package i18n

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// TestBothLanguagesAreComplete: a message added to one language and
// forgotten in the other is an empty line on screen, or — for the ones
// that are functions — a crash the moment the screen is drawn.
func TestBothLanguagesAreComplete(t *testing.T) {
	tr, en := reflect.ValueOf(*trMessages), reflect.ValueOf(*enMessages)
	for i := range tr.NumField() {
		name := tr.Type().Field(i).Name
		a, b := tr.Field(i), en.Field(i)
		switch a.Kind() {
		case reflect.Func:
			if a.IsNil() || b.IsNil() {
				t.Errorf("%s: missing in TR=%v EN=%v", name, a.IsNil(), b.IsNil())
			}
		case reflect.String:
			// A few help lines are deliberately blank; blank in one
			// language only is the mistake.
			if (a.String() == "") != (b.String() == "") {
				t.Errorf("%s: set in only one language (TR=%q, EN=%q)", name, a.String(), b.String())
			}
		}
	}
}

func TestDetectOS(t *testing.T) {
	origLang := os.Getenv("LANG")
	origLC := os.Getenv("LC_ALL")
	defer func() {
		os.Setenv("LANG", origLang)
		os.Setenv("LC_ALL", origLC)
	}()

	os.Setenv("LC_ALL", "")
	os.Setenv("LANG", "tr_TR.UTF-8")
	if got := DetectOS(); got != TR {
		t.Errorf("DetectOS() for tr_TR = %v, want %v", got, TR)
	}

	os.Setenv("LANG", "en_US.UTF-8")
	if got := DetectOS(); got != EN {
		t.Errorf("DetectOS() for en_US = %v, want %v", got, EN)
	}

	os.Setenv("LANG", "de_DE.UTF-8")
	if got := DetectOS(); got != EN {
		t.Errorf("DetectOS() for de_DE = %v, want %v", got, EN)
	}

	os.Setenv("LANG", "en_TR.UTF-8")
	if got := DetectOS(); got != EN {
		t.Errorf("DetectOS() for en_TR = %v, want %v", got, EN)
	}
}

func TestToggle(t *testing.T) {
	if got := Toggle(TR); got != EN {
		t.Errorf("Toggle(TR) = %v, want EN", got)
	}
	if got := Toggle(EN); got != TR {
		t.Errorf("Toggle(EN) = %v, want TR", got)
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("TR"); got != TR {
		t.Errorf("Normalize(TR) = %v, want TR", got)
	}
	if got := Normalize("turkish"); got != TR {
		t.Errorf("Normalize(turkish) = %v, want TR", got)
	}
	if got := Normalize("en"); got != EN {
		t.Errorf("Normalize(en) = %v, want EN", got)
	}
	if got := Normalize("english"); got != EN {
		t.Errorf("Normalize(english) = %v, want EN", got)
	}
}

func TestExplain(t *testing.T) {
	err := errors.New("room code does not match")
	headTR, hintsTR := Explain(err, TR)
	if headTR != "Kod eşleşmedi." || len(hintsTR) == 0 {
		t.Errorf("Explain(TR) unexpected: %s, %v", headTR, hintsTR)
	}

	headEN, hintsEN := Explain(err, EN)
	if headEN != "Room code does not match." || len(hintsEN) == 0 {
		t.Errorf("Explain(EN) unexpected: %s, %v", headEN, hintsEN)
	}
}

func TestGetCompleteness(t *testing.T) {
	tr := Get(TR)
	en := Get(EN)

	if tr.WelcomeHeadline == "" || en.WelcomeHeadline == "" {
		t.Error("WelcomeHeadline is empty")
	}
	if tr.ConnectingTitle == "" || en.ConnectingTitle == "" {
		t.Error("ConnectingTitle is empty")
	}
	if tr.PickTitle == "" || en.PickTitle == "" {
		t.Error("PickTitle is empty")
	}
	if tr.WaitingTitle == "" || en.WaitingTitle == "" {
		t.Error("WaitingTitle is empty")
	}
	if tr.EnterTitle == "" || en.EnterTitle == "" {
		t.Error("EnterTitle is empty")
	}
	if tr.FindingTitle == "" || en.FindingTitle == "" {
		t.Error("FindingTitle is empty")
	}
	if tr.ConfirmTitle == "" || en.ConfirmTitle == "" {
		t.Error("ConfirmTitle is empty")
	}
	if tr.TransferSendTitle == "" || en.TransferSendTitle == "" {
		t.Error("TransferSendTitle is empty")
	}
	if tr.DoneSendTitle == "" || en.DoneSendTitle == "" {
		t.Error("DoneSendTitle is empty")
	}
	if tr.ErrorTitle == "" || en.ErrorTitle == "" {
		t.Error("ErrorTitle is empty")
	}
}

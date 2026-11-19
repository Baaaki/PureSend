package transfer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/schollz/pake/v3"
)

// The room code is a weak secret. Its number is a public nameplate the
// server finds the room by; only its two words are secret, 65,536
// combinations (16 bits), small enough to guess offline if they ever
// leaked in a form an attacker could test against. PAKE
// (password-authenticated key exchange) turns the code into a strong
// shared key without putting it — or anything derived from it that could
// be attacked offline — on the wire.
//
// What this buys us concretely:
//
//   - The rendezvous server is not a trusted party. It is the server that
//     tells the receiver "the sender is peer X"; if it lied and named one
//     of its own peers instead, the receiver used to connect to the
//     impostor and accept whatever files it offered. Now the impostor has
//     to know the code to complete the handshake. The server only ever
//     sees the nameplate, so it would have to guess the words — one guess
//     per attempt, each failure visible to the person it was tried on.
//   - A passive listener on either link learns nothing it can test codes
//     against.
//   - The exchange binds both peer IDs into the session key, so an attacker
//     cannot sit in the middle and pass the two sides' messages through to
//     each other: the two ends would derive different keys and the
//     confirmation step below would fail.
//
// What it does not buy: protection from someone who guesses the words
// outright. Every guess costs a full handshake with a live sender, and a
// sender closes its room after a few wrong ones (see p2p.Node), so the odds
// are a few in 65,536 per room — but they are not zero.
//
// On the choice of library: schollz/pake is a SPAKE2-style exchange of its
// own design, not RFC 9382 SPAKE2 or CPace, and has had far less review
// than either. It is what croc has used in production for years, and the
// parts that carry the weight here — binding both identities, and the
// mutual confirmation tags below — are ours and do not depend on its
// details. Moving to a standard construction when a maintained Go
// implementation exists is a change to this file and the protocol
// version, nothing else.
const (
	// pakeCurve is the group the exchange runs in. P-256 comes from the
	// standard library, where it is constant-time and well reviewed.
	pakeCurve = "p256"

	// Roles in the underlying protocol. The receiver dials and therefore
	// speaks first, which makes it party A.
	roleReceiver = 0
	roleSender   = 1

	// Domain separators for the two confirmation tags, so a tag from one
	// side can never be replayed as the other's.
	confirmReceiverLabel = "puresend/pake/confirm/receiver/v1"
	confirmSenderLabel   = "puresend/pake/confirm/sender/v1"
)

// Credentials are the three things both sides must already agree on before
// a single file byte moves: the room code, and who the two ends are. The
// sender learns the receiver's peer ID from the incoming stream; the
// receiver learns the sender's from the room lookup. If those two views
// disagree — which is exactly what a lying server or a peer in the middle
// produces — the handshake fails.
type Credentials struct {
	Code     string // the room code, e.g. "kiraz-liman-42"
	Sender   string // peer ID of the side serving the files
	Receiver string // peer ID of the side asking for them
}

func (c Credentials) validate() error {
	switch {
	case c.Code == "":
		return fmt.Errorf("room code is required")
	case c.Sender == "":
		return fmt.Errorf("sender identity is required")
	case c.Receiver == "":
		return fmt.Errorf("receiver identity is required")
	}
	return nil
}

// identities returns the ordered identity strings bound into the session
// key. Party A is the receiver, party B the sender; both sides must build
// this the same way or the keys will not match.
func (c Credentials) identities() (idA, idB []byte) {
	return []byte("puresend/receiver/" + c.Receiver),
		[]byte("puresend/sender/" + c.Sender)
}

// authMsg is one step of the handshake. Exactly one field is set.
type authMsg struct {
	PAKE    []byte `json:"pake,omitempty"`    // a marshalled PAKE public value
	Confirm []byte `json:"confirm,omitempty"` // a key confirmation tag
}

// readMsgFunc and writeMsgFunc hide the fact that the two sides frame
// messages differently: the sender can decode straight off the stream,
// while the receiver has to read bounded lines because raw file bytes
// follow on the same stream.
type (
	readMsgFunc  func(*authMsg) error
	writeMsgFunc func(authMsg) error
)

// judgeFunc is where the sender judges the receiver's proof of the code;
// see SendOptions.Judge.
type judgeFunc func(proves func() bool) bool

// authenticate runs the exchange and returns the session key. Four
// messages: the two PAKE halves, then a confirmation tag from each side.
//
// The confirmation is not optional. The exchange alone leaves both sides
// with *a* key; only exchanging tags over it proves the other side derived
// the same one, which is what makes a wrong code fail loudly instead of
// silently corrupting the transfer that follows.
//
// judge is only consulted on the sender's side; nil judges every proof.
func authenticate(role int, creds Credentials, read readMsgFunc, write writeMsgFunc, judge judgeFunc) ([]byte, error) {
	if err := creds.validate(); err != nil {
		return nil, err
	}
	idA, idB := creds.identities()
	p, err := pake.InitCurveWithIdentities([]byte(creds.Code), role, pakeCurve, idA, idB)
	if err != nil {
		return nil, fmt.Errorf("could not start the security handshake: %w", err)
	}

	if role == roleReceiver {
		if err := write(authMsg{PAKE: p.Bytes()}); err != nil {
			return nil, fmt.Errorf("could not start the security handshake: %w", err)
		}
	}

	var theirs authMsg
	if err := read(&theirs); err != nil {
		return nil, fmt.Errorf("no answer to the security handshake: %w", describe(err))
	}
	if len(theirs.PAKE) == 0 {
		return nil, ErrWrongCode
	}
	if err := validatePAKEBytes(role, theirs.PAKE); err != nil {
		return nil, ErrWrongCode
	}
	if err := safePAKEUpdate(p, theirs.PAKE); err != nil {
		// A malformed or off-curve value means the other side is not
		// running this protocol, or is not who it claims to be.
		return nil, ErrWrongCode
	}

	if role == roleSender {
		if err := write(authMsg{PAKE: p.Bytes()}); err != nil {
			return nil, fmt.Errorf("could not answer the security handshake: %w", err)
		}
	}

	key, err := p.SessionKey()
	if err != nil {
		return nil, ErrWrongCode
	}

	// Mutual confirmation. Each side sends the tag for its own role and
	// checks the other's, in the same order as the exchange above so that
	// neither side is ever writing while the other is also writing.
	ours, expected := confirmSenderLabel, confirmReceiverLabel
	if role == roleReceiver {
		ours, expected = confirmReceiverLabel, confirmSenderLabel
	}
	send := func() error {
		return write(authMsg{Confirm: confirmTag(key, ours)})
	}
	readTag := func() ([]byte, error) {
		var msg authMsg
		if err := read(&msg); err != nil {
			return nil, fmt.Errorf("no confirmation from the other side: %w", describe(err))
		}
		return msg.Confirm, nil
	}
	proves := func(tag []byte) bool {
		return hmac.Equal(tag, confirmTag(key, expected))
	}

	if role == roleReceiver {
		if err := send(); err != nil {
			return nil, err
		}
		tag, err := readTag()
		if err != nil {
			return nil, err
		}
		if !proves(tag) {
			return nil, ErrWrongCode
		}
		return key, nil
	}

	// The sender only shows its own tag to a receiver that has shown the
	// right one first. The tag is a test of the password: a guesser who got
	// it could check its guess against it at leisure. It used to be sent
	// whatever the check said, so a guesser who sent garbage instead of a
	// tag — a failure that is not a wrong code, and not counted as one —
	// learned whether its guess was right without it ever counting against
	// the room.
	//
	// The tag is read before it is judged, and only the judging goes
	// through judge: a guesser that is slow to send its tag holds nothing
	// up but its own handshake.
	//
	// A wrong tag still gets an answer, an empty one. Hanging up instead
	// would leave the receiver reading an empty connection, and it would
	// report a dropped link when the real answer is "that is not the code".
	tag, err := readTag()
	if err != nil {
		return nil, err
	}
	if judge == nil {
		judge = func(proves func() bool) bool { return proves() }
	}
	if !judge(func() bool { return proves(tag) }) {
		_ = write(authMsg{})
		return nil, ErrWrongCode
	}
	if err := send(); err != nil {
		return nil, err
	}
	return key, nil
}

// confirmTag is the proof that a side holds the session key.
func confirmTag(key []byte, label string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// pakeWire mirrors the JSON fields schollz/pake reads during Update so they
// can be checked for nil before entering the library. schollz/pake panics on
// a JSON "null" payload or missing curve coordinates (nil *big.Int passed to
// elliptic.P256().IsOnCurve).
type pakeWire struct {
	Role *int     `json:"Role"`
	Xu   *big.Int `json:"Xᵤ"`
	Xv   *big.Int `json:"Xᵥ"`
	Yu   *big.Int `json:"Yᵤ"`
	Yv   *big.Int `json:"Yᵥ"`
}

func validatePAKEBytes(role int, raw []byte) error {
	var msg *pakeWire
	if err := json.Unmarshal(raw, &msg); err != nil || msg == nil || msg.Role == nil {
		return ErrWrongCode
	}
	if role == roleSender {
		if *msg.Role != roleReceiver || msg.Xu == nil || msg.Xv == nil {
			return ErrWrongCode
		}
		return nil
	}
	if *msg.Role != roleSender || msg.Yu == nil || msg.Yv == nil {
		return ErrWrongCode
	}
	return nil
}

// safePAKEUpdate guards against any unexpected panic inside the third-party
// PAKE library when handling untrusted wire input.
func safePAKEUpdate(p *pake.Pake, raw []byte) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrWrongCode
		}
	}()
	return p.Update(raw)
}

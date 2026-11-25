package supervisor

import (
	"bufio"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrUnconfirmed     = errors.New("supervised processes stopped without confirmation")
	ErrSession         = errors.New("could not maintain the supervision session file")
	ErrReaper          = errors.New("supervision reaper unavailable")
	ErrStopped         = errors.New("supervision is stopping or launch was cancelled")
	ErrLaunch          = errors.New("could not launch supervised process")
	ErrCheckoutChanged = errors.New("approved checkout changed before launch")
	ErrPrepareTimeout  = errors.New("prepared launch was not released in time")
	ErrBusy            = errors.New("run already live or the live run limit was reached")
	errUnreadablePeer  = errors.New("peer process could not be read")
)

const (
	maxFrame        = 1 << 20 // maximum local control/gate frame in bytes, not including the newline
	wireTimeout     = 2 * time.Second
	gracePeriod     = 15 * time.Second
	killWait        = 5 * time.Second
	prepareWait     = 5 * time.Second
	reaperSlack     = 3 * time.Second
	unconfirmed     = 4 // helper exit status, distinct from an unexpected death or protocol error
	codeStopped     = "stopped"
	codeLaunch      = "launch"
	codeCheckout    = "checkout_changed"
	codeTimeout     = "launch_timeout"
	codeBusy        = "busy"
	codeSession     = "session"
	codeRecovery    = "unconfirmed"
	codeAttribute   = "attribute"
	codeReaper      = "reaper"
	bootstrapFailed = 126
)

type Options struct {
	Jobs []FixedJob
	// Nonzero periods can shorten the production limits for fixture tests. The CLI does not expose them.
	Grace, KillWait, PrepareWait time.Duration
}

type periods struct{ Grace, KillWait, PrepareWait time.Duration }
type timing struct{ grace, killWait, prepare time.Duration }

func (p periods) timing() timing {
	limit := func(n, def time.Duration) time.Duration {
		if n <= 0 || n > def {
			return def
		}
		return n
	}
	return timing{limit(p.Grace, gracePeriod), limit(p.KillWait, killWait), limit(p.PrepareWait, prepareWait)}
}

// The protocol is private to a pair of anonymous pipes. One command is outstanding at a time.
// Each reply has Finished == 0; a run completion is unsolicited and carries its unique ticket.
type request struct {
	Op      string
	RunID   string     `json:",omitempty"`
	Ticket  uint64     `json:",omitempty"`
	PID     int        `json:",omitempty"`
	Command *Command   `json:",omitempty"`
	Jobs    []FixedJob `json:",omitempty"`
	Periods *periods   `json:",omitempty"`
	Until   int64      `json:",omitempty"` // absolute unix nanos; release must finish by then
	Receipt string     `json:",omitempty"` // durable permit nonce, only for actual agent.run requests
	Reason  string     `json:",omitempty"` // fixed stop code
}

type message struct {
	Error    string `json:",omitempty"`
	RunID    string `json:",omitempty"`
	PID      int    `json:",omitempty"`
	Finished uint64 `json:",omitempty"`
	Cleaned  bool   `json:",omitempty"` // the tree is confirmed gone, independently of launch outcome
	Exited   uint64 `json:",omitempty"`
	Exit     int    `json:",omitempty"`
	Reason   string `json:",omitempty"`
}

func (m message) err() error {
	switch m.Error {
	case "":
		return nil
	case codeStopped:
		return ErrStopped
	case codeLaunch:
		return ErrLaunch
	case codeCheckout:
		return ErrCheckoutChanged
	case codeTimeout:
		return ErrPrepareTimeout
	case codeBusy:
		return ErrBusy
	case codeSession:
		return ErrSession
	case codeRecovery:
		return ErrUnconfirmed
	default:
		return ErrReaper
	}
}

func wireScanner(f *os.File) *bufio.Scanner {
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), maxFrame+1)
	return s
}

func wireWrite(f *os.File, payload []byte, deadline time.Time) error {
	if err := f.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err := f.Write(append(payload, '\n'))
	return err
}

func inheritedPipe(fd uintptr, name string) (*os.File, error) {
	f := os.NewFile(fd, name)
	if f == nil {
		return nil, ErrReaper
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		f.Close()
		return nil, ErrReaper
	}
	unix.CloseOnExec(int(fd))
	return f, nil
}

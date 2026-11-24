// Package actsocket implements the same-user proposal socket for beacon serve, not the signed Swift socket.
package actsocket

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

const (
	FrameMax     = 256 * 1024 // including the newline
	MaxWait      = 300        // seconds
	frameTimeout = 10 * time.Second
	idleTimeout  = 60 * time.Second
	writeTimeout = 5 * time.Second
)

type limits struct {
	connections, waits, frameBytes int
	pendingPerPeer, perHour, total int
	frame, idle                    time.Duration
}

var socketLimits = limits{32, 16, FrameMax, 20, 60, 40, frameTimeout, idleTimeout}

type Options struct {
	Origin  approval.Origin
	Enabled []approval.Action
	Now     func() time.Time
	// Attribute is looked up once, using the kernel PID. Only the supervisor may supply it; it must
	// return a run id only for a member of a live run. Nil or an empty result means unattributed;
	// a supervisor error rejects the connection rather than widening a run's ownership.
	Attribute func(pid int) (string, error)
	// IssueOpen issues a sign-in code or reports a full code pool. The code cannot authorize an action.
	IssueOpen func() (code string, ok bool)
	// PlaceSlack verifies visibility before the journal commits the proposed record. Nil keeps Slack review-only.
	PlaceSlack func(context.Context, approval.Request, string) (approval.SlackPlacement, error)
	// ResumeDelivery wakes the ordered sender. It does not approve a proposal or dispatch an action.
	ResumeDelivery func()
	// Proposed signals after a successful append. The callback must not block or take the proposal lock.
	Proposed func()
}

type Server struct {
	listener *listener
	journal  *approval.Journal
	opts     Options
	limits   limits
	uid      int
	waits    chan struct{}
	propose  sync.Mutex // counts and the subsequent journal append must be checked as one operation
	recent   map[string][]time.Time
}

// Listen binds run/act.sock beneath the already-locked private journal directory. Nothing is read
// until Serve runs. Close also works if startup fails before Serve.
func Listen(dir string, j *approval.Journal, opts Options) (*Server, error) {
	return listenServer(dir, j, opts, socketLimits)
}

func listenServer(dir string, j *approval.Journal, opts Options, lim limits) (*Server, error) {
	if opts.Origin.Host() == "" {
		return nil, errors.New("no canonical review origin")
	}
	opts.Enabled = slices.Clone(opts.Enabled)
	for i, a := range opts.Enabled {
		if a.ParamsSchema() == nil || slices.Contains(opts.Enabled[:i], a) {
			return nil, errors.New("invalid enabled proposal types")
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	l, err := listen(dir)
	if err != nil {
		return nil, err
	}
	return &Server{listener: l, journal: j, opts: opts, limits: lim, uid: os.Getuid(),
		waits: make(chan struct{}, lim.waits), recent: make(map[string][]time.Time)}, nil
}

func (s *Server) Close() error { return s.listener.Close() }

// Serve runs once and joins every connection before returning. Cancel or close to stop accepting.
func (s *Server) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { s.Close() })
	var peers sync.WaitGroup
	defer func() {
		stop()
		cancel()
		peers.Wait()
	}()
	slots := make(chan struct{}, s.limits.connections)
	for {
		c, err := s.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			peers.Add(1)
			go func() {
				defer peers.Done()
				defer func() { <-slots }()
				s.serveConn(ctx, c)
			}()
		default:
			c.Close()
		}
	}
}

func (s *Server) serveConn(ctx context.Context, c *net.UnixConn) {
	defer c.Close()
	pid, err := peerPID(c, s.uid)
	if err != nil {
		return // no frame read, no reply
	}
	peer := approval.PeerLabel{PID: pid, Executable: executable(pid)}
	if s.opts.Attribute != nil {
		id, err := s.opts.Attribute(pid)
		if err != nil || (id != "" && !approval.ValidID(id, "run")) {
			return
		}
		peer.RunID = id
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	frames := stream{chunks: readChunks(ctx, c), limit: s.limits.frameBytes}
	defer func() {
		stop()
		cancel()
		c.Close()
		for range frames.chunks {
		} // join reader
	}()
	for ctx.Err() == nil {
		b, err := frames.next(ctx, s.limits.idle, s.limits.frame)
		if err != nil {
			if errors.Is(err, errFrameLarge) {
				reply(c, failure("frame_too_large"))
			}
			return
		}
		o, err := approval.ParseObject(b)
		var answer approval.Object
		var closeAfter bool
		switch {
		case err != nil:
			answer = failure("invalid_frame")
		case o["op"] == "wait":
			answer, closeAfter = s.wait(ctx, &frames, peer, o)
		default:
			answer = s.handle(ctx, peer, o)
		}
		if answer == nil || !reply(c, answer) || closeAfter {
			return
		}
	}
}

func reply(c *net.UnixConn, answer approval.Object) bool {
	b, err := marshalFrame(answer, FrameMax)
	if err != nil || c.SetWriteDeadline(time.Now().Add(writeTimeout)) != nil {
		return false
	}
	_, err = c.Write(b)
	return err == nil
}

func failure(code string) approval.Object { return approval.Object{"error": code} }

func invalid(field string) approval.Object {
	if len(field) > 512 {
		field = "" // an unrecognized key can itself be almost a frame
	}
	return approval.Object{"error": "invalid", "field": field}
}

func status(p approval.Proposal) approval.Object {
	return approval.Object{"id": p.ID, "status": p.Status, "version": p.Version}
}

func journalError(err error) approval.Object {
	var f approval.Invalid
	switch {
	case errors.As(err, &f):
		return invalid(string(f))
	case errors.Is(err, approval.ErrNotFound):
		return failure("not_found")
	case errors.Is(err, approval.ErrTransition):
		return failure("not_pending")
	default:
		return failure("unavailable")
	}
}

func (s *Server) handle(ctx context.Context, peer approval.PeerLabel, o approval.Object) approval.Object {
	switch o["op"] {
	case "types":
		if len(o) != 1 {
			return invalid("")
		}
		out := make([]approval.Object, 0, len(s.opts.Enabled))
		for _, a := range s.opts.Enabled {
			out = append(out, approval.Object{"type": a, "params": a.ParamsSchema()})
		}
		return approval.Object{"types": out}
	case "open":
		if len(o) != 1 {
			return invalid("")
		}
		if s.opts.IssueOpen == nil {
			return failure("unavailable")
		}
		code, ok := s.opts.IssueOpen()
		if !ok {
			return failure("busy")
		}
		return approval.Object{"code": code, "url": s.opts.Origin.URL() + "/open?code=" + url.QueryEscape(code)}
	case "resume-delivery":
		if len(o) != 1 {
			return invalid("")
		}
		if peer.RunID != "" {
			return failure("forbidden") // attributed runs cannot change global delivery state
		}
		if s.opts.ResumeDelivery == nil || ctx.Err() != nil {
			return failure("unavailable")
		}
		s.opts.ResumeDelivery()
		return approval.Object{"queued": true}
	case "propose":
		return s.store(ctx, peer, o)
	case "get", "withdraw":
		id, bad := requestID(o, "op", "id")
		if bad != nil {
			return bad
		}
		if o["op"] == "withdraw" {
			p, err := s.journal.Withdraw(id, peer.RunID, s.opts.Now())
			if err != nil {
				return journalError(err)
			}
			return status(p)
		}
		p, _, err := s.owned(id, peer)
		if err != nil {
			return journalError(err)
		}
		return status(p)
	default:
		return invalid("/op")
	}
}

func (s *Server) store(ctx context.Context, peer approval.PeerLabel, o approval.Object) approval.Object {
	r, err := approval.ParseRequest(o)
	if err != nil {
		return journalError(err)
	}
	if !slices.Contains(s.opts.Enabled, r.Type) {
		return failure("type_disabled")
	}
	var place approval.SlackPlacement
	placeSlack := r.Type == approval.Post && s.opts.PlaceSlack != nil
	if placeSlack {
		// Do not spend intake reads for a proposal already over a limit. Recheck after I/O, without
		// holding the socket's proposal lock across a network request.
		s.propose.Lock()
		bad := s.admit(peer, s.opts.Now())
		s.propose.Unlock()
		if bad != nil {
			return bad
		}
		place, err = s.opts.PlaceSlack(ctx, r, peer.RunID)
		if err != nil {
			var r approval.Refusal
			if errors.As(err, &r) && r.Code == "busy" {
				return failure("busy")
			}
			return failure("unavailable")
		}
	}
	s.propose.Lock()
	defer s.propose.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	at := s.opts.Now()
	if bad := s.admit(peer, at); bad != nil {
		return bad
	}
	var p approval.Proposal
	if placeSlack {
		p, err = s.journal.AddSlack(r, peer, at, place)
	} else {
		p, err = s.journal.Add(r, peer, at)
	}
	if err != nil {
		return journalError(err)
	}
	s.recent[peer.RunID] = append(s.recent[peer.RunID], at)
	if s.opts.Proposed != nil {
		s.opts.Proposed()
	}
	answer := status(p)
	answer["params_hash"] = p.ParamsHash
	answer["review_url"] = s.opts.Origin.URL() + "/p/" + p.ID
	if p.RunnerID != "" {
		answer["runner_id"] = p.RunnerID
	}
	return answer
}

// admit runs under the proposal lock. Nil means there is room for the proposal now.
func (s *Server) admit(peer approval.PeerLabel, at time.Time) approval.Object {
	for key, times := range s.recent {
		// Prune all idle identities, not only the caller. Proposals per live run are bounded, but
		// many different runs can connect over a service lifetime.
		times = slices.DeleteFunc(times, func(t time.Time) bool { return at.Sub(t) >= time.Hour })
		if len(times) == 0 {
			delete(s.recent, key)
		} else {
			s.recent[key] = times
		}
	}
	recent := s.recent[peer.RunID]
	if len(recent) >= s.limits.perHour {
		return failure("busy")
	}
	pending, err := s.journal.Pending(at)
	if err != nil {
		return journalError(err)
	}
	if len(pending) >= s.limits.total {
		return failure("busy")
	}
	count := 0
	for _, p := range pending {
		if p.Peer.RunID == peer.RunID {
			count++
		}
	}
	if count >= s.limits.pendingPerPeer {
		return failure("busy")
	}
	return nil
}

func requestID(o approval.Object, keys ...string) (string, approval.Object) {
	if err := approval.CheckKeys(o, "", keys...); err != nil {
		return "", journalError(err)
	}
	id, _ := o["id"].(string)
	if !approval.ValidID(id, "apr") {
		return "", invalid("/id")
	}
	return id, nil
}

func (s *Server) owned(id string, peer approval.PeerLabel) (approval.Proposal, <-chan struct{}, error) {
	p, change, err := s.journal.Get(id, s.opts.Now())
	if err == nil && p.Peer.RunID != peer.RunID {
		err = approval.ErrNotFound
	}
	return p.Proposal, change, err
}

func (s *Server) wait(ctx context.Context, frames *stream, peer approval.PeerLabel, o approval.Object) (approval.Object, bool) {
	id, bad := requestID(o, "op", "id", "after", "timeout")
	if bad != nil {
		return bad, false
	}
	p, change, err := s.owned(id, peer)
	if err != nil {
		return journalError(err), false
	}
	after, ok := o["after"].(int64)
	if !ok || after < 0 {
		return invalid("/after"), false
	}
	seconds, ok := o["timeout"].(int64)
	if !ok || seconds < 1 || seconds > MaxWait {
		return invalid("/timeout"), false
	}
	if p.Version > after {
		return status(p), false
	}
	select {
	case s.waits <- struct{}{}:
		defer func() { <-s.waits }()
	default:
		return failure("busy"), false
	}
	duration := time.Duration(seconds) * time.Second
	deadline := time.Now().Add(duration)
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, true
		case b, ok := <-frames.chunks:
			if !ok {
				return nil, true
			}
			if !frames.add(b) {
				return failure("frame_too_large"), true
			}
			continue // no queued operation can overtake the wait
		case <-change:
		case <-timer.C:
		}
		p, change, err = s.owned(id, peer)
		if err != nil {
			return journalError(err), false
		}
		if p.Version > after || !time.Now().Before(deadline) {
			return status(p), false
		}
	}
}

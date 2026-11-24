package approval

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

// liveJournals lets a passkey check record a reset while this process already holds the journal.
var liveJournals sync.Map

var (
	ErrNotFound   = errors.New("proposal not found")
	ErrTransition = errors.New("proposal is not in the required state")
	ErrStale      = errors.New("frozen proposal no longer matches")
	ErrStopping   = errors.New("approval journal is shutting down")
	ErrJournalAge = errors.New("journal must sort after the archived journal")
	ErrJournalIO  = errors.New("approval journal write was not confirmed; restart to recover")
)

const startWindow = 10 * time.Minute

// frame is the v1 disk schema. An action entry commits its sequence and replay nonce in the same frame.
// Its proposal/transition is the record recipe; the outbox later commits the wire bytes before sending.
type frame struct {
	Kind           string    `json:"kind"`
	ID             string    `json:"id,omitempty"`
	Seq            int64     `json:"seq,omitempty"`
	At             int64     `json:"at,omitempty"` // Unix milliseconds
	Nonce          string    `json:"nonce,omitempty"`
	Proposal       *Proposal `json:"proposal,omitempty"`
	Frozen         *Frozen   `json:"frozen,omitempty"`
	Result         *Result   `json:"result,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Note           string    `json:"note,omitempty"`
	Bytes          []byte    `json:"bytes,omitempty"`
	Ambiguous      bool      `json:"ambiguous,omitempty"`
	CredentialID   []byte    `json:"credential_id,omitempty"`
	BatchChallenge []byte    `json:"batch_challenge,omitempty"`
	Batch          []string  `json:"batch,omitempty"`
	VaultHash      []byte    `json:"vault_hash,omitempty"`
	SlackOperation string    `json:"slack_operation,omitempty"`
}

// View is a detached snapshot of a proposal and any previously committed approval.
type View struct {
	Proposal
	Frozen               *Frozen
	StartedAt, UnknownAt time.Time
	Outcome              *Result
	SlackOperation       string `json:"-"` // durability marker, never part of a wire record
	asserted             bool
}

type proposalState struct {
	View
	order      int
	approvedAt time.Time
	dispatched bool // process-local; replay never dispatches a started action
}

// Journal owns the service's instance lock. It never dispatches an action. A failed append/sync poisons the
// instance: durability is then unknown and nothing may act on its in-memory view until replay.
type Journal struct {
	mu         sync.Mutex
	dir        string
	file, lock *os.File
	id         string
	states     map[string]*proposalState
	active     map[string]*proposalState
	order      []*proposalState
	changed    chan struct{}
	queue      []*frame
	delivery   deliveryHead
	lastSeq    int64
	delivered  int64
	draining   sync.Mutex
	stopping   bool
	failed     bool
	hooks      journalHooks
}

type journalHooks struct {
	newID func(string, time.Time) (string, error)
	nonce func() string
	sync  func(*os.File) error
}

// OpenJournal creates or replays journal.log. When after is given (an archived Swift journal id), refuse
// to create or open an id that does not sort strictly after it. Do not import or modify the archive.
func OpenJournal(dir string, at time.Time, after string) (*Journal, error) {
	return openJournal(dir, at, after, journalHooks{newID, nonce, syncJournal})
}

func openJournal(dir string, at time.Time, after string, hooks journalHooks) (*Journal, error) {
	if after != "" && !ValidID(after, "jrn") {
		return nil, ErrJournalAge
	}
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	lock, err := LockInstance(dir)
	if err != nil {
		return nil, err
	}
	j := &Journal{dir: filepath.Clean(dir), lock: lock, states: make(map[string]*proposalState), active: make(map[string]*proposalState),
		changed: make(chan struct{}), hooks: hooks}
	if err := j.load(filepath.Join(dir, journalName), at, after); err != nil {
		j.Close()
		return nil, err
	}
	liveJournals.Store(j.dir, j)
	return j, nil
}

func (j *Journal) load(path string, at time.Time, after string) error {
	f, err := OpenPrivate(path, os.O_RDWR|os.O_APPEND)
	if errors.Is(err, os.ErrNotExist) {
		id, err := j.hooks.newID("jrn", at)
		if err != nil || !ValidID(id, "jrn") || id <= after {
			return ErrJournalAge
		}
		if err := createJournal(path, id, j.hooks.sync); err != nil {
			return err
		}
		return j.load(path, at, after)
	}
	if err != nil {
		return err
	}
	j.file = f
	r := bufio.NewReader(f)
	h, offset, err := readFrame(r)
	if err != nil || h.Kind != "journal" || !ValidID(h.ID, "jrn") {
		return ErrCorrupt
	}
	j.id = h.ID
	if j.id <= after {
		return ErrJournalAge
	}
	var batch []frame
	batchAt := offset
	for {
		e, size, err := readFrame(r)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// A torn final frame, or approved frames with no commit marker, is not a decision.
			cut := offset
			if len(batch) > 0 {
				cut = batchAt
			}
			if err == io.ErrUnexpectedEOF || len(batch) > 0 {
				if err := f.Truncate(cut); err != nil {
					return err
				}
				if err := j.hooks.sync(f); err != nil {
					return err
				}
			}
			break
		}
		if err != nil {
			return ErrCorrupt
		}
		if e.Kind == "approved" && len(e.BatchChallenge) > 0 {
			s := j.states[e.ID]
			if len(batch) == 10 || s == nil || s.Type != Merge && s.Type != Post {
				return ErrCorrupt
			}
			if len(batch) == 0 {
				batchAt = offset
			}
			batch = append(batch, e)
			offset += size
			continue
		}
		if e.Kind == "approved_commit" {
			if !batchCommitted(batch, e) {
				return ErrCorrupt
			}
			for _, part := range batch {
				if !j.fold(part) {
					return ErrCorrupt
				}
			}
			batch = nil
			offset += size
			batchAt = offset
			continue
		}
		if len(batch) > 0 {
			return ErrCorrupt
		}
		if !j.fold(e) {
			return ErrCorrupt
		}
		offset += size
		batchAt = offset
	}
	// An unacknowledged attempt may have reached Signal before the process died.
	if j.delivery.inFlight {
		j.delivery.ambiguous = true
	}
	for _, s := range j.order {
		switch s.Status {
		case "approved":
			if err := j.transition(s.ID, "expired", "restart_before_start", at); err != nil {
				return err
			}
		case "started":
			if err := j.transition(s.ID, "outcome_unknown", "app_restarted", at); err != nil {
				return err
			}
		}
	}
	return j.sweep(at)
}

func (j *Journal) ID() string { return j.id }

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	liveJournals.Delete(j.dir)
	if j.lock == nil {
		return nil
	}
	err := error(nil)
	if j.file != nil {
		err = j.file.Close()
		j.file = nil
	}
	err = errors.Join(err, CloseInstance(j.dir, j.lock))
	j.lock = nil
	j.notify()
	return err
}

func (j *Journal) notify() {
	close(j.changed)
	j.changed = make(chan struct{})
}

// Changes is closed after a durable action entry, not after a delivery cursor change.
func (j *Journal) Changes() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.changed
}

func (j *Journal) healthy() bool { return j.file != nil && !j.failed }

func (j *Journal) commit(e frame) error {
	if !j.healthy() {
		return ErrJournalIO
	}
	if appendFrame(j.file, e) != nil || j.hooks.sync(j.file) != nil || !j.fold(e) {
		j.failed = true
		j.notify()
		return ErrJournalIO
	}
	return nil
}

func (j *Journal) action(e frame, at time.Time) error {
	e.Seq, e.Nonce, e.At = j.lastSeq+1, j.hooks.nonce(), at.UnixMilli()
	if err := j.commit(e); err != nil {
		return err
	}
	j.notify()
	return nil
}

func (j *Journal) transition(id, kind, reason string, at time.Time) error {
	return j.action(frame{Kind: kind, ID: id, Reason: reason}, at)
}

// RecordLocal writes a local audit marker without allocating a record sequence. It takes no credential or payload.
func (j *Journal) RecordLocal(kind string, at time.Time) error {
	return j.recordLocal(kind, "", nil, at)
}

// RecordPasskey records the SHA-256 of write-vault.json. OpenVault refuses a different file.
func (j *Journal) RecordPasskey(hash []byte, at time.Time) error {
	if len(hash) != 32 {
		return Invalid("/vault_hash")
	}
	return j.recordLocal("passkey_added", "", append([]byte(nil), hash...), at)
}

func (j *Journal) recordLocal(kind, reason string, hash []byte, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !localKind(kind) {
		return Invalid("/kind")
	}
	return j.commit(frame{Kind: kind, At: at.UnixMilli(), Reason: reason, VaultHash: hash})
}

// NoteVaultMismatch records a visible passkey reset while this process holds the journal.
func NoteVaultMismatch(dir string, at time.Time) error {
	v, ok := liveJournals.Load(filepath.Clean(dir))
	if !ok {
		return ErrLocked
	}
	return v.(*Journal).recordLocal("passkeys_reset", "vault_mismatch", nil, at)
}

func localKind(kind string) bool {
	switch kind {
	case "passkey_added", "passkey_removed", "passkeys_reset", "settings_changed", "origin_changed":
		return true
	}
	return false
}

func (j *Journal) Add(r Request, peer PeerLabel, at time.Time) (Proposal, error) {
	return j.addRequest(r, peer, at, nil)
}

// AddSlack commits the verified placement with the proposal. No later lookup can change its subject.
func (j *Journal) AddSlack(r Request, peer PeerLabel, at time.Time, placement SlackPlacement) (Proposal, error) {
	placement.PublicDay = ""
	if placement.Visibility == SlackPublic && r.Params["thread_ts"] == nil {
		placement.PublicDay = at.In(time.Local).Format(time.DateOnly)
	}
	if !placement.valid(r) {
		return Proposal{}, Invalid("/slack_placement")
	}
	return j.addRequest(r, peer, at, &placement)
}

func (j *Journal) addRequest(r Request, peer PeerLabel, at time.Time, placement *SlackPlacement) (Proposal, error) {
	if err := r.check(); err != nil {
		return Proposal{}, err
	}
	if peer.RunID != "" && !ValidID(peer.RunID, "run") {
		return Proposal{}, Invalid("/peer")
	}
	r.Params = maps.Clone(r.Params)
	// Labels from the OS are not always valid UTF-8. Do not mutate action parameters this way.
	peer.Executable = strings.ToValidUTF8(peer.Executable, "�")
	at = time.UnixMilli(at.UnixMilli()).UTC()
	id, err := j.hooks.newID("apr", at)
	if err != nil {
		return Proposal{}, err
	}
	p := Proposal{Request: r, ID: id, Salt: nonce(), Peer: peer, Slack: placement, ProposedAt: at, ExpiresAt: at.Add(Lifetime), Status: "pending", Version: 1}
	if r.Type == Run {
		p.RunnerID, err = j.hooks.newID("run", at)
		if err != nil {
			return Proposal{}, err
		}
	}
	p.ParamsHash, err = paramsDigest(r.Type, r.Params, p.Salt)
	if err != nil {
		return Proposal{}, err
	}
	return j.add(p)
}

func (j *Journal) add(p Proposal) (Proposal, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopping {
		return Proposal{}, ErrStopping
	}
	if j.states[p.ID] != nil {
		return Proposal{}, ErrTransition
	}
	if err := j.action(frame{Kind: "proposed", ID: p.ID, Proposal: &p}, p.ProposedAt); err != nil {
		return Proposal{}, err
	}
	return j.states[p.ID].snapshot().Proposal, nil
}

func (s *proposalState) snapshot() View {
	v := s.View
	v.Params = maps.Clone(v.Params)
	if v.Slack != nil {
		p := *v.Slack
		v.Slack = &p
	}
	if f := v.Frozen; f != nil {
		cp := *f
		cp.Params, cp.Destination = maps.Clone(cp.Params), maps.Clone(cp.Destination)
		v.Frozen = &cp
	}
	if v.Outcome != nil {
		r := *v.Outcome
		v.Outcome = &r
	}
	return v
}

// Get returns a snapshot and a change notification from the same lock acquisition, for race-free waits.
func (j *Journal) Get(id string, at time.Time) (View, <-chan struct{}, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.sweep(at); err != nil {
		return View{}, nil, err
	}
	s := j.states[id]
	if s == nil {
		return View{}, nil, ErrNotFound
	}
	return s.snapshot(), j.changed, nil
}

func (j *Journal) Pending(at time.Time) ([]Proposal, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.sweep(at); err != nil {
		return nil, err
	}
	var pending []Proposal
	for _, s := range ordered(j.active) {
		if s.Status == "pending" {
			pending = append(pending, s.snapshot().Proposal)
		}
	}
	return pending, nil
}

// Expire updates the journal even with no client reading it. It also wakes waiters and delivery.
func (j *Journal) Expire(at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.sweep(at)
}

func ordered(m map[string]*proposalState) []*proposalState {
	return slices.SortedFunc(maps.Values(m), func(a, b *proposalState) int { return a.order - b.order })
}

func (j *Journal) Withdraw(id, runID string, at time.Time) (Proposal, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.sweep(at); err != nil {
		return Proposal{}, err
	}
	s := j.states[id]
	if s == nil || s.Peer.RunID != runID {
		return Proposal{}, ErrNotFound
	}
	if j.stopping || s.Status != "pending" {
		return Proposal{}, ErrTransition
	}
	if err := j.transition(id, "withdrawn", "proposer_withdrew", at); err != nil {
		return Proposal{}, err
	}
	return s.snapshot().Proposal, nil
}

func (j *Journal) Decline(id, note string, at time.Time) error {
	if note != "" && !(field{max: maxNote}).matches(note) {
		return Invalid("/note")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopping {
		return ErrStopping
	}
	if err := j.require(id, "pending", at); err != nil {
		return err
	}
	return j.action(frame{ID: id, Kind: "declined", Reason: "person_declined", Note: note}, at)
}

// Approve persists an already-authorized decision. The caller MUST verify the person's assertion over f
// before calling. This is not exposed over the proposal socket.
func (j *Journal) Approve(f Frozen, at time.Time) error {
	return j.ApproveAll([]Frozen{f}, nil, nil, at)
}

// ApproveAll commits every frozen approval or none of them. A batch is several approved frames plus a
// commit marker; replay applies the frames only when that marker is present. credentialID is recorded on
// each approved frame. batchChallenge is recorded only for a batch.
func (j *Journal) ApproveAll(items []Frozen, credentialID, batchChallenge []byte, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopping {
		return ErrStopping
	}
	if len(items) == 0 || len(items) > 10 {
		return ErrTransition
	}
	seen := map[string]bool{}
	frames := make([]frame, 0, len(items))
	for _, f := range items {
		if seen[f.ID] {
			return ErrTransition
		}
		seen[f.ID] = true
		if err := j.require(f.ID, "pending", at); err != nil {
			return err
		}
		p := j.states[f.ID].Proposal
		if len(items) > 1 && p.Type != Merge && p.Type != Post {
			return ErrTransition
		}
		selected := ""
		if p.Type == Run {
			selected = text(f.Params, "model")
		}
		bound, err := Freeze(p, f.Binding, selected)
		if err != nil || !reflect.DeepEqual(f, bound) || at.Before(p.ProposedAt) {
			return ErrStale
		}
		part := frame{ID: f.ID, Kind: "approved", Frozen: &bound, CredentialID: append([]byte(nil), credentialID...)}
		if len(items) > 1 {
			part.BatchChallenge = append([]byte(nil), batchChallenge...)
		}
		frames = append(frames, part)
	}
	if len(items) > 1 && len(batchChallenge) == 0 {
		return ErrTransition
	}
	if !j.healthy() {
		return ErrJournalIO
	}
	for i := range frames {
		frames[i].Seq, frames[i].Nonce, frames[i].At = j.lastSeq+int64(i)+1, j.hooks.nonce(), at.UnixMilli()
		if appendFrame(j.file, frames[i]) != nil {
			j.failed = true
			j.notify()
			return ErrJournalIO
		}
	}
	if len(frames) > 1 {
		ids := make([]string, len(frames))
		for i := range frames {
			ids[i] = frames[i].ID
		}
		marker := frame{Kind: "approved_commit", CredentialID: append([]byte(nil), credentialID...), BatchChallenge: append([]byte(nil), batchChallenge...), Batch: ids}
		if appendFrame(j.file, marker) != nil {
			j.failed = true
			j.notify()
			return ErrJournalIO
		}
	}
	if j.hooks.sync(j.file) != nil {
		j.failed = true
		j.notify()
		return ErrJournalIO
	}
	for i := range frames {
		if !j.fold(frames[i]) {
			j.failed = true
			j.notify()
			return ErrJournalIO
		}
	}
	j.notify()
	return nil
}

// Start commits the no-retry barrier before the handler may perform a write. Once started, a process
// crash leaves the outcome unknown; recovery cannot issue a second Start.
func (j *Journal) Start(id string, at time.Time) (Frozen, error) {
	v, err := j.start(id, at, false)
	if err != nil {
		return Frozen{}, err
	}
	return *v.Frozen, nil
}

// StartAsserted consumes a durably committed passkey approval for one handler, not a frozen preview.
// Only the assertion route calls the dispatcher; the credential ID in the approval is a further check.
func (j *Journal) StartAsserted(id string, at time.Time) (View, error) {
	return j.start(id, at, true)
}

func (j *Journal) start(id string, at time.Time, asserted bool) (View, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopping {
		return View{}, ErrStopping
	}
	if err := j.require(id, "approved", at); err != nil {
		return View{}, err
	}
	s := j.states[id]
	if at.Before(s.approvedAt) || asserted && !s.asserted {
		return View{}, ErrTransition
	}
	start := frame{Kind: "started", ID: id}
	if s.Type == Post {
		start.SlackOperation = newSlackOperation()
	}
	if err := j.action(start, at); err != nil {
		return View{}, err
	}
	return s.snapshot(), nil
}

// MayDispatch checks whether the action can begin its final reads.
func (j *Journal) MayDispatch(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dispatchable(j.states[id])
}

// ClaimDispatch is the one-shot barrier at the write, after final reads. It never holds the journal
// lock across a provider call. The durable start already prevents a replay after process death.
func (j *Journal) ClaimDispatch(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.states[id]
	if !j.dispatchable(s) {
		return false
	}
	s.dispatched = true
	return true
}

func (j *Journal) dispatchable(s *proposalState) bool {
	return j.healthy() && !j.stopping && s != nil && s.Status == "started" && s.asserted && !s.dispatched
}

// Unknowns is a read-only recovery queue. It cannot produce a start.
func (j *Journal) Unknowns() []View {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []View
	for _, s := range j.order {
		if s.Status == "outcome_unknown" {
			out = append(out, s.snapshot())
		}
	}
	return out
}

func (j *Journal) Unknown(id, reason string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !statusCode.matches(reason) {
		return Invalid("/reason")
	}
	if err := j.require(id, "started", at); err != nil {
		return err
	}
	return j.transition(id, "outcome_unknown", reason, at)
}

func (j *Journal) SetResult(id string, result Result, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.states[id]
	if s == nil {
		return ErrNotFound
	}
	if s.Status != "started" && !(s.Status == "outcome_unknown" && result.ConfirmedBy == "read_back") {
		return ErrTransition
	}
	if err := result.check(s.Type); err != nil {
		return err
	}
	kind := "executed"
	if result.Error != "" {
		kind = "failed"
	}
	return j.action(frame{ID: id, Kind: kind, Result: &result}, at)
}

func (j *Journal) Shutdown(at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.stopping = true
	for _, s := range ordered(j.active) {
		if s.Status == "approved" {
			if err := j.transition(s.ID, "expired", "quit_before_start", at); err != nil {
				return err
			}
		}
	}
	return nil
}

func (j *Journal) require(id, status string, at time.Time) error {
	if err := j.sweep(at); err != nil {
		return err
	}
	if s := j.states[id]; s == nil {
		return ErrNotFound
	} else if s.Status != status {
		return ErrTransition
	}
	return nil
}

func (j *Journal) sweep(at time.Time) error {
	if !j.healthy() {
		return ErrJournalIO
	}
	for _, s := range ordered(j.active) {
		var reason string
		switch {
		case s.Status == "pending" && !at.Before(s.ExpiresAt):
			reason = "proposal_expired"
		case s.Status == "approved" && at.Sub(s.approvedAt) > startWindow:
			reason = "start_window_passed"
		}
		if reason != "" {
			if err := j.transition(s.ID, "expired", reason, at); err != nil {
				return err
			}
		}
	}
	return nil
}

// Replay checks the disk format and transition order, not today's intake parameter schema.
func (j *Journal) fold(e frame) bool {
	if localKind(e.Kind) {
		return e.Seq == 0 && e.ID == ""
	}
	if e.ID == "" {
		return j.foldDelivery(e)
	}
	if e.Seq != j.lastSeq+1 || !replayNonce.matches(e.Nonce) || !ValidID(e.ID, "apr") {
		return false
	}
	s := j.states[e.ID]
	if e.Kind == "proposed" {
		if s != nil || e.Proposal == nil || e.Proposal.ID != e.ID || e.Proposal.Params == nil ||
			e.Proposal.Status != "pending" || e.Proposal.Version != 1 || e.Proposal.Slack != nil && !e.Proposal.Slack.valid(e.Proposal.Request) {
			return false
		}
		s = &proposalState{View: View{Proposal: *e.Proposal}, order: len(j.order)}
		j.states[s.ID], j.active[s.ID] = s, s
		j.order = append(j.order, s)
	} else {
		if s == nil || !foldTransition(s, e) {
			return false
		}
		s.Status = e.Kind
		s.Version++
		if s.Status != "pending" && s.Status != "approved" {
			delete(j.active, s.ID)
		}
	}
	j.lastSeq++
	j.queue = append(j.queue, &e)
	return true
}

func batchCommitted(parts []frame, marker frame) bool {
	if len(parts) < 2 || len(parts) > 10 || len(marker.Batch) != len(parts) || len(marker.BatchChallenge) == 0 {
		return false
	}
	for i, part := range parts {
		if marker.Batch[i] != part.ID || !bytes.Equal(part.BatchChallenge, marker.BatchChallenge) || !bytes.Equal(part.CredentialID, marker.CredentialID) {
			return false
		}
	}
	return true
}

// PasskeyMarks reports the journal position of the latest passkey_added and passkeys_reset.
// Position is the frame's place in the file, starting at 1. Zero means that kind is absent.
// A missing journal is a first registration, not an error.
func PasskeyMarks(dir string) (addedIndex, resetIndex int, err error) {
	st, err := scanPasskeys(dir)
	return st.addedIndex, st.resetIndex, err
}

// PasskeyVault is the SHA-256 recorded on the latest passkey_added that no later passkeys_reset superseded.
// Later means a higher frame index, not a later wall-clock time.
func PasskeyVault(dir string) (hash []byte, active bool, err error) {
	st, err := scanPasskeys(dir)
	if err != nil || st.addedIndex == 0 || st.resetIndex > st.addedIndex || len(st.hash) != 32 {
		return nil, false, err
	}
	return append([]byte(nil), st.hash...), true, nil
}

type passkeyScan struct {
	addedIndex, resetIndex int
	hash                   []byte
}

func scanPasskeys(dir string) (passkeyScan, error) {
	var st passkeyScan
	f, err := OpenPrivate(filepath.Join(dir, journalName), os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if _, _, err := readFrame(r); err != nil {
		return st, err
	}
	index := 0
	for {
		e, _, err := readFrame(r)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return st, nil
		}
		if err != nil {
			return passkeyScan{}, err
		}
		index++
		switch e.Kind {
		case "passkey_added":
			st.addedIndex, st.hash = index, append([]byte(nil), e.VaultHash...)
		case "passkeys_reset":
			st.resetIndex = index
		}
	}
}

func foldTransition(s *proposalState, e frame) bool {
	switch e.Kind {
	case "approved":
		if s.Status != "pending" || e.Frozen == nil || e.Frozen.ID != s.ID || e.Frozen.Version != s.Version {
			return false
		}
		s.Frozen, s.approvedAt = e.Frozen, time.UnixMilli(e.At)
		s.asserted = len(e.CredentialID) != 0
		return true
	case "withdrawn", "declined":
		return s.Status == "pending"
	case "expired":
		return s.Status == "pending" || s.Status == "approved"
	case "started":
		if s.Status != "approved" || e.SlackOperation != "" && (s.Type != Post || !slackOperation.matches(e.SlackOperation)) {
			return false
		}
		if s.Slack != nil && e.SlackOperation == "" {
			return false // a verified Slack placement is new enough to require an operation UUID
		}
		s.StartedAt = time.UnixMilli(e.At)
		s.SlackOperation = e.SlackOperation
		return true
	case "outcome_unknown":
		if s.Status != "started" {
			return false
		}
		s.UnknownAt = time.UnixMilli(e.At)
		return true
	case "executed", "failed":
		if e.Result == nil || !(s.Status == "started" || s.Status == "outcome_unknown" && e.Result.ConfirmedBy == "read_back") {
			return false
		}
		s.Outcome = e.Result
		return true
	}
	return false
}

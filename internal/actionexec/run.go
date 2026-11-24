package actionexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/keychain"
	"github.com/CrossTerra/beacon/internal/supervisor"
)

// RunHandler claims a checkout at approval, then transfers its claim to the reaper's run ticket.
// It does not keep the dispatcher busy for the life of the child or hand a credential to it.
type RunHandler struct {
	settings RunSettings
	sup      *supervisor.Supervisor
	parent   context.Context
	now      func() time.Time
	dir      string
	account  string
	name     string
	env      []string

	mu       sync.Mutex
	claims   map[string]*runClaim // by directory identity, not by setting name
	stopping bool
	monitors sync.WaitGroup
	joined   chan struct{}
	failed   chan error
}

type runClaim struct {
	id, checkout string
	pin          *supervisor.Directory // kept until PrepareRun hands it to the reaper
	run          *supervisor.Run
}

func NewRunHandler(ctx context.Context, s RunSettings, state string, sup *supervisor.Supervisor, now func() time.Time) (*RunHandler, error) {
	u, err := user.Current()
	if err != nil || u.Username == "" || !absolutePath(u.HomeDir) || strings.ContainsRune(u.HomeDir, ':') {
		return nil, errors.New("could not identify the local run account")
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	// Opt-in preview: use the person's home and OpenCode's own sign-in, but never inherit
	// the service's variables (including loader variables or tokens) or run a profile.
	path := filepath.Dir(s.Opencode) + ":" + filepath.Join(u.HomeDir, ".local/bin") + ":/usr/bin:/bin:/usr/sbin:/sbin"
	return &RunHandler{settings: s, dir: filepath.Join(state, "runs"), sup: sup, parent: ctx, now: now,
		account: osName + ":user:" + u.Username, name: u.Username,
		env:    []string{"HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username, "PATH=" + path, "LANG=C", "TERM=dumb"},
		claims: make(map[string]*runClaim), joined: make(chan struct{}), failed: make(chan error, 1)}, nil
}

func (h *RunHandler) checkout(name string) string {
	for _, c := range h.settings.Checkouts {
		if c.Name == name {
			return c.Path
		}
	}
	return ""
}

func checkoutID(pin *supervisor.Directory) string {
	return fmt.Sprintf("vol:%d/inode:%d", pin.Device, pin.Inode)
}

func str(o approval.Object, key string) string { s, _ := o[key].(string); return s }

func (h *RunHandler) ready() bool { return h.parent.Err() == nil && h.sup.Ready() }

func (h *RunHandler) Resolve(ctx context.Context, p approval.Proposal) (approval.Resolved, error) {
	if !h.ready() || ctx.Err() != nil {
		return approval.Resolved{}, refusal("supervisor_not_ready")
	}
	if p.Type != approval.Run || approval.Run.CheckParams(p.Params) != nil {
		return approval.Resolved{}, approval.Invalid("/params")
	}
	name, model := str(p.Params, "checkout"), str(p.Params, "model")
	path := h.checkout(name)
	if path == "" {
		return approval.Resolved{}, refusal("unknown_checkout")
	}
	if !executableFile(h.settings.Opencode) {
		return approval.Resolved{}, refusal("opencode_missing")
	}
	if !slices.Contains(h.settings.Models, model) {
		return approval.Resolved{}, refusal("model_unavailable")
	}
	pin, err := supervisor.PinDirectory(path)
	if err != nil {
		return approval.Resolved{}, refusal("checkout_missing")
	}
	id, branch := checkoutID(pin), checkoutBranch(pin)
	h.mu.Lock()
	busy := h.claims[id] != nil
	h.mu.Unlock()
	if busy {
		_ = pin.Close()
		return approval.Resolved{}, refusal("checkout_busy")
	}
	if !pin.MatchesPath() {
		_ = pin.Close()
		return approval.Resolved{}, refusal("checkout_changed")
	}
	facts := approval.Object{
		"Checkout": path,
		"Branch":   branch,
		"Runs as":  "opencode run --auto: permissions not denied by OpenCode configuration may be approved. This is not a sandbox.",
	}
	if executableFile(filepath.Join(path, "bin", "run")) {
		facts["Note"] = "This checkout has its own bin/run. Beacon runs opencode directly, without it."
	}
	return approval.Resolved{Binding: approval.Binding{Account: h.account, Destination: approval.Object{
		"checkout_id": id, "branch": branch,
	}}, AccountLabel: h.name, DestinationLabel: name + " · " + branch + " · " + path, Facts: facts, Pin: pin}, nil
}

// Unlock is called only after the site's assertion and PRF vault check. There is no provider
// write credential for a run, so the verified key is not kept or handed to the worker.
func (h *RunHandler) Unlock(f *approval.Frozen, key []byte) (keychain.Secret, error) {
	if len(key) != 32 {
		return keychain.Secret{}, refusal("credential_unavailable")
	}
	if f == nil || f.Origin != "" || f.Account != h.account || approval.Run.CheckParams(f.Params) != nil ||
		h.checkout(str(f.Params, "checkout")) == "" || !slices.Contains(h.settings.Models, str(f.Params, "model")) ||
		str(f.Destination, "checkout_id") == "" {
		return keychain.Secret{}, refusal("destination_changed")
	}
	return keychain.Secret{}, nil
}

// Reserve runs before ApproveAll; a busy checkout leaves the proposal pending. The returned
// cleanup releases only an untransferred claim (e.g. if ApproveAll fails or the start expires).
func (h *RunHandler) Reserve(f *approval.Frozen, pin *supervisor.Directory) (func(), error) {
	if !h.ready() {
		return nil, refusal("supervisor_not_ready")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping {
		return nil, approval.ErrStopping
	}
	c := &runClaim{id: f.ID, checkout: str(f.Destination, "checkout_id")}
	if pin == nil || checkoutID(pin) != c.checkout || h.checkout(str(f.Params, "checkout")) != pin.Path {
		return nil, approval.ErrStale
	}
	if h.claims[c.checkout] != nil {
		return nil, refusal("checkout_busy")
	}
	owned, err := pin.Dup()
	if err != nil {
		return nil, refusal("checkout_missing")
	}
	c.pin = owned
	h.claims[c.checkout] = c
	return func() {
		_ = owned.Close() // no-op if the dispatcher has already handed it to the reaper
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.claims[c.checkout] == c && c.run == nil {
			delete(h.claims, c.checkout)
		}
	}, nil
}

func (h *RunHandler) Run(ctx context.Context, j *approval.Journal, v approval.View, token keychain.Secret) error {
	if v.Type != approval.Run || v.Frozen == nil || !token.Empty() || !approval.ValidID(v.RunnerID, "run") {
		return approval.ErrTransition
	}
	if ctx.Err() != nil || !j.MayDispatch(v.ID) || h.parent.Err() != nil {
		return stopBeforeDispatch(j, v.ID, h.now())
	}
	fail := func(code string) error {
		return j.FailUnclaimed(v.ID, approval.Result{ConfirmedBy: "pre_dispatch", Error: code}, h.now())
	}
	f := v.Frozen
	id := str(f.Destination, "checkout_id")
	h.mu.Lock()
	c := h.claims[id]
	reserved := c != nil && c.id == v.ID && c.run == nil && !h.stopping
	h.mu.Unlock()
	if !reserved || !h.ready() {
		return fail("supervisor_unavailable")
	}
	model := str(f.Params, "model")
	if !c.pin.MatchesPath() || checkoutBranch(c.pin) != str(f.Destination, "branch") {
		return fail("checkout_changed")
	}
	if !slices.Contains(h.settings.Models, model) {
		return fail("model_unavailable")
	}
	if !executableFile(h.settings.Opencode) {
		return fail("opencode_missing")
	}
	cmd, err := h.prepare(v.RunnerID, str(f.Params, "prompt"), model, c.pin)
	if err != nil {
		return fail("launch_failed")
	}
	r, err := h.sup.PrepareRun(v.RunnerID, cmd)
	_ = c.pin.Close() // a successful prepare passed the held descriptor to the gated child
	clear(cmd.Input)
	cmd.Input, cmd.Dir = nil, nil
	if err != nil {
		switch {
		case errors.Is(err, supervisor.ErrCheckoutChanged):
			return fail("checkout_changed")
		case errors.Is(err, supervisor.ErrStopped):
			return stopBeforeDispatch(j, v.ID, h.now())
		default:
			return fail("launch_failed")
		}
	}
	h.mu.Lock()
	c.run = r
	h.mu.Unlock()
	launched := false
	defer func() {
		h.monitors.Add(1)
		go h.monitor(j, c, cmd, launched)
	}()
	// PrepareRun registered the group but has given the child neither args nor environment.
	// Record that attribution and consume the one-shot permit before Release can exec.
	if ctx.Err() != nil || !h.ready() {
		_ = r.Stop()
		return stopBeforeDispatch(j, v.ID, h.now())
	}
	permit, err := j.ClaimRunDispatch(v.ID, r.PGID(), h.now())
	if err != nil {
		_ = r.Stop()
		if errors.Is(err, approval.ErrStopping) {
			return stopBeforeDispatch(j, v.ID, h.now())
		}
		return err
	}
	if ctx.Err() != nil || h.parent.Err() != nil {
		_ = r.Stop()
		return j.SetResult(v.ID, approval.Result{ConfirmedBy: "pre_dispatch", Error: "stopped"}, h.now())
	}
	if err := r.ReleaseWithReceipt(permit); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrCheckoutChanged):
			return j.SetResult(v.ID, approval.Result{ConfirmedBy: "reply", Error: "checkout_changed"}, h.now())
		case errors.Is(err, supervisor.ErrStopped), errors.Is(err, supervisor.ErrPrepareTimeout), errors.Is(err, supervisor.ErrLaunch):
			return j.SetResult(v.ID, approval.Result{ConfirmedBy: "reply", Error: "launch_failed"}, h.now())
		default:
			return j.Unknown(v.ID, "launch_unconfirmed", h.now())
		}
	}
	if err := j.SetResult(v.ID, approval.Result{ConfirmedBy: "reply"}, h.now()); err != nil {
		return err
	}
	launched = true
	if h.parent.Err() != nil {
		return nil
	}
	return j.RecordRunner(v.ID, approval.RunnerEvent{Kind: approval.RunnerWorking}, h.now())
}

func (h *RunHandler) prepare(id, prompt, model string, pin *supervisor.Directory) (supervisor.Command, error) {
	if err := approval.PrivateDir(h.dir); err != nil {
		return supervisor.Command{}, err
	}
	dir := filepath.Join(h.dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return supervisor.Command{}, err
	}
	return supervisor.Command{Path: h.settings.Opencode, Args: []string{"run", "--standalone", "--format", "json", "--auto", "--model", model},
		Env: h.env, Dir: pin, Input: []byte(prompt), Stdout: filepath.Join(dir, "output"), Stderr: filepath.Join(dir, "errors")}, nil
}

// RecoverRunFiles deletes temporary input left by a crash. Call under instance.lock, after the
// sweep, before replayed runs can be marked closed. Keep output files private and local.
func RecoverRunFiles(state string) error {
	dir := filepath.Join(state, "runs")
	if _, err := approval.CheckPrivate(dir, os.ModeDir|0o700); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("run directory is not private")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("could not read run directory")
	}
	for _, ent := range entries {
		if !approval.ValidID(ent.Name(), "run") {
			continue
		}
		p := filepath.Join(dir, ent.Name())
		if _, err := approval.CheckPrivate(p, os.ModeDir|0o700); err != nil {
			return errors.New("run directory is not private")
		}
		if err := os.Remove(filepath.Join(p, "prompt")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("could not discard recovered run input")
		}
	}
	return nil
}

func (h *RunHandler) monitor(j *approval.Journal, c *runClaim, cmd supervisor.Command, launched bool) {
	defer h.monitors.Done()
	r := c.run
	watch := time.NewTicker(2 * time.Second)
	defer watch.Stop()
	leader := r.LeaderDone()
	outcomeRecorded, requestedStop := false, false
	recordLeader := func(exit supervisor.LeaderExit) {
		if launched && !outcomeRecorded {
			outcomeRecorded = true
			h.record(j, c.id, runOutcome(cmd, exit))
		}
	}
	settle := func(confirmed bool) {
		if exit, ok := r.LeaderExit(); ok {
			recordLeader(exit) // the exit and cleanup notifications can both be ready
		} else if launched && !outcomeRecorded {
			outcomeRecorded = true
			h.record(j, c.id, approval.RunnerEvent{Kind: approval.RunnerFailed, Error: "supervisor_ended"})
		}
		if !confirmed {
			return // preserve the outcome but don't close or free the checkout
		}
		h.mu.Lock()
		delete(h.claims, c.checkout)
		h.mu.Unlock()
		if launched {
			h.record(j, c.id, approval.RunnerEvent{Kind: approval.RunnerClosed})
		}
	}
	for {
		select {
		case <-leader:
			exit, _ := r.LeaderExit()
			recordLeader(exit)
			leader = nil
		case <-r.Done():
			_, err := r.Wait(context.Background())
			settle(err == nil || !launched && !errors.Is(err, supervisor.ErrUnconfirmed))
			return
		case <-h.joined:
			// Close reached its bound. Accept a completion already received, but do not
			// wait forever or claim cleanup when the reaper could not confirm it.
			select {
			case <-r.Done():
				_, err := r.Wait(context.Background())
				settle(err == nil || !launched && !errors.Is(err, supervisor.ErrUnconfirmed))
			default:
				settle(false)
			}
			return
		case <-watch.C:
			if launched && leader != nil && !requestedStop && outputTooLarge(cmd) {
				requestedStop = r.StopWithReason("output_too_large") == nil
			}
		}
	}
}

func outputTooLarge(cmd supervisor.Command) bool {
	var bytes int64
	for _, path := range []string{cmd.Stdout, cmd.Stderr} {
		if st, err := approval.CheckPrivate(path, 0o600); err == nil {
			if st.Size() > runOutputLimit-bytes {
				return true
			}
			bytes += st.Size()
		}
	}
	return false
}

func runOutcome(cmd supervisor.Command, exit supervisor.LeaderExit) approval.RunnerEvent {
	reason := exit.Reason
	event := approval.RunnerEvent{Kind: approval.RunnerFailed, Error: reason}
	if reason == "stopped_at_quit" {
		event.Text = "Beacon stopped while the run was working."
		return event
	}
	if reason == "output_too_large" || outputTooLarge(cmd) {
		return approval.RunnerEvent{Kind: approval.RunnerFailed, Error: "output_too_large"}
	}
	report := readRunReport(cmd.Stdout)
	switch {
	case reason != "":
		event.Text = firstText(report.text, report.problem)
	case exit.Code == 0:
		// Catch a burst between ticks (or growth while the report was read). A prefix
		// of an oversized stream cannot establish what the final message was.
		if outputTooLarge(cmd) {
			return approval.RunnerEvent{Kind: approval.RunnerFailed, Error: "output_too_large"}
		}
		return approval.RunnerEvent{Kind: approval.RunnerFinished, Text: firstText(report.text, "The run ended without a final message.")}
	default:
		event.Error = "exit_" + strconv.Itoa(exit.Code)
		if exit.Code == 130 {
			event.Error = "interrupted"
		} else if exit.Code > 128 {
			event.Error = "signal_" + strconv.Itoa(exit.Code-128)
		}
		event.Text = firstText(report.problem, errorTail(cmd.Stderr), report.text)
	}
	return event
}

func (h *RunHandler) record(j *approval.Journal, id string, event approval.RunnerEvent) {
	if err := j.RecordRunner(id, event, h.now()); err != nil {
		select {
		case h.failed <- err:
		default:
		}
	}
}

func (h *RunHandler) running(id string) *runClaim {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping || !h.ready() {
		return nil
	}
	for _, c := range h.claims {
		if c.id == id && c.run != nil {
			select {
			case <-c.run.Done():
			case <-c.run.LeaderDone():
			default:
				return c
			}
		}
	}
	return nil
}

func (h *RunHandler) CanStop(id string) bool { return h.running(id) != nil }

func (h *RunHandler) Stop(id string) error {
	if c := h.running(id); c != nil {
		if err := c.run.Stop(); err != nil {
			return refusal("supervisor_unavailable")
		}
		return nil
	}
	return approval.ErrTransition
}

func (h *RunHandler) BeginShutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopping = true
}

func (h *RunHandler) Failed() <-chan error { return h.failed }

// Join after dispatch has stopped and the supervisor has finished its bounded Close.
func (h *RunHandler) Join() error {
	close(h.joined)
	h.monitors.Wait()
	select {
	case err := <-h.failed:
		return err
	default:
		return nil
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/CrossTerra/beacon/internal/peer"
)

const actUsage = `usage: beacon act types
       beacon act propose < REQUEST.json
       beacon act get ID
       beacon act wait ID [--after VERSION] [--timeout SECONDS]
       beacon act withdraw ID
       beacon act quit
`

// actFrameMax is the largest frame either side sends, newline included (13.00, _The local API_).
const actFrameMax = 256 * 1024

// actSettings is ~/Library/Application Support/beacon/act.json. The person writes it at setup. requirement is
// Beacon.app's code requirement (its identifier and Apple Development certificate): the client checks the
// socket's listener against it before sending a byte.
type actSettings struct {
	Requirement string `json:"requirement"`
	Socket      string `json:"socket,omitempty"`
}

func (a *app) actSettingsPath() (string, error) {
	if a.actSettingsFile != "" {
		return a.actSettingsFile, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "beacon", "act.json"), nil
}

func (a *app) loadActSettings() (actSettings, error) {
	var s actSettings
	path, err := a.actSettingsPath()
	if err != nil {
		return s, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("beacon act needs %s with Beacon.app's code requirement; see docs/commands.md", path)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || s.Requirement == "" {
		return s, fmt.Errorf("%s must be {\"requirement\": \"…\"}", path)
	}
	if s.Socket == "" {
		s.Socket = filepath.Join(filepath.Dir(path), "run", "act.sock")
	}
	return s, nil
}

// cmdAct sends one request to Beacon.app's proposal socket and prints the one-line JSON reply. A reply with
// "error" exits 1. The client cannot approve: the socket has no such operation.
func (a *app) cmdAct(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, actUsage)
		return exitError{2}
	}
	request := map[string]any{"op": args[0]}
	var wait time.Duration
	switch args[0] {
	case "types", "quit":
		if len(args) != 1 {
			fmt.Fprint(a.stderr, actUsage)
			return exitError{2}
		}
	case "propose":
		if len(args) != 1 {
			fmt.Fprint(a.stderr, actUsage)
			return exitError{2}
		}
		body, err := io.ReadAll(io.LimitReader(a.stdin, actFrameMax))
		if err != nil {
			return err
		}
		if len(body) >= actFrameMax-len(`,"op":"propose"`)-1 {
			return errors.New("the request is larger than one 256 KiB frame")
		}
		var fields map[string]json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(body))
		if err := dec.Decode(&fields); err != nil || dec.More() {
			return errors.New("the request on standard input must be one JSON object")
		}
		if _, ok := fields["op"]; ok {
			return errors.New("the request must not name op")
		}
		for k, v := range fields {
			request[k] = v
		}
	case "get", "withdraw":
		if len(args) != 2 {
			fmt.Fprint(a.stderr, actUsage)
			return exitError{2}
		}
		request["id"] = args[1]
	case "wait":
		fs, _ := a.flags("act wait")
		after := fs.Int64("after", 0, "answer once the proposal's version is past this")
		timeout := fs.Int("timeout", 300, "seconds to wait, at most 300")
		if len(args) < 2 {
			fmt.Fprint(a.stderr, actUsage)
			return exitError{2}
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			fmt.Fprint(a.stderr, actUsage)
			return exitError{2}
		}
		request["id"], request["after"], request["timeout"] = args[1], *after, *timeout
		wait = time.Duration(*timeout) * time.Second
	default:
		fmt.Fprint(a.stderr, actUsage)
		return exitError{2}
	}
	settings, err := a.loadActSettings()
	if err != nil {
		return err
	}
	frame, err := json.Marshal(request)
	if err != nil {
		return err
	}
	reply, err := actCall(ctx, settings, append(frame, '\n'), 10*time.Second+wait)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, string(reply))
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(reply, &answer) != nil || answer.Error != "" {
		return exitError{1}
	}
	return nil
}

// actCall connects, checks the listener, and only then writes the frame and reads one reply line.
func actCall(ctx context.Context, settings actSettings, frame []byte, timeout time.Duration) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", settings.Socket)
	if err != nil {
		return nil, errors.New("the proposal socket is not open: Beacon.app is not running, or does not listen yet")
	}
	defer conn.Close()
	unix, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.New("the proposal socket is not a Unix socket")
	}
	if err := peer.Check(unix, settings.Requirement); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(frame); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(io.LimitReader(conn, actFrameMax), actFrameMax).ReadSlice('\n')
	if err != nil {
		return nil, errors.New("the app closed the connection without a reply")
	}
	return bytes.TrimSuffix(line, []byte("\n")), nil
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/CrossTerra/beacon/internal/actsocket"
	"github.com/CrossTerra/beacon/internal/approval"
	"github.com/CrossTerra/beacon/internal/localstate"
)

const actUsage = `usage: beacon act [--state-dir DIR] types
       beacon act [--state-dir DIR] propose < REQUEST.json
       beacon act [--state-dir DIR] get ID
       beacon act [--state-dir DIR] wait ID [--after VERSION] [--timeout SECONDS]
       beacon act [--state-dir DIR] withdraw ID
       beacon act [--state-dir DIR] resume-delivery
`

// cmdAct uses only the same-user socket. It never falls back to an older listener or executes an action.
func (a *app) cmdAct(ctx context.Context, args []string) error {
	fs, path := a.reviewFlags("act")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	usage := func() error {
		fmt.Fprint(a.stderr, actUsage)
		return exitError{2}
	}
	if len(args) == 0 {
		return usage()
	}
	request := approval.Object{"op": args[0]}
	var wait time.Duration
	switch args[0] {
	case "types", "resume-delivery":
		if len(args) != 1 {
			return usage()
		}
	case "propose":
		if len(args) != 1 {
			return usage()
		}
		body, err := io.ReadAll(io.LimitReader(a.stdin, actsocket.FrameMax))
		if err != nil {
			return err
		}
		if len(body) >= actsocket.FrameMax-len(`,"op":"propose"`)-1 {
			return errors.New("the request is larger than one 256 KiB frame")
		}
		fields, err := approval.ParseObject(body)
		if err != nil {
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
			return usage()
		}
		request["id"] = args[1]
	case "wait":
		if len(args) < 2 {
			return usage()
		}
		fs, _ := a.flags("act wait")
		after := fs.Int64("after", 0, "answer once the proposal's version is past this")
		timeout := fs.Int("timeout", actsocket.MaxWait, "seconds to wait")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *timeout < 1 || *timeout > actsocket.MaxWait {
			return usage()
		}
		request["id"], request["after"], request["timeout"] = args[1], *after, *timeout
		wait = time.Duration(*timeout) * time.Second
	default:
		return usage()
	}
	dir, err := localstate.Dir(*path, false)
	if err != nil {
		return err
	}
	if _, err := localstate.Load(dir); err != nil {
		return errors.New("Go Beacon is not set up in this state directory")
	}
	reply, err := actsocket.Call(ctx, dir, request, wait)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, string(reply))
	var answer struct{ Error string }
	if json.Unmarshal(reply, &answer) != nil || answer.Error != "" {
		return exitError{1}
	}
	return nil
}

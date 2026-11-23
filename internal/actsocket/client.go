package actsocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

// Call sends one Go socket request. Ownership and modes, then kernel uid, are checked before any bytes
// are sent. This client is not used for Beacon.app's signed socket.
func Call(ctx context.Context, dir string, request approval.Object, wait time.Duration) ([]byte, error) {
	frame, err := marshalFrame(request, FrameMax)
	if err != nil {
		return nil, err
	}
	c, err := dial(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if err := c.SetWriteDeadline(time.Now().Add(frameTimeout)); err != nil {
		return nil, err
	}
	if _, err := c.Write(frame); err != nil {
		return nil, err
	}
	if err := c.SetReadDeadline(time.Now().Add(frameTimeout + min(max(wait, 0), MaxWait*time.Second))); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(c, FrameMax).ReadSlice('\n')
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("the Go proposal socket did not return a complete reply")
	}
	line = line[:len(line)-1]
	if _, err := approval.ParseObject(line); err != nil {
		return nil, errors.New("the Go proposal socket returned an invalid reply")
	}
	return line, nil
}

var errFrameLarge = errors.New("the request is larger than one 256 KiB frame")

func marshalFrame(object approval.Object, limit int) ([]byte, error) {
	b, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	if len(b) >= limit {
		return nil, errFrameLarge
	}
	return append(b, '\n'), nil
}

package actsocket

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"time"
)

const frameMargin = 4096 // allowance for bytes pipelined behind one frame during a wait

type stream struct {
	chunks  <-chan []byte
	buf     []byte
	partial time.Time
	limit   int
}

// Reading continues during a wait so a disconnected peer frees its slot immediately. This reader
// can hold only one additional chunk; the connection loop owns, and bounds, all buffered frames.
func readChunks(ctx context.Context, c *net.UnixConn) <-chan []byte {
	out := make(chan []byte)
	go func() {
		defer close(out)
		for {
			b := make([]byte, frameMargin)
			n, err := c.Read(b)
			if n > 0 {
				select {
				case out <- b[:n]:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

func (s *stream) add(b []byte) bool {
	if len(s.buf) == 0 {
		s.partial = time.Now()
	}
	if len(s.buf)+len(b) > s.limit+frameMargin {
		return false
	}
	s.buf = append(s.buf, b...)
	return s.frameEnd() <= s.limit
}

// frameEnd is zero for an incomplete frame, or the size including the newline. A frame without room
// for its newline is already too large.
func (s *stream) frameEnd() int {
	if i := bytes.IndexByte(s.buf, '\n'); i >= 0 {
		return i + 1
	}
	if len(s.buf) >= s.limit {
		return s.limit + 1
	}
	return 0
}

func (s *stream) next(ctx context.Context, idle, frame time.Duration) ([]byte, error) {
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		end := s.frameEnd()
		if end > s.limit {
			return nil, errFrameLarge
		}
		if end > 0 {
			b := s.buf[:end-1]
			s.buf = s.buf[end:]
			if len(s.buf) != 0 {
				s.partial = time.Now()
			}
			return b, nil
		}
		if len(s.buf) != 0 {
			d := time.Until(s.partial.Add(frame))
			if d <= 0 {
				return nil, os.ErrDeadlineExceeded
			}
			timer.Reset(d) // later bytes never extend the first byte's deadline
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, os.ErrDeadlineExceeded
		case b, ok := <-s.chunks:
			if !ok {
				return nil, io.EOF
			}
			if !s.add(b) {
				return nil, errFrameLarge
			}
		}
	}
}

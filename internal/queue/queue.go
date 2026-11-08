// Package queue keeps what Beacon decided not to send now: held items waiting for review (DEC-4) and the record of
// dropped items. Both are local files; nothing in them leaves the machine.
package queue

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// File names inside the state directory.
const (
	HeldFile    = "held.jsonl"
	DroppedFile = "dropped.jsonl"
)

// Held is an item a hold rule matched. It waits for the person's approval, which Phase 5 adds.
type Held struct {
	Key    string      `json:"key"`
	Rule   string      `json:"rule"`
	HeldAt time.Time   `json:"held_at"`
	Item   source.Item `json:"item"`
}

// Dropped records that an item was dropped, and by which rule; an empty rule is the default. It holds no content.
type Dropped struct {
	Key       string    `json:"key"`
	Rule      string    `json:"rule,omitempty"`
	DroppedAt time.Time `json:"dropped_at"`
}

// Queue is the held and dropped records of one state directory.
type Queue struct {
	dir     string
	held    map[string]Held
	order   []string
	dropped map[string]Dropped
}

// Open loads the queue in dir.
func Open(dir string) (*Queue, error) {
	q := &Queue{dir: dir, held: map[string]Held{}, dropped: map[string]Dropped{}}
	err := readLines(filepath.Join(dir, HeldFile), func(line []byte) error {
		var h Held
		if err := json.Unmarshal(line, &h); err != nil {
			return err
		}
		if _, ok := q.held[h.Key]; !ok {
			q.order = append(q.order, h.Key)
		}
		q.held[h.Key] = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = readLines(filepath.Join(dir, DroppedFile), func(line []byte) error {
		var d Dropped
		if err := json.Unmarshal(line, &d); err != nil {
			return err
		}
		q.dropped[d.Key] = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return q, nil
}

// readLines calls fn for each complete line. A last line torn by a crash (no newline, or not JSON) is cut off the
// file, so the next append starts on a line of its own.
func readLines(path string, fn func([]byte) error) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var good int64
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !json.Valid(line) {
			if _, peek := br.Peek(1); errors.Is(peek, io.EOF) {
				break
			}
			return fmt.Errorf("%s line %d: not JSON", path, n)
		}
		if err := fn(line); err != nil {
			return fmt.Errorf("%s line %d: %w", path, n, err)
		}
		good += int64(len(line))
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > good {
		if err := f.Truncate(good); err != nil {
			return err
		}
		return f.Sync()
	}
	return nil
}

func appendLine(path string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := syncDir(filepath.Dir(path)); err != nil {
			f.Close()
			return err
		}
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Hold adds an item to the hold queue. Holding a key twice is a no-op.
func (q *Queue) Hold(h Held) error {
	if _, ok := q.held[h.Key]; ok {
		return nil
	}
	if err := appendLine(filepath.Join(q.dir, HeldFile), h); err != nil {
		return err
	}
	q.held[h.Key] = h
	q.order = append(q.order, h.Key)
	return nil
}

// IsHeld reports whether a key is in the hold queue.
func (q *Queue) IsHeld(key string) bool { _, ok := q.held[key]; return ok }

// HeldItems returns the hold queue, oldest first.
func (q *Queue) HeldItems() []Held {
	out := make([]Held, 0, len(q.order))
	for _, k := range q.order {
		out = append(out, q.held[k])
	}
	return out
}

// DroppedBy reports whether a key is recorded as dropped, and by which rule ("" for the default).
func (q *Queue) DroppedBy(key string) (rule string, ok bool) {
	d, ok := q.dropped[key]
	return d.Rule, ok
}

// Drop records a dropped key once.
func (q *Queue) Drop(d Dropped) error {
	if prev, ok := q.dropped[d.Key]; ok && prev.Rule == d.Rule {
		return nil
	}
	if err := appendLine(filepath.Join(q.dir, DroppedFile), d); err != nil {
		return err
	}
	q.dropped[d.Key] = d
	return nil
}

// Package seed reads a seed file (DEC-24). A line is "<subject> never|send". A never line drops that subject before
// the rules file, so an item already dismissed stays dismissed. A send line is recorded and does not force a send:
// the rules file still decides, and the default remains deny.
package seed

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RuleName is the built-in rule a never line applies, before the rules file.
const RuleName = "seed"

// FileName is the imported seed in the state directory.
const FileName = "seed"

// File is a parsed seed.
type File struct {
	never map[string]bool
	send  map[string]bool
}

// Parse parses a seed file. A '#' starts a comment only when it is the first non-space character, because a subject
// may contain '#'. A subject may appear once.
func Parse(data []byte) (*File, error) {
	f := &File{never: map[string]bool{}, send: map[string]bool{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("seed: line %d: want <subject> never|send", line)
		}
		subject, decision := fields[0], fields[1]
		if strings.ContainsAny(subject, " \t") || subject == "" {
			return nil, fmt.Errorf("seed: line %d: the subject is empty", line)
		}
		if f.never[subject] || f.send[subject] {
			return nil, fmt.Errorf("seed: line %d: %s is already in the file", line, subject)
		}
		switch decision {
		case "never":
			f.never[subject] = true
		case "send":
			f.send[subject] = true
		default:
			return nil, fmt.Errorf("seed: line %d: decision is never or send", line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return f, nil
}

// Drops reports whether a never line names the subject. Send lines do not drop and do not force a send.
func (f *File) Drops(subject string) bool {
	if f == nil {
		return false
	}
	return f.never[subject]
}

// Counts returns how many never and send lines the file has.
func (f *File) Counts() (never, send int) {
	if f == nil {
		return 0, 0
	}
	return len(f.never), len(f.send)
}

// Load reads a seed file.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// ReadDir reads the imported seed in the state directory. A missing file is an empty seed, not an error.
func ReadDir(dir string) (*File, error) {
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return &File{never: map[string]bool{}, send: map[string]bool{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Write validates data and stores it in the state directory, replacing any imported seed.
func Write(dir string, data []byte) (*File, error) {
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, FileName)
	tmp, err := os.CreateTemp(dir, FileName+".tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, err
	}
	cleanup = false
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return nil, err
	}
	return f, nil
}

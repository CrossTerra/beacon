package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/CrossTerra/beacon/internal/rules"
	"github.com/CrossTerra/beacon/internal/source"
)

// Signal's limits on a sealed write (src/domain/seal.ts, src/domain/limits.ts, SEALED_REQUEST_MAX_BYTES).
const (
	sealedPartsMax   = 65_536
	sealedRequestMax = 131_072
	metadataMax      = 32 * 1024
)

// ErrBodyNeedsSeal is a body from a personal subject that a plain event would carry (DEC-32). The runner holds such
// an item as body_needs_seal.
var ErrBodyNeedsSeal = errors.New("project: a personal body leaves only sealed (DEC-32)")

// ErrSealShared is a sealed event on a subject that is not a person's own: Signal refuses it (Signal DEC-250).
var ErrSealShared = errors.New("project: Signal seals only a person's own subjects")

// flagFields are the state fields that stay plain on a sealed event, as booleans (DEC-31). Every other state field
// is sealed.
var flagFields = map[string]bool{"unread": true, "starred": true}

// NeedsSeal reports whether a plain event for this item would carry a body from a personal subject (DEC-32).
func NeedsSeal(it source.Item, r *rules.Rule) bool {
	return source.PersonalStem(it.Subject) != "" && r.ListsBody() && it.Fields[FieldBody] != ""
}

// projectSealed builds a sealed event. Outside sealed it carries only the envelope DEC-31 fixes: the subject, the
// type, the key, by, to, priority, due_at, closes, occurred_at, open.title from a plain field, metadata.fields with
// the plain fields, metadata.native with the source's opaque id, and boolean state flags. The event title, the body,
// the actor, references, every other field and every other state field go in sealed.
func projectSealed(in Input) (*Body, []byte, error) {
	r, it := in.Rule, in.Item
	if source.PersonalStem(it.Subject) == "" {
		return nil, nil, ErrSealShared
	}
	if strings.Contains(it.Key, "@") {
		return nil, nil, errors.New("project: a sealed event's key is opaque, and this one holds @")
	}
	plain := map[string]bool{}
	for _, name := range r.Plain {
		plain[name] = true
	}
	b := &Body{
		Subject:    it.Subject,
		Type:       it.Source + "." + it.Type,
		Key:        it.Key,
		OccurredAt: Instant(it.OccurredAt),
		Open:       &Open{},
	}
	sl := &Sealed{Level: string(in.Level)}
	if it.EventTitle != "" {
		sl.Title = title(it.EventTitle)
	}
	plainFields := map[string]string{}
	sealedFields := map[string]string{}
	sealedMeta := map[string]any{}
	listed := map[string]bool{}
	var plainRefs []string
	for _, name := range r.Fields {
		listed[name] = true
		switch name {
		case rules.FieldActor:
			if it.Actor != "" && !it.Own {
				sealedMeta["actor"] = it.Actor
			}
		case rules.FieldReferences:
			if len(it.References) == 0 {
				continue
			}
			// Left plain, they relate this thread to the subjects they name (Signal DEC-284); sealed, they relate
			// nothing.
			if plain[name] {
				plainRefs = append([]string(nil), it.References...)
			} else {
				sealedMeta["references"] = append([]string(nil), it.References...)
			}
		default:
			v, ok := it.Fields[name]
			if !ok {
				continue
			}
			switch {
			case name == FieldBody:
				if !in.Truncated {
					sl.Body = v
				}
			case name == FieldTitle:
				// The object's title is the thread's title only when it is plain; sealed, it is kept with the other
				// sealed fields so a reader the level opens for still has it.
				if !plain[name] {
					sealedFields[name] = v
				}
				if sl.Title == "" {
					sl.Title = title(v)
				}
			case plain[name]:
				plainFields[name] = v
			default:
				sealedFields[name] = v
			}
		}
	}
	// The thread title is the first of title, summary and subject the rule lists, as on a plain event: plain in
	// open.title when that field is plain, else sealed in sealed.open.title, which Signal opens for the readers the
	// level allows and shows to anyone else as "Sealed item" (Signal DEC-281).
	for _, name := range []string{FieldTitle, "summary", "subject"} {
		v, ok := it.Fields[name]
		if !listed[name] || !ok {
			continue
		}
		if t := title(v); t != "" {
			if plain[name] {
				b.Open.Title = t
			} else {
				sl.Open = &Open{Title: t}
			}
		}
		break
	}
	meta := map[string]any{}
	if len(plainFields) > 0 {
		meta["fields"] = plainFields
	}
	if it.Native != "" && !strings.Contains(it.Native, "@") {
		meta["native"] = it.Native
	}
	if len(plainRefs) > 0 {
		meta["references"] = plainRefs
	}
	if len(meta) > 0 {
		b.Metadata = meta
	}
	if len(sealedFields) > 0 {
		sealedMeta["fields"] = sealedFields
	}
	if in.Truncated {
		sealedMeta["truncated"] = true
	}
	if len(sealedMeta) > 0 {
		sl.Metadata = sealedMeta
	}
	if len(it.State) > 0 && it.ObservedAt != nil && it.Version != "" {
		flags := map[string]any{}
		rest := map[string]string{}
		for _, name := range r.State {
			if name == source.StateObservedAt || name == source.StateVersion {
				continue
			}
			v, ok := it.State[name]
			if !ok {
				continue
			}
			if flagFields[name] && (v == "true" || v == "false") {
				flags[name] = v == "true"
			} else {
				rest[name] = v
			}
		}
		// sealed.state is part of a snapshot, so the snapshot is sent even when no flag stays plain.
		if len(flags) > 0 || len(rest) > 0 {
			b.State = &Snapshot{Fields: flags, ObservedAt: Instant(*it.ObservedAt), Version: it.Version}
		}
		if len(rest) > 0 {
			sl.State = rest
		}
	}
	b.Sealed = sl
	envelope(b, in)
	if err := fitSealed(sl); err != nil {
		return nil, nil, err
	}
	if len(sl.Body) > bodyMaxBytes {
		return nil, nil, fmt.Errorf("project: body is %d bytes, over Signal's %d", len(sl.Body), bodyMaxBytes)
	}
	if sl.Metadata != nil {
		m, err := json.Marshal(sl.Metadata)
		if err != nil {
			return nil, nil, err
		}
		if len(m) > metadataMax {
			return nil, nil, fmt.Errorf("project: sealed metadata is %d bytes, over Signal's %d", len(m), metadataMax)
		}
	}
	if b.Metadata != nil {
		m, err := json.Marshal(b.Metadata)
		if err != nil {
			return nil, nil, err
		}
		if len(m) > metadataMax {
			return nil, nil, fmt.Errorf("project: metadata is %d bytes, over Signal's %d", len(m), metadataMax)
		}
	}
	data, err := Marshal(b)
	if err != nil {
		return nil, nil, err
	}
	if len(data) > sealedRequestMax {
		return nil, nil, fmt.Errorf("project: request is %d bytes, over Signal's %d", len(data), sealedRequestMax)
	}
	return b, data, nil
}

// fitSealed cuts a body that would take the sealed parts over Signal's limits, and marks them truncated, as Signal
// DEC-262 asks of a long transcript. Without a body to cut, parts over the limit are an error.
func fitSealed(sl *Sealed) error {
	size := func() (int, error) {
		parts := *sl
		parts.Level = ""
		data, err := json.Marshal(parts)
		return len(data), err
	}
	n, err := size()
	if err != nil {
		return err
	}
	if n <= sealedPartsMax && len(sl.Body) <= bodyMaxBytes {
		return nil
	}
	if sl.Body == "" {
		return fmt.Errorf("project: the sealed parts are %d bytes, over Signal's %d", n, sealedPartsMax)
	}
	if sl.Metadata == nil {
		sl.Metadata = map[string]any{}
	}
	sl.Metadata["truncated"] = true
	full := sl.Body
	lo, hi := 0, len(full)
	if hi > bodyMaxBytes {
		hi = bodyMaxBytes
	}
	// The longest prefix, cut on a character boundary, that fits.
	for lo < hi {
		mid := (lo + hi + 1) / 2
		sl.Body = cut(full, mid)
		if n, err = size(); err != nil {
			return err
		}
		if n <= sealedPartsMax {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	sl.Body = cut(full, lo)
	if n, err = size(); err != nil {
		return err
	}
	if n > sealedPartsMax {
		return fmt.Errorf("project: the sealed parts are %d bytes, over Signal's %d", n, sealedPartsMax)
	}
	return nil
}

// cut is the first n bytes of s, shortened to a character boundary.
func cut(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

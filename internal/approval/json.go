// Package approval implements Beacon's local journal and Signal's approval record contract.
package approval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

const maxSafeInteger = 1<<53 - 1

// Object holds only JSON values. ParseObject represents numbers as int64, nested objects as Object.
type Object map[string]any

var errJSON = errors.New("invalid contract JSON")

func ParseObject(b []byte) (Object, error) {
	v, err := parseJSON(b)
	if o, ok := v.(Object); err == nil && ok {
		return o, nil
	}
	return nil, errJSON
}

func (o *Object) UnmarshalJSON(b []byte) error {
	parsed, err := ParseObject(b)
	if err == nil {
		*o = parsed
	}
	return err
}

func parseJSON(b []byte) (any, error) {
	if !utf8.Valid(b) || !pairedSurrogates(b) {
		return nil, errJSON
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := readValue(d, 0)
	if err != nil {
		return nil, errJSON
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errJSON
	}
	return v, nil
}

// encoding/json replaces lone UTF-16 surrogates instead of refusing them. Check escapes in the original bytes;
// the decoder still checks the syntax and reports malformed escape sequences.
func pairedSurrogates(b []byte) bool {
	for i := 0; i+1 < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		i++
		if b[i] != 'u' || i+4 >= len(b) {
			continue
		}
		r, _ := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
		i += 4
		if r >= 0xdc00 && r <= 0xdfff {
			return false
		}
		if r >= 0xd800 && r <= 0xdbff {
			if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
				return false
			}
			low, _ := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
			if low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func readValue(d *json.Decoder, depth int) (any, error) {
	if depth == 64 {
		return nil, errJSON
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := Object{}
		for d.More() {
			k, err := d.Token()
			key, ok := k.(string)
			if err != nil || !ok {
				return nil, errJSON
			}
			if _, exists := o[key]; exists {
				return nil, errJSON
			}
			v, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			o[key] = v
		}
		end, err := d.Token()
		if err == nil && end == json.Delim('}') {
			return o, nil
		}
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := readValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		end, err := d.Token()
		if err == nil && end == json.Delim(']') {
			return a, nil
		}
	default:
		switch v := t.(type) {
		case json.Number:
			n, err := v.Int64()
			if err == nil && n >= -maxSafeInteger && n <= maxSafeInteger {
				return n, nil
			}
		case string, bool, nil:
			return v, nil
		}
	}
	return nil, errJSON
}

// Canonical uses Signal's JSON.stringify string escaping and UTF-16 key order. It accepts values from
// ParseObject (no floats, no invalid Unicode), which can differ from encoding/json's output.
func Canonical(v any) ([]byte, error) { return appendJSON(nil, v, 0) }

func appendJSON(b []byte, v any, depth int) ([]byte, error) {
	if depth == 64 {
		return nil, errJSON
	}
	switch v := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		return strconv.AppendBool(b, v), nil
	case int64:
		if v >= -maxSafeInteger && v <= maxSafeInteger {
			return strconv.AppendInt(b, v, 10), nil
		}
	case string:
		if utf8.ValidString(v) {
			return quote(b, v), nil
		}
	case Object:
		type key struct {
			s string
			u []uint16
		}
		keys := make([]key, 0, len(v))
		for k := range v {
			if !utf8.ValidString(k) {
				return nil, errJSON
			}
			keys = append(keys, key{k, utf16.Encode([]rune(k))})
		}
		slices.SortFunc(keys, func(a, b key) int { return slices.Compare(a.u, b.u) })
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(quote(b, k.s), ':')
			var err error
			b, err = appendJSON(b, v[k.s], depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	case []any:
		b = append(b, '[')
		for i, item := range v {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			b, err = appendJSON(b, item, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	}
	return nil, errJSON
}

func quote(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b = append(b, '\\', c)
		case '\b':
			b = append(b, `\b`...)
		case '\f':
			b = append(b, `\f`...)
		case '\n':
			b = append(b, `\n`...)
		case '\r':
			b = append(b, `\r`...)
		case '\t':
			b = append(b, `\t`...)
		default:
			if c < 0x20 {
				const digits = "0123456789abcdef"
				b = append(b, '\\', 'u', '0', '0', digits[c>>4], digits[c&15])
			} else {
				b = append(b, c)
			}
		}
	}
	return append(b, '"')
}

func digest(v Object) (string, error) {
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	d := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(d[:]), nil
}

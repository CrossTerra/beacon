package webauthn

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"unicode/utf8"
)

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin *bool  `json:"crossOrigin"`
}

// parseClientData checks type, challenge, origin and crossOrigin.
// Extra fields are ignored: Chrome adds a hint that the JSON must not be
// compared as a template. A repeated key is refused, because encoding/json
// would keep the last origin and hide the first.
func parseClientData(raw []byte, ceremony Ceremony, origin string, challenge []byte) (clientData, error) {
	if !utf8.Valid(raw) || jsonHasDuplicateKeys(raw) {
		return clientData{}, errCode(CodeClientData)
	}
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return clientData{}, errCode(CodeClientData)
	}
	if cd.Type != string(ceremony) {
		return clientData{}, errCode(CodeType)
	}
	if cd.CrossOrigin != nil && *cd.CrossOrigin {
		return clientData{}, errCode(CodeCrossOrigin)
	}
	// Exact match. go-webauthn folds case and strips a path; D4 does not.
	if cd.Origin != origin {
		return clientData{}, errCode(CodeOrigin)
	}
	got, err := base64.RawURLEncoding.DecodeString(cd.Challenge)
	if err != nil || len(got) == 0 || len(challenge) == 0 {
		return clientData{}, errCode(CodeChallenge)
	}
	if len(got) != len(challenge) || subtle.ConstantTimeCompare(got, challenge) != 1 {
		return clientData{}, errCode(CodeChallenge)
	}
	if base64.RawURLEncoding.EncodeToString(got) != cd.Challenge {
		return clientData{}, errCode(CodeChallenge)
	}
	return cd, nil
}

// jsonHasDuplicateKeys reports whether raw is not one JSON value, or an object
// in it repeats a key.
func jsonHasDuplicateKeys(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dup, err := walkJSON(dec)
	if err != nil || dup {
		return true
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return true
	}
	return false
}

func walkJSON(dec *json.Decoder) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	switch tok {
	case json.Delim('{'):
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return false, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return false, errCode(CodeClientData)
			}
			if _, ok := seen[key]; ok {
				return true, nil
			}
			seen[key] = struct{}{}
			dup, err := walkJSON(dec)
			if err != nil || dup {
				return dup, err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return false, err
		}
		if end != json.Delim('}') {
			return false, errCode(CodeClientData)
		}
		return false, nil
	case json.Delim('['):
		for dec.More() {
			dup, err := walkJSON(dec)
			if err != nil || dup {
				return dup, err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return false, err
		}
		if end != json.Delim(']') {
			return false, errCode(CodeClientData)
		}
		return false, nil
	default:
		return false, nil
	}
}

package webauthn

import "github.com/fxamacker/cbor/v2"

// dec rejects duplicate keys, tags and inputs that nest or fan out past a
// credential. Indefinite lengths are allowed because the size limits already
// bound the input; authenticators use definite lengths in practice.
var dec cbor.DecMode

func init() {
	var err error
	dec, err = cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		TimeTag:           cbor.DecTagIgnored,
		MaxNestedLevels:   8,
		MaxArrayElements:  32,
		MaxMapPairs:       32,
		IndefLength:       cbor.IndefLengthAllowed,
		TagsMd:            cbor.TagsForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorNone,
		UTF8:              cbor.UTF8RejectInvalid,
		IntDec:            cbor.IntDecConvertNone,
	}.DecMode()
	if err != nil {
		panic("webauthn: cbor options: " + err.Error())
	}
}

// unmarshalOne decodes one CBOR item and returns the unread tail.
func unmarshalOne(data []byte, v any) ([]byte, error) {
	return dec.UnmarshalFirst(data, v)
}

package source

import (
	"testing"
	"time"
)

func TestSiblings(t *testing.T) {
	s := "slack:dm:winton:D1:1700000000.000100"
	for other, want := range map[string]bool{
		"slack:dm:winton:D1:1700000000.000200": true,
		s:                                      true,
		"slack:dm:winton:D2:1700000000.000200": false,
		"slack:dm:winton:D1:x:y":               false,
		"slack:dm:winton:D1":                   false,
	} {
		if IsSibling(s, other) != want {
			t.Errorf("%s: %v", other, !want)
		}
	}
	if IsSibling("nocolon", "nocolon") {
		t.Fatal("a subject with no colon has no siblings")
	}
}

func TestCloseSiblingsValidation(t *testing.T) {
	it := Item{Source: "slack", Subject: "slack:dm:winton:D1:1700000000.000100", Key: "slack:D1:1700000000.000100",
		Type: "own_message", OccurredAt: time.Unix(1700000000, 0), Own: true, CloseSiblings: true}
	if err := Validate(it, nil, nil); err != nil {
		t.Fatal(err)
	}
	notOwn := it
	notOwn.Own = false
	wide := it
	wide.Subject = "slack:dm:winton:1700000000.000100"
	channel := it
	channel.Subject = "slack:C1:1700000000.000100"
	for name, bad := range map[string]Item{"not own": notOwn, "person-wide": wide, "channel": channel} {
		if Validate(bad, nil, nil) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

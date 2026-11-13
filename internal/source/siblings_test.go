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

func TestClosesSibling(t *testing.T) {
	reply := "slack:dm:winton:D1:1700000300.000000"
	before := "1700000300.000000"
	for _, tc := range []struct {
		subject, key string
		want         bool
	}{
		{"slack:dm:winton:D1:1700000100.000000", "slack:D1:1700000100.000000", true},
		// Posted after the reply, though decided earlier (late indexing).
		{"slack:dm:winton:D1:1700000400.000000", "slack:D1:1700000400.000000", false},
		// A question inside a thread: its subject is the root's, and its key is not the root.
		{"slack:dm:winton:D1:1700000100.000000", "slack:D1:1700000200.000000", false},
		// Another conversation.
		{"slack:dm:winton:D2:1700000100.000000", "slack:D2:1700000100.000000", false},
		// Its own subject is not a sibling here; the ordinary rule covers it.
		{reply, "slack:D1:1700000300.000000", false},
	} {
		if got := ClosesSibling(reply, before, tc.subject, tc.key); got != tc.want {
			t.Errorf("%s %s: %v", tc.subject, tc.key, got)
		}
	}
	if ClosesSibling(reply, "", "slack:dm:winton:D1:1700000100.000000", "slack:D1:1700000100.000000") {
		t.Fatal("no bound closes")
	}
}

func TestDecimalBefore(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1700000100.000000", "1700000300.000000", true},
		{"999999999.999999", "1000000000.000000", true},
		{"1700000300.000000", "1700000300.000000", false},
		{"1700000300.00001", "1700000300.000002", false},
		{"1700000300.1", "1700000300.10", false},
		{"x", "1", false},
		{"1", "", false},
	} {
		if got := DecimalBefore(tc.a, tc.b); got != tc.want {
			t.Errorf("%s < %s: %v", tc.a, tc.b, got)
		}
	}
}

func TestCloseSiblingsValidation(t *testing.T) {
	it := Item{Source: "slack", Subject: "slack:dm:winton:D1:1700000000.000100", Key: "slack:D1:1700000000.000100",
		Type: "own_message", OccurredAt: time.Unix(1700000000, 0), Own: true, CloseSiblingsBefore: "1700000000.000100"}
	if err := Validate(it, nil, nil); err != nil {
		t.Fatal(err)
	}
	notOwn := it
	notOwn.Own = false
	wide := it
	wide.Subject = "slack:dm:winton:1700000000.000100"
	channel := it
	channel.Subject = "slack:C1:1700000000.000100"
	bad := it
	bad.CloseSiblingsBefore = "later"
	for name, b := range map[string]Item{"not own": notOwn, "person-wide": wide, "channel": channel, "bad bound": bad} {
		if Validate(b, nil, nil) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCloseOnlyValidation(t *testing.T) {
	it := Item{Source: "slack", Subject: "slack:C1:1700000000.000100", Key: "slack:C1:1700000000.000200:deleted",
		Type: "message_deleted", OccurredAt: time.Unix(1700000000, 0), Version: "deleted", CloseOnly: []string{"slack:C1:1700000000.000200"}}
	if err := Validate(it, nil, nil); err != nil {
		t.Fatal(err)
	}
	other := it
	other.CloseOnly = []string{"gmail:t1:m1"}
	// An own act may name the asks it answers on its subject and close earlier siblings too.
	both := it
	both.Own, both.Subject, both.CloseSiblingsBefore = true, "slack:dm:winton:D1:2026-11-12", "1700000000.000100"
	if err := Validate(both, nil, nil); err != nil {
		t.Fatalf("own act with both: %v", err)
	}
	for name, b := range map[string]Item{"another source's key": other} {
		if Validate(b, nil, nil) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

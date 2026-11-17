package source

import "testing"

func TestActNative(t *testing.T) {
	if got := ActNative("github", "merge", "abc123"); got != "github:merge:abc123" {
		t.Fatalf("%q", got)
	}
	if got := ActNative("slack", "message", "C0ENG:1794133800.000400"); got != "slack:message:C0ENG:1794133800.000400" {
		t.Fatalf("%q", got)
	}
	for _, id := range []string{"", "a b", "a@b", "has\nline"} {
		if got := ActNative("gmail", "message", id); got != "" {
			t.Fatalf("id %q left as %q", id, got)
		}
	}
	if got := ActNative("GitHub", "comment", "1"); got != "" {
		t.Fatalf("uppercase source matched %q", got)
	}
	long := make([]byte, 201)
	for i := range long {
		long[i] = 'a'
	}
	if got := ActNative("github", "comment", string(long)); got != "" {
		t.Fatal("over 200 characters was kept")
	}
}

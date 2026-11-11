package keychain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// counting wraps Memory and counts reads of each account, the operation that prompts on the login keychain.
type counting struct {
	*Memory
	reads map[string]int
	// failSet fails the next Set of this account, once.
	failSet string
}

func (c *counting) Get(account string) (Secret, error) {
	c.reads[account]++
	return c.Memory.Get(account)
}

func (c *counting) Set(account string, s Secret) error {
	if account == c.failSet {
		c.failSet = ""
		return errors.New("write refused")
	}
	return c.Memory.Set(account, s)
}

func newCounting() *counting {
	m := NewMemory()
	m.Put("signal:signal.example:443", "sig_one")
	m.Put("source:github", "gho_two")
	m.Put("google:me@example.com:refresh", "1//three")
	return &counting{Memory: m, reads: map[string]int{}}
}

func TestRepairRecreatesEachItemReadingItOnce(t *testing.T) {
	s := newCounting()
	var done []string
	if err := Repair(s, func(a string) { done = append(done, a) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"google:me@example.com:refresh", "signal:signal.example:443", "source:github"}
	if !reflect.DeepEqual(done, want) {
		t.Fatalf("done %v", done)
	}
	accounts, _ := s.Accounts()
	if !reflect.DeepEqual(accounts, want) {
		t.Fatalf("left %v; a repair copy must not remain", accounts)
	}
	if !s.Contains("signal:signal.example:443", "sig_one") || !s.Contains("source:github", "gho_two") || !s.Contains("google:me@example.com:refresh", "1//three") {
		t.Fatal("a value changed")
	}
	// One read of the original (the prompt) and one of the re-created item, which this build made and which
	// therefore does not prompt. The copy is never read back when the repair succeeds.
	for _, a := range want {
		if s.reads[a] != 2 || s.reads[repairPrefix+a] != 0 {
			t.Fatalf("%s read %d times, copy %d", a, s.reads[a], s.reads[repairPrefix+a])
		}
	}
}

func TestRepairKeepsACopyWhenTheWriteBackFails(t *testing.T) {
	s := newCounting()
	s.failSet = "source:github"
	err := Repair(s, nil)
	if err == nil || !strings.Contains(err.Error(), "beacon/repair:source:github") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "gho_two") {
		t.Fatal("the error holds the value")
	}
	if _, err := s.Get("source:github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("item %v", err)
	}
	if !s.Contains(repairPrefix+"source:github", "gho_two") {
		t.Fatal("the copy is gone")
	}
	// The next repair restores the item from the copy, repairs it, and removes the copy.
	if err := Repair(s, nil); err != nil {
		t.Fatal(err)
	}
	if !s.Contains("source:github", "gho_two") {
		t.Fatal("not restored")
	}
	if _, err := s.Get(repairPrefix + "source:github"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the copy remains")
	}
}

func TestRepairDropsACopyWhoseItemExists(t *testing.T) {
	s := newCounting()
	// Cut short after the item was written back and before the copy was removed.
	s.Put(repairPrefix+"source:github", "gho_two")
	if err := Repair(s, nil); err != nil {
		t.Fatal(err)
	}
	accounts, _ := s.Accounts()
	for _, a := range accounts {
		if strings.HasPrefix(a, repairPrefix) {
			t.Fatalf("copy %s remains", a)
		}
	}
	if !s.Contains("source:github", "gho_two") {
		t.Fatal("value changed")
	}
}

type unlisted struct{ Store }

func TestRepairNeedsAList(t *testing.T) {
	if err := Repair(unlisted{NewMemory()}, nil); err == nil {
		t.Fatal("repaired a store that cannot list its items")
	}
}

func TestRepairEmpty(t *testing.T) {
	called := false
	if err := Repair(NewMemory(), func(string) { called = true }); err != nil || called {
		t.Fatalf("%v %v", err, called)
	}
}

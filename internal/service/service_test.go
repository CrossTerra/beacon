package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotateAppendDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StdoutName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(bytesRepeat('a', 100)); err != nil {
		t.Fatal(err)
	}
	if err := Rotate(path, 50); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil || string(backup) != string(bytesRepeat('a', 100)) {
		t.Fatalf("backup %q err %v", backup, err)
	}
	if _, err := f.Write([]byte("next")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "next" {
		t.Fatalf("after truncate, append wrote %q", got)
	}
	info, err := os.Stat(path + ".1")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v err %v", info, err)
	}
	if err := Rotate(path, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal(err)
	}
}

func TestRotateSkipsSmallAndHuge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.log")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rotate(path, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("small file was rotated")
	}
	if err := Rotate(filepath.Join(dir, "missing.log"), 1); err != nil {
		t.Fatal(err)
	}
}

func TestPassRoundTrip(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC)
	in := Pass{At: at, Outcome: "ok", Lease: LeaseHeld, Holder: "0123456789abcdef"}
	if err := WritePass(dir, in); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, PassName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", info, err)
	}
	got, err := ReadPass(dir)
	if err != nil || !got.At.Equal(at) || got.Outcome != "ok" || got.Lease != LeaseHeld || got.Holder != in.Holder {
		t.Fatalf("%+v %v", got, err)
	}
	if err := WritePass(dir, Pass{At: at, Outcome: "ok\nkey", Lease: LeaseHeld, Holder: in.Holder}); err == nil {
		t.Fatal("outcome with a newline was written")
	}
	if err := os.WriteFile(filepath.Join(dir, PassName), []byte("{\"at\":\"2026-11-10T15:04:05Z\",\"outcome\":\"ok\\nfixture:chat:1\",\"lease\":\"held\",\"holder\":\"not-an-id\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPass(dir); err != ErrPass || strings.Contains(err.Error(), "fixture") {
		t.Fatalf("err %v", err)
	}
}

func TestFormatPass(t *testing.T) {
	at := time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC)
	got := FormatPass(Pass{At: at, Outcome: "ok", Lease: LeaseHeld, Holder: "0123456789abcdef"}, nil)
	want := "last_pass: 2026-11-10T15:04:05Z\noutcome: ok\nlease: held\nholder: 0123456789abcdef\n"
	if got != want {
		t.Fatalf("%q", got)
	}
	other := FormatPass(Pass{At: at, Outcome: "ok", Lease: LeaseOther}, nil)
	if !strings.Contains(other, "holder: another instance\n") || strings.Contains(other, "0123456789abcdef") {
		t.Fatalf("%s", other)
	}
	bad := FormatPass(Pass{}, ErrPass)
	if strings.Contains(bad, "fixture") || !strings.Contains(bad, "last_pass: unreadable\n") {
		t.Fatalf("%s", bad)
	}
	none := FormatPass(Pass{}, os.ErrNotExist)
	if !strings.Contains(none, "last_pass: none\n") {
		t.Fatalf("%s", none)
	}
}

func TestBackoff(t *testing.T) {
	poll := time.Minute
	if Backoff(poll, 1) != poll || Backoff(poll, 2) != 2*poll || Backoff(poll, 3) != 4*poll {
		t.Fatalf("backoff %s %s %s", Backoff(poll, 1), Backoff(poll, 2), Backoff(poll, 3))
	}
	if Backoff(poll, 20) != BackoffCap || Backoff(0, 1) != time.Minute {
		t.Fatalf("cap %s default %s", Backoff(poll, 20), Backoff(0, 1))
	}
	if NextFailures(Pass{}, false) != 1 || NextFailures(Pass{Outcome: "ok"}, true) != 1 || NextFailures(Pass{Outcome: "unauthorized", Failures: 3}, true) != 4 {
		t.Fatal("streak")
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestPassHealthFields(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC)
	until := at.Add(45 * time.Minute)
	p := Pass{At: at, Outcome: "ok", Lease: LeaseHeld, Holder: "0123456789abcdef", LeaseUntil: &until, SendReason: "unavailable"}
	if err := WritePass(dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPass(dir)
	if err != nil || got.LeaseUntil == nil || !got.LeaseUntil.Equal(until) || got.SendReason != "unavailable" {
		t.Fatalf("%v %+v", err, got)
	}
	for _, bad := range []Pass{
		{At: at, Outcome: "ok", Lease: LeaseOther, LeaseUntil: &until},
		{At: at, Outcome: "ok", LeaseReason: "Signal said no"},
		{At: at, Outcome: "ok", SendReason: "x\ny"},
	} {
		if err := WritePass(dir, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestPassWithheld(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 10, 15, 4, 5, 0, time.UTC)
	p := Pass{At: at, Outcome: "ok", Withheld: []Withheld{
		{Source: "gmail", Prefix: "gmail:*", Reason: WithheldUnrestricted},
		{Source: "drive", Prefix: "drive:*", Reason: WithheldUnknown},
	}}
	if err := WritePass(dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPass(dir)
	if err != nil || len(got.Withheld) != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	out := FormatPass(got, nil)
	if !strings.Contains(out, "withheld: gmail gmail:* prefix_unrestricted\n") || !strings.Contains(out, "withheld: drive drive:* restrictions_unknown\n") {
		t.Fatalf("%s", out)
	}
	for _, bad := range []Withheld{
		{Source: "gmail", Prefix: "gmail:*", Reason: "Budget review"},
		{Source: "gmail", Prefix: "gmail:winton thread", Reason: WithheldUnknown},
		{Source: "Gmail!", Prefix: "gmail:*", Reason: WithheldUnknown},
	} {
		if err := WritePass(dir, Pass{At: at, Outcome: "ok", Withheld: []Withheld{bad}}); err == nil {
			t.Errorf("wrote %+v", bad)
		}
	}
}

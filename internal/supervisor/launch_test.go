package supervisor

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSilentBootstrapDeathAfterPermitIsNotExec(t *testing.T) {
	gateR, gateW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gateR.Close()
	defer gateW.Close()
	statusR, statusW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusR.Close()
	defer statusW.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyR.Close()
	defer readyW.Close()
	// This stand-in models an interrupted bootstrap: read the entire permit, then
	// die without exec or an error write. The kernel still closes its status fd.
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", `printf '\001' >&6; exec 6>&-; while IFS= read -r line <&3; do :; done; kill -KILL $$`}, &os.ProcAttr{
		Env: []string{}, Files: []*os.File{nil, nil, nil, gateR, statusW, nil, readyW},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Kill(); _, _ = p.Wait() }()
	gateR.Close()
	statusW.Close()
	readyW.Close()
	j := job{pid: p.Pid, gate: gateW, payload: []byte(`{"permit":true}`), execStatus: statusR, ready: readyR}
	err = j.release(context.Background(), time.Time{})
	if j.events != nil {
		j.events.close()
	}
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("silent bootstrap death reported as %v, want unconfirmed", err)
	}
}

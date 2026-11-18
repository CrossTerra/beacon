package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/CrossTerra/beacon/internal/source"
)

// namedSource is a fake source under another name, for a pass of one source among several.
type namedSource struct {
	*fakeSource
	name string
}

func (n namedSource) Name() string { return n.name }

// A pass of one source, gathered on its own, decides and sends that source's items, commits only that source's cursor,
// and says what its collect came to. Another source is not collected.
func TestProcessOneSource(t *testing.T) {
	f := newFixture(t)
	f.src.next = []byte("fx-next")
	other := &fakeSource{err: errors.New("must not be collected")}
	r, done := f.runner(true, nil)
	defer done()
	r.Sources = []source.Source{f.src, namedSource{other, "other"}}
	cur := &memCursor{m: map[string][]byte{"other": []byte("other-cursor")}}
	r.Cursors = cur

	g := Gather(context.Background(), f.src, nil)
	rep, err := r.Process(context.Background(), []Gathered{g})
	if err != nil {
		t.Fatal(err)
	}
	if other.cursor != nil {
		t.Fatal("the other source was collected")
	}
	if got, _ := cur.Get("fx"); string(got) != "fx-next" {
		t.Fatalf("fx cursor %q", got)
	}
	if got, _ := cur.Get("other"); string(got) != "other-cursor" {
		t.Fatalf("other cursor %q", got)
	}
	if len(rep.Ran) != 1 || rep.Ran[0].Name != "fx" || rep.Ran[0].Failed != "" {
		t.Fatalf("ran %+v", rep.Ran)
	}
	if s, h, _, _ := rep.Counts(); s != 1 || h != 1 || len(rep.Results) != 1 {
		t.Fatalf("send %d hold %d results %d", s, h, len(rep.Results))
	}

	// A collect that failed is reported with its reason and moves no cursor.
	f.src.err = source.ErrUnauthorized
	f.src.next = []byte("not-this")
	rep, err = r.Process(context.Background(), []Gathered{Gather(context.Background(), f.src, []byte("fx-next"))})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Ran) != 1 || rep.Ran[0].Failed != "source_unauthorized" {
		t.Fatalf("ran %+v", rep.Ran)
	}
	if got, _ := cur.Get("fx"); string(got) != "fx-next" {
		t.Fatalf("fx cursor %q after a failure", got)
	}

	// A pass with nothing gathered still sends what is pending.
	rep, err = r.Process(context.Background(), nil)
	if err != nil || len(rep.Ran) != 0 || rep.Lease == nil || !rep.Lease.Held {
		t.Fatalf("%+v %v", rep, err)
	}
}

package approval

import (
	"bytes"
	"errors"
)

type deliveryHead struct {
	bytes     []byte
	inFlight  bool
	ambiguous bool // Signal may have stored exactly these bytes
	stopped   string
}

type DeliveryStatus struct {
	Delivered int64
	Waiting   int64
	Stopped   string
}

func (j *Journal) DeliveryStatus() DeliveryStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return DeliveryStatus{j.delivered, j.lastSeq - j.delivered, j.delivery.stopped}
}

// beginDelivery syncs both the bytes and the in-flight marker before the network is touched. If a process
// dies between the POST and committing its answer, replay cannot mistake the send for a definite refusal.
func (j *Journal) beginDelivery(who SignalIdentity) ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.healthy() {
		return nil, ErrJournalIO
	}
	if len(j.queue) == 0 || j.delivery.stopped != "" {
		return nil, nil
	}
	e := j.queue[0]
	attempt := frame{Kind: "sending", Seq: e.Seq}
	if j.delivery.bytes == nil {
		b, err := record(j.id, e, j.states[e.ID], who)
		if err != nil {
			reason := "record:unreadable"
			var field Invalid
			if errors.As(err, &field) {
				reason = "record:" + string(field)
			}
			return nil, j.commit(frame{Kind: "stop", Seq: e.Seq, Reason: reason})
		}
		attempt.Bytes = b
	}
	if err := j.commit(attempt); err != nil {
		return nil, err
	}
	return bytes.Clone(j.delivery.bytes), nil
}

func (j *Journal) endDelivery(kind, reason string, ambiguous bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.queue) == 0 || !j.delivery.inFlight {
		return ErrTransition
	}
	return j.commit(frame{Kind: kind, Seq: j.queue[0].Seq, Reason: reason, Ambiguous: ambiguous})
}

// ResumeDelivery retries the stopped head. Definite refusals can be rebuilt for corrected Signal settings;
// a record that could have been stored keeps its original exact bytes and key.
func (j *Journal) ResumeDelivery() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.healthy() {
		return ErrJournalIO
	}
	if j.delivery.stopped == "" {
		return nil
	}
	return j.commit(frame{Kind: "resume", Seq: j.queue[0].Seq})
}

func (j *Journal) foldDelivery(e frame) bool {
	if len(j.queue) == 0 || e.Seq != j.queue[0].Seq {
		return false
	}
	h := &j.delivery
	switch e.Kind {
	case "sending":
		if h.stopped != "" || (len(e.Bytes) > 0) == (len(h.bytes) > 0) || len(e.Bytes) > maxRecord {
			return false
		}
		if len(e.Bytes) > 0 {
			h.bytes = e.Bytes
		}
		h.ambiguous = h.ambiguous || h.inFlight
		h.inFlight = true
	case "ack":
		if !h.inFlight || !ValidID(e.Reason, "evt") {
			return false
		}
		j.queue[0] = nil
		j.queue = j.queue[1:]
		j.delivered++
		*h = deliveryHead{}
	case "retry":
		if !h.inFlight {
			return false
		}
		h.inFlight, h.ambiguous = false, h.ambiguous || e.Ambiguous
	case "stop":
		if e.Reason == "" || h.stopped != "" {
			return false
		}
		h.inFlight, h.stopped = false, e.Reason
		h.ambiguous = h.ambiguous || e.Ambiguous
	case "resume":
		if h.stopped == "" {
			return false
		}
		h.stopped = ""
		if !h.ambiguous {
			h.bytes = nil
		}
	default:
		return false
	}
	return true
}

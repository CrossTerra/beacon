package approvalserve

import (
	"testing"

	"github.com/CrossTerra/beacon/internal/approval"
)

func TestNoHandlerDoesNotStart(t *testing.T) {
	if err := Dispatch(nil, approval.Frozen{}); err != ErrNoHandler {
		t.Fatal(err)
	}
}

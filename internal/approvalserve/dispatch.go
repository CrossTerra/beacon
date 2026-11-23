package approvalserve

import (
	"context"
	"errors"

	"github.com/CrossTerra/beacon/internal/approval"
)

// ErrNoHandler means the approval is recorded and must not be started.
var ErrNoHandler = errors.New("no Go handler is wired for this action")

// Handler would perform one frozen action. github.merge, slack.post, and agent.run
// exist in Swift only. The preview passes nil and does not call Journal.Start.
type Handler interface {
	Run(context.Context, approval.Frozen) error
}

// Dispatch is the only path from a recorded approval to a provider write.
// A nil handler returns ErrNoHandler and starts nothing.
func Dispatch(h Handler, frozen approval.Frozen) error {
	if h == nil {
		return ErrNoHandler
	}
	return h.Run(context.Background(), frozen)
}

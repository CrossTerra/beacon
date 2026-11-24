package approvalserve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

func noticeLoop(ctx context.Context, changes <-chan struct{}, send func(context.Context, string), j *approval.Journal, now func() time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
			pending, err := j.Pending(now())
			if err != nil {
				return err
			}
			if message := noticeText(pending); message != "" {
				attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
				send(attempt, message)
				cancel()
			}
		}
	}
}

func noticeText(pending []approval.Proposal) string {
	counts := map[approval.Action]int{}
	for _, p := range pending {
		counts[p.Type]++
	}
	var words []string
	for _, kind := range []approval.Action{approval.Merge, approval.Post, approval.Run} {
		if n := counts[kind]; n != 0 {
			words = append(words, fmt.Sprintf("%d %s", n, kind))
		}
	}
	if len(words) == 0 {
		return ""
	}
	return "Pending: " + strings.Join(words, ", ")
}

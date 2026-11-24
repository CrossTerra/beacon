package notice

import (
	"context"
	"os/exec"
	"strconv"
)

func Send(ctx context.Context, message string) {
	// Called only with a count and fixed action names. The notification takes no proposal text and cannot approve.
	_ = exec.CommandContext(ctx, "/usr/bin/osascript", "-e", "display notification "+strconv.Quote(message)+" with title \"Beacon\"").Run()
}

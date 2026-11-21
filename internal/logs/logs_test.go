package logs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSignalCode(t *testing.T) {
	for raw, want := range map[string]string{
		"idempotency_conflict": "idempotency_conflict",
		"private_item_content": "other",
		"arbitrary reply text": "other",
		"":                     "",
	} {
		var output strings.Builder
		New(&output).Log(SendResult, Code(raw))
		var entry struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(output.String()), &entry); err != nil || entry.Code != want {
			t.Errorf("code %q: got %q, want %q (parse error: %v)", raw, entry.Code, want, err)
		}
	}
}

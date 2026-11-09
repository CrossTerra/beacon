package jira

import "strings"

// adfText returns the plain text of an Atlassian document and the account ids it mentions.
func adfText(v any) (string, map[string]bool) {
	var b strings.Builder
	mentions := map[string]bool{}
	walk(v, &b, mentions, false)
	return strings.TrimSpace(b.String()), mentions
}

func walk(v any, b *strings.Builder, mentions map[string]bool, after bool) bool {
	switch n := v.(type) {
	case map[string]any:
		if n["type"] == "mention" {
			if attrs, ok := n["attrs"].(map[string]any); ok {
				if id, ok := attrs["id"].(string); ok && id != "" {
					mentions[id] = true
				}
				if text, ok := attrs["text"].(string); ok && text != "" {
					b.WriteString(text)
					return true
				}
			}
		}
		if text, ok := n["text"].(string); ok {
			b.WriteString(text)
			after = true
		}
		if content, ok := n["content"].([]any); ok {
			block := n["type"] == "paragraph" || n["type"] == "heading"
			for _, child := range content {
				after = walk(child, b, mentions, after)
			}
			if block && after {
				b.WriteByte('\n')
			}
		}
		return after || b.Len() > 0
	case []any:
		for _, child := range n {
			after = walk(child, b, mentions, after)
		}
		return after
	default:
		return after
	}
}

// mentioned reports whether text names the account, in ADF mention form or wiki markup.
func mentioned(text, account string) bool {
	if text == "" || account == "" {
		return false
	}
	return strings.Contains(text, "[~accountid:"+account+"]") || strings.Contains(text, account)
}

package review

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/CrossTerra/beacon/internal/approval"
)

type slackPreview struct {
	Plain, Source string
	Special       bool
}

func slackDraft(a approval.Action, params approval.Object) *slackPreview {
	if a != approval.Post {
		return nil
	}
	s, _ := params["text"].(string)
	return &slackPreview{Plain: slackPlain(s), Source: s, Special: strings.Contains(s, "<!")}
}

// slackPlain is a non-interactive rendering, never the payload. Resolve no links or names, and do not trust
// the draft's labels for mentions. The frozen text is shown separately, open by default on special mentions.
func slackPlain(s string) string {
	var plain strings.Builder
	end := 0
	for _, m := range slackSpans.FindAllStringIndex(s, -1) {
		i, j := m[0], m[1]
		plain.WriteString(slackEntities.Replace(s[end:i]))
		span := s[i:j]
		switch {
		case strings.HasPrefix(span, "```"):
			plain.WriteString(slackEntities.Replace(span[3 : len(span)-3]))
		case strings.HasPrefix(span, "`"):
			plain.WriteString(slackEntities.Replace(span[1 : len(span)-1]))
		case strings.HasPrefix(span, "<"):
			plain.WriteString(slackAngle(span))
		default:
			before, _ := utf8.DecodeLastRuneInString(s[:i])
			after, _ := utf8.DecodeRuneInString(s[j:])
			if !wordRune(before) && !wordRune(after) {
				inner := span[1 : len(span)-1]
				if strings.TrimSpace(inner) == inner {
					span = inner
				}
			}
			plain.WriteString(slackEntities.Replace(span))
		}
		end = j
	}
	plain.WriteString(slackEntities.Replace(s[end:]))
	return plain.String()
}

var (
	slackSpans = regexp.MustCompile("(?s:```.*?```)|`[^`\r\n]*`|<[^<>\r\n]+>|" +
		"\\*[^*_~`<>\r\n]+\\*|_[^*_~`<>\r\n]+_|~[^*_~`<>\r\n]+~")
	// Slack unescapes only these three entities, and only once.
	slackEntities = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

func slackAngle(span string) string {
	target, label, labeled := strings.Cut(span[1:len(span)-1], "|")
	switch target {
	case "!here":
		return "[@here — notifies everyone present]"
	case "!channel":
		return "[@channel — notifies everyone in the channel]"
	case "!everyone":
		return "[@everyone — notifies everyone in the workspace]"
	}
	switch {
	case strings.HasPrefix(target, "!subteam^"):
		return "[@" + strings.TrimPrefix(target, "!subteam^") + " — notifies a user group]"
	case strings.HasPrefix(target, "@") || strings.HasPrefix(target, "#"):
		return target
	case strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "mailto:"):
		if labeled {
			return slackEntities.Replace(label + " (" + target + ")")
		}
		return slackEntities.Replace(target)
	default:
		return slackEntities.Replace(span)
	}
}

func proposalTitle(p approval.Proposal) string {
	if p.Type == approval.Post && p.Slack != nil {
		return "Post in Slack " + p.Slack.Conversation + " · " + p.Slack.Visibility.Label() + " · " + p.Slack.WorkspaceLabel
	}
	return actionName(p.Type)
}

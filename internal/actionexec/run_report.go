package actionexec

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	runOutputLimit = 64 << 20
	maxReportLine  = 4 << 20
	maxReportText  = 32_000
)

type runReport struct{ text, problem string }

func readRunReport(f *os.File, size int64) runReport {
	return parseRunReport(io.NewSectionReader(f, 0, size))
}

func parseRunReport(src io.Reader) runReport {
	type part struct{ id, text string }
	type message struct {
		parts []part
		index map[string]int
	}
	messages := make(map[string]*message)
	var last *message
	var report runReport
	r := bufio.NewReaderSize(src, 64<<10)
	var line []byte
	var skipping bool
	for {
		b, err := r.ReadSlice('\n')
		if len(line)+len(b) > maxReportLine {
			skipping = true
		}
		if !skipping {
			line = append(line, b...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !skipping && len(line) > 0 {
			var event struct {
				Type  string
				Part  struct{ ID, MessageID, Text string }
				Error struct {
					Name, Message string
					Data          struct{ Message string }
				}
			}
			if json.Unmarshal(line, &event) == nil {
				switch event.Type {
				case "text":
					p := event.Part
					if p.MessageID == "" || p.ID == "" {
						break
					}
					m := messages[p.MessageID]
					if m == nil {
						m = &message{index: make(map[string]int)}
						messages[p.MessageID] = m
					}
					if i, ok := m.index[p.ID]; ok {
						m.parts[i].text = p.Text
					} else {
						m.index[p.ID] = len(m.parts)
						m.parts = append(m.parts, part{p.ID, p.Text})
					}
					last = m
				case "error":
					report.problem = firstText(event.Error.Message, event.Error.Data.Message, event.Error.Name)
				}
			}
		}
		line, skipping = line[:0], false
		if err != nil {
			break
		}
	}
	if last != nil {
		var text strings.Builder
		for _, p := range last.parts {
			// The projected part emitted at idle continues the preceding text part.
			if text.Len() > 0 && !(strings.HasPrefix(p.id, "prt_") && strings.Contains(p.id, "_text-")) {
				text.WriteString("\n\n")
			}
			text.WriteString(p.text)
		}
		report.text = capReport(strings.TrimSpace(text.String()))
	}
	report.problem = capReport(report.problem)
	return report
}

func capReport(text string) string {
	n := utf8.RuneCountInString(text)
	if n <= maxReportText {
		return text
	}
	note := fmt.Sprintf("[Beacon kept the last part of a longer report: %d characters in all.]\n\n", n)
	return note + tailRunes(text, maxReportText-utf8.RuneCountInString(note))
}

func tailRunes(s string, n int) string {
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

func errorTail(f *os.File, size int64) string {
	start := max(0, size-(64<<10))
	b, _ := io.ReadAll(io.NewSectionReader(f, start, size-start))
	return tailRunes(strings.TrimSpace(strings.ToValidUTF8(string(b), "�")), 4000)
}

func firstText(values ...string) string {
	for _, s := range values {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

package jira

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// Fields is the source's fixed field list (DEC-6). from_status and to_status are set on a transition.
var Fields = []string{"project", "key", "summary", "url", "issue_type", "priority", "body", "from_status", "to_status"}

// StateFields is the source's fixed state field list (DEC-28).
var StateFields = []string{"status", "assignee", "resolution", "due", "priority"}

// Item types (DEC-18).
const (
	TypeAssigned     = "assigned"
	TypeUnassigned   = "unassigned"
	TypeTransitioned = "transitioned"
	TypeCommented    = "commented"
	TypeMentioned    = "mentioned"
	TypeDueChanged   = "due_changed"
)

type issue struct {
	key        string
	project    string
	summary    string
	url        string
	issueType  string
	priority   string
	status     string
	assignee   string
	account    string
	resolution string
	due        string
}

type change struct {
	field      string
	from       string
	to         string
	fromString string
	toString   string
}

type history struct {
	id      string
	author  string
	created time.Time
	items   []change
}

type comment struct {
	id       string
	author   string
	created  time.Time
	body     string
	mentions map[string]bool
}

type emit struct {
	typ        string
	id         string
	field      string
	actor      string
	at         time.Time
	addressed  bool
	body       string
	fromStatus string
	toStatus   string
	// detail is the assignee display name or the due date this change set. It is never comment text.
	detail string
}

// itemsFrom builds the items for one issue. A history with more than one relevant change suffixes the changelog
// id with the field, so two changes in one history stay distinct; a history with one change uses the changelog id
// alone, as the plan specifies.
func itemsFrom(name, person string, iss issue, histories []history, comments []comment, since time.Time, first bool, observed time.Time, loc *time.Location) ([]source.Item, error) {
	if first {
		return firstItems(name, person, iss, histories, observed, loc)
	}
	// No assignee history means the issue was assigned at creation and never reassigned: the current assignee
	// held it for every transition.
	assignee := initialAssignee(histories, iss.account)
	var pending []emit
	for _, h := range histories {
		var assigneeChange, statusChange, dueChange *change
		var mentions []change
		next := assignee
		for i := range h.items {
			it := &h.items[i]
			switch it.field {
			case "assignee":
				next = it.to
				assigneeChange = it
			case "status":
				statusChange = it
			case "duedate", "due":
				dueChange = it
			default:
				if mentionAdded(it.fromString, it.toString, person) || mentionAdded(it.from, it.to, person) {
					mentions = append(mentions, *it)
				}
			}
		}
		assignee = next
		if h.created.Before(since) {
			continue
		}
		if assigneeChange != nil {
			typ := TypeAssigned
			addressed := assigneeChange.to == person
			if assigneeChange.to == "" {
				typ = TypeUnassigned
				addressed = false
			}
			pending = append(pending, emit{
				typ: typ, id: h.id, field: "assignee", actor: h.author, at: h.created, addressed: addressed,
				detail: assigneeChange.toString,
			})
		}
		if statusChange != nil {
			addressed := assignee == person && h.author != person
			pending = append(pending, emit{
				typ: TypeTransitioned, id: h.id, field: "status", actor: h.author, at: h.created, addressed: addressed,
				fromStatus: statusChange.fromString, toStatus: statusChange.toString,
			})
		}
		if dueChange != nil {
			pending = append(pending, emit{
				typ: TypeDueChanged, id: h.id, field: "duedate", actor: h.author, at: h.created,
				detail: dueValue(dueChange),
			})
		}
		for _, m := range mentions {
			pending = append(pending, emit{typ: TypeMentioned, id: h.id, field: m.field, actor: h.author, at: h.created, addressed: true})
		}
	}
	for _, c := range comments {
		if c.created.Before(since) {
			continue
		}
		pending = append(pending, emit{
			typ: TypeCommented, id: c.id, actor: c.author, at: c.created,
			addressed: c.mentions[person], body: c.body,
		})
	}
	return finish(name, person, iss, pending, observed, loc)
}

func firstItems(name, person string, iss issue, histories []history, observed time.Time, loc *time.Location) ([]source.Item, error) {
	if iss.account != person {
		return nil, nil
	}
	id := "open"
	actor := ""
	at := observed
	nameOf := iss.assignee
	for i := len(histories) - 1; i >= 0; i-- {
		h := histories[i]
		for _, it := range h.items {
			if it.field == "assignee" && it.to == person {
				id = h.id
				actor = h.author
				at = h.created
				if it.toString != "" {
					nameOf = it.toString
				}
				break
			}
		}
		if id != "open" {
			break
		}
	}
	return finish(name, person, iss, []emit{{
		typ: TypeAssigned, id: id, actor: actor, at: at, addressed: true, detail: nameOf,
	}}, observed, loc)
}

func finish(name, person string, iss issue, pending []emit, observed time.Time, loc *time.Location) ([]source.Item, error) {
	counts := map[string]int{}
	for _, e := range pending {
		if e.field != "" {
			counts[e.id]++
		}
	}
	obs := observed.UTC()
	due := dueAt(iss.due, loc)
	seen := map[string]bool{}
	var out []source.Item
	for _, e := range pending {
		changeID := e.id
		if e.field != "" && counts[e.id] > 1 {
			changeID = e.id + ":" + e.field
		}
		// Comment ids and changelog ids are separate sequences and can overlap, including across passes.
		// A comment key always carries :comment so it cannot be the key of a changelog item.
		if e.typ == TypeCommented {
			changeID = e.id + ":comment"
		}
		if changeID == "" || seen[changeID] {
			continue
		}
		seen[changeID] = true
		fields := map[string]string{
			"project":    iss.project,
			"key":        iss.key,
			"summary":    iss.summary,
			"url":        iss.url,
			"issue_type": iss.issueType,
			"priority":   iss.priority,
		}
		if e.body != "" {
			fields["body"] = e.body
		}
		if e.fromStatus != "" || e.toStatus != "" {
			fields["from_status"] = e.fromStatus
			fields["to_status"] = e.toStatus
		}
		actor := ""
		if e.actor != "" {
			actor = "jira:account:" + e.actor
		}
		key, version := source.FitKey("jira:"+iss.key+":"+changeID, changeID)
		it := source.Item{
			Source:     name,
			Subject:    "jira:" + iss.key,
			Key:        key,
			Type:       e.typ,
			EventTitle: eventTitle(e),
			OccurredAt: e.at.UTC(),
			Actor:      actor,
			Own:        e.actor != "" && e.actor == person,
			Addressed:  e.addressed,
			Fields:     fields,
			DueAt:      due,
			State:      stateOf(iss),
			ObservedAt: &obs,
			Version:    version,
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// eventTitle describes the change. It uses the type and, when the change has them, the assignee's display name,
// the status names, and the due date. It does not use the issue summary or any comment text.
func eventTitle(e emit) string {
	switch e.typ {
	case TypeAssigned:
		if name := oneLine(e.detail); name != "" {
			return "Assigned to " + name
		}
		return "Assigned"
	case TypeUnassigned:
		return "Unassigned"
	case TypeTransitioned:
		from, to := oneLine(e.fromStatus), oneLine(e.toStatus)
		if from == "" && to == "" {
			return "Transitioned"
		}
		return from + " → " + to
	case TypeCommented:
		return "Commented"
	case TypeMentioned:
		return "Mentioned"
	case TypeDueChanged:
		if date := oneLine(e.detail); date != "" {
			return "Due date set to " + date
		}
		return "Due date cleared"
	default:
		return ""
	}
}

// dueValue is the date the changelog already returned. toString is the display value; to is the raw value.
func dueValue(c *change) string {
	if c == nil {
		return ""
	}
	if s := strings.TrimSpace(c.toString); s != "" {
		return s
	}
	return strings.TrimSpace(c.to)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func stateOf(iss issue) map[string]string {
	st := map[string]string{
		"status":   iss.status,
		"assignee": iss.assignee,
		"priority": iss.priority,
	}
	if iss.resolution != "" {
		st["resolution"] = iss.resolution
	}
	if iss.due != "" {
		st["due"] = iss.due
	}
	return st
}

func initialAssignee(histories []history, current string) string {
	for _, h := range histories {
		for _, it := range h.items {
			if it.field == "assignee" {
				return it.from
			}
		}
	}
	return current
}

func mentionAdded(from, to, person string) bool {
	if person == "" || to == "" {
		return false
	}
	return mentioned(to, person) && !mentioned(from, person)
}

func dueAt(date string, loc *time.Location) *time.Time {
	if date == "" {
		return nil
	}
	if loc == nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return nil
	}
	end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, loc).UTC()
	return &end
}

func parseJiraTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05.000+0000", "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time")
}

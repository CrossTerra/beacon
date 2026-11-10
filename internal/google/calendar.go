package google

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CrossTerra/beacon/internal/source"
)

// CalendarFields is Calendar's fixed field list (DEC-6). attendees is a count. body is the description, read only
// when a rule lists it.
var CalendarFields = []string{"summary", "start", "end", "organizer", "attendees", "response", "link", "body"}

// CalendarStateFields is Calendar's state field list (DEC-28).
var CalendarStateFields = []string{"status", "start", "end", "response"}

// Calendar item types (DEC-18).
const (
	TypeCreated        = "created"
	TypeChanged        = "changed"
	TypeCancelled      = "cancelled"
	TypeResponseNeeded = "response_needed"
	TypeResponded      = "responded"
	TypeUpcoming       = "upcoming"
)

const (
	// calendarWindow is how far ahead each pass reads and fingerprints events.
	calendarWindow = 30 * 24 * time.Hour
	// calendarFirst is how far ahead a first run emits events (DEC-23).
	calendarFirst = 7 * 24 * time.Hour
)

// Calendar reads the person's primary calendar.
type Calendar struct{ common }

// NewCalendar builds the Calendar source. It reads no credential.
func NewCalendar(o Options) (*Calendar, error) {
	c, err := newCommon(o, "calendar", APIBase, calendarPath)
	if err != nil {
		return nil, err
	}
	return &Calendar{c}, nil
}

func calendarPath(p string) bool { return p == "/calendar/v3/calendars/primary/events" }

// Fields implements source.Source.
func (c *Calendar) Fields() []string { return append([]string(nil), CalendarFields...) }

// StateFields implements source.Source.
func (c *Calendar) StateFields() []string { return append([]string(nil), CalendarStateFields...) }

// calCursor is the committed cursor: the time the last pass began, and what each event in the window looked like.
// It holds hashes and the person's response, not event content.
type calCursor struct {
	Since  string             `json:"since"`
	Events map[string]calSeen `json:"events"`
}

type calSeen struct {
	// F is a hash of the summary, location, start, end and status; T of the start and end.
	F string `json:"f"`
	T string `json:"t"`
	// R is the person's response.
	R string `json:"r,omitempty"`
	C bool   `json:"c,omitempty"`
	// End is when the event ends, so an entry is dropped once the event is over.
	End string `json:"end,omitempty"`
}

type calTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
	TimeZone string `json:"timeZone"`
}

// at is the time, or the start of the day in tz for an all-day event.
func (t calTime) at(tz *time.Location) time.Time {
	if t.DateTime != "" {
		v, err := time.Parse(time.RFC3339, t.DateTime)
		if err == nil {
			return v.UTC()
		}
		return time.Time{}
	}
	if t.Date != "" {
		loc := tz
		if t.TimeZone != "" {
			if l, err := time.LoadLocation(t.TimeZone); err == nil {
				loc = l
			}
		}
		if loc == nil {
			loc = time.UTC
		}
		v, err := time.ParseInLocation("2006-01-02", t.Date, loc)
		if err == nil {
			return v.UTC()
		}
	}
	return time.Time{}
}

func (t calTime) String() string {
	if t.DateTime != "" {
		if v, err := time.Parse(time.RFC3339, t.DateTime); err == nil {
			return v.UTC().Format(time.RFC3339)
		}
		return t.DateTime
	}
	return t.Date
}

type calPerson struct {
	Email          string `json:"email"`
	Self           bool   `json:"self"`
	ResponseStatus string `json:"responseStatus"`
}

type calEvent struct {
	ID          string      `json:"id"`
	Status      string      `json:"status"`
	ETag        string      `json:"etag"`
	Summary     string      `json:"summary"`
	Description string      `json:"description"`
	Location    string      `json:"location"`
	Start       calTime     `json:"start"`
	End         calTime     `json:"end"`
	Created     string      `json:"created"`
	Updated     string      `json:"updated"`
	HTMLLink    string      `json:"htmlLink"`
	Organizer   calPerson   `json:"organizer"`
	Creator     calPerson   `json:"creator"`
	Attendees   []calPerson `json:"attendees"`
	Attachments []struct {
		FileID   string `json:"fileId"`
		Title    string `json:"title"`
		MimeType string `json:"mimeType"`
	} `json:"attachments"`
}

func (e *calEvent) self() (calPerson, bool) {
	for _, a := range e.Attendees {
		if a.Self {
			return a, true
		}
	}
	return calPerson{}, false
}

// response is the person's response: their attendee status, or accepted when they organise it and are not listed.
func (e *calEvent) response() string {
	if a, ok := e.self(); ok {
		return a.ResponseStatus
	}
	if e.Organizer.Self {
		return "accepted"
	}
	return ""
}

func (e *calEvent) selfEmail() string {
	if a, ok := e.self(); ok && a.Email != "" {
		return strings.ToLower(a.Email)
	}
	if e.Organizer.Self && e.Organizer.Email != "" {
		return strings.ToLower(e.Organizer.Email)
	}
	if e.Creator.Self && e.Creator.Email != "" {
		return strings.ToLower(e.Creator.Email)
	}
	return ""
}

func hash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func (e *calEvent) seen() calSeen {
	return calSeen{
		F: hash(e.Summary, e.Location, e.Start.String(), e.End.String(), e.Status),
		T: hash(e.Start.String(), e.End.String()),
		R: e.response(),
		C: e.Status == "cancelled",
	}
}

// Collect implements source.Source. A nil cursor emits events in the next 7 days (DEC-23) and fingerprints the rest
// of the 30-day window.
func (c *Calendar) Collect(ctx context.Context, cursor []byte) (source.Collected, error) {
	if _, err := c.grant(ctx, []string{ScopeCalendarEvents, ScopeCalendarRead}); err != nil {
		return source.Collected{}, err
	}
	c.clock.Reset()
	first := len(cursor) == 0
	cur := calCursor{Events: map[string]calSeen{}}
	var since time.Time
	if !first {
		if err := json.Unmarshal(cursor, &cur); err != nil || cur.Since == "" {
			return source.Collected{}, errors.New("calendar: the cursor could not be read")
		}
		t, err := parseTime(cur.Since)
		if err != nil {
			return source.Collected{}, errors.New("calendar: the cursor could not be read")
		}
		since = t
		if cur.Events == nil {
			cur.Events = map[string]calSeen{}
		}
	}
	now := c.now().UTC()
	floor := time.Time{}
	if !first {
		floor = since.Add(-source.Lookback)
	}
	events, tz, err := c.list(ctx, now, floor)
	if isStatus(err, http.StatusGone) && !floor.IsZero() {
		// updatedMin is too old for the API. Read the whole window; the fingerprints keep unchanged events quiet.
		events, tz, err = c.list(ctx, now, time.Time{})
	}
	if err != nil {
		return source.Collected{}, err
	}
	obs := c.observed()
	var items []source.Item
	var times []time.Time
	for i := range events {
		e := &events[i]
		if !idPattern.MatchString(e.ID) {
			continue
		}
		if err := c.checkIdentity(e.selfEmail()); err != nil {
			return source.Collected{}, err
		}
		updated, _ := parseTime(e.Updated)
		if !updated.IsZero() {
			times = append(times, updated)
		}
		prev, known := cur.Events[e.ID]
		next := e.seen()
		if end := e.End.at(tz); !end.IsZero() {
			next.End = end.Format(time.RFC3339)
		} else if known && prev.End != "" {
			next.End = prev.End
		} else {
			// A deleted instance can come without times. Keep it no longer than the window.
			next.End = now.Add(calendarWindow).Format(time.RFC3339)
		}
		cur.Events[e.ID] = next
		var typ string
		if first {
			if e.Status == "cancelled" {
				continue
			}
			if start := e.Start.at(tz); start.IsZero() || start.After(now.Add(calendarFirst)) {
				continue
			}
			typ = TypeUpcoming
			if e.response() == "needsAction" && !e.Organizer.Self {
				typ = TypeResponseNeeded
			}
		} else {
			typ = changeType(e, prev, known, next, floor)
		}
		if typ == "" {
			continue
		}
		items = append(items, c.item(e, typ, prev, known, next, tz, obs))
	}
	for id, s := range cur.Events {
		if end, err := parseTime(s.End); err == nil && end.Before(now.Add(-24*time.Hour)) {
			delete(cur.Events, id)
		}
	}
	nextSince, err := c.clock.Next(since, c.now(), times)
	if errors.Is(err, source.ErrClock) && first && len(times) == 0 {
		// An empty calendar on a first run has no event time; the window starts now.
		nextSince, err = []byte(now.Format(time.RFC3339)), nil
	}
	if err != nil {
		return source.Collected{Items: items}, err
	}
	cur.Since = string(nextSince)
	data, err := json.Marshal(cur)
	if err != nil {
		return source.Collected{Items: items}, err
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	return source.Collected{Items: items, Next: data}, nil
}

// changeType is the one item an event yields this pass: a cancel, then a change of the person's response, then a
// change of the event. Another attendee's response bumps updated and changes none of these.
func changeType(e *calEvent, prev calSeen, known bool, next calSeen, floor time.Time) string {
	needs := next.R == "needsAction" && !e.Organizer.Self
	if next.C {
		if known && prev.C {
			return ""
		}
		return TypeCancelled
	}
	if !known {
		created, _ := parseTime(e.Created)
		switch {
		case needs:
			return TypeResponseNeeded
		case !created.IsZero() && !created.Before(floor):
			return TypeCreated
		default:
			return TypeChanged
		}
	}
	if next.R != prev.R {
		switch next.R {
		case "needsAction":
			if needs {
				return TypeResponseNeeded
			}
		case "accepted", "declined", "tentative":
			if !e.Organizer.Self {
				return TypeResponded
			}
		}
	}
	if next.F != prev.F || prev.C {
		return TypeChanged
	}
	return ""
}

func (c *Calendar) list(ctx context.Context, now, updatedMin time.Time) ([]calEvent, *time.Location, error) {
	fields := "items(id,status,etag,summary,location,start,end,created,updated,htmlLink,organizer(email,self),creator(email,self),attendees(email,self,responseStatus)"
	if c.bodies {
		fields += ",description"
	}
	fields += "),nextPageToken,timeZone"
	q := url.Values{
		"singleEvents": {"true"},
		"timeMin":      {now.Format(time.RFC3339)},
		"timeMax":      {now.Add(calendarWindow).Format(time.RFC3339)},
		"maxResults":   {"250"},
		"fields":       {fields},
	}
	if !updatedMin.IsZero() {
		q.Set("updatedMin", updatedMin.Format(time.RFC3339))
		q.Set("showDeleted", "true")
	}
	return listEvents(ctx, &c.common, q)
}

// listEvents pages through the primary calendar's events.
func listEvents(ctx context.Context, c *common, q url.Values) ([]calEvent, *time.Location, error) {
	var out []calEvent
	var tz *time.Location
	token := ""
	for pages := 0; pages < 40; pages++ {
		if token != "" {
			q.Set("pageToken", token)
		}
		body, err := c.get(ctx, c.base+"/calendar/v3/calendars/primary/events?"+q.Encode())
		if err != nil {
			return nil, nil, err
		}
		var page struct {
			Items         []calEvent `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
			TimeZone      string     `json:"timeZone"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, nil, errors.New("calendar: events could not be read")
		}
		if tz == nil && page.TimeZone != "" {
			if l, err := time.LoadLocation(page.TimeZone); err == nil {
				tz = l
			}
		}
		out = append(out, page.Items...)
		if page.NextPageToken == "" {
			if tz == nil {
				tz = time.UTC
			}
			return out, tz, nil
		}
		token = page.NextPageToken
	}
	return nil, nil, errors.New("calendar: pagination did not end")
}

func (c *Calendar) item(e *calEvent, typ string, prev calSeen, known bool, next calSeen, tz *time.Location, obs time.Time) source.Item {
	resp := e.response()
	fields := map[string]string{
		"summary":   oneLine(e.Summary),
		"start":     e.Start.String(),
		"end":       e.End.String(),
		"organizer": strings.ToLower(e.Organizer.Email),
		"attendees": strconv.Itoa(len(e.Attendees)),
		"response":  resp,
		"link":      e.HTMLLink,
	}
	if c.bodies && e.Description != "" {
		fields["body"] = e.Description
	}
	for k, v := range fields {
		if v == "" {
			delete(fields, k)
		}
	}
	state := map[string]string{"status": e.Status, "start": e.Start.String(), "end": e.End.String(), "response": resp}
	for k, v := range state {
		if v == "" {
			delete(state, k)
		}
	}
	etag := strings.Trim(e.ETag, `"`)
	if etag == "" || !idPattern.MatchString(etag) {
		etag = hash(e.ETag, e.Updated)
	}
	key, version := source.FitKey(c.name+":"+e.ID+":"+typ+":"+etag, etag)
	at, _ := parseTime(e.Updated)
	if at.IsZero() {
		at = obs
	}
	it := source.Item{
		Source:     c.name,
		Subject:    "calendar:" + c.person + ":" + e.ID,
		Key:        key,
		Type:       typ,
		EventTitle: calendarTitle(typ, resp, known && prev.T != next.T),
		OccurredAt: at,
		Fields:     fields,
		State:      state,
		ObservedAt: &obs,
		Version:    version,
	}
	if len(state) == 0 {
		it.State, it.ObservedAt = nil, nil
	}
	if start := e.Start.at(tz); !start.IsZero() {
		it.DueAt = &start
	}
	organizer := ""
	if e.Organizer.Email != "" {
		organizer = "email:" + strings.ToLower(e.Organizer.Email)
	}
	switch typ {
	case TypeResponded:
		it.Own = true
		if me := e.selfEmail(); me != "" {
			it.Actor = "email:" + me
		}
	case TypeResponseNeeded:
		it.Addressed = true
		it.Actor = organizer
	case TypeUpcoming:
		it.Actor = organizer
	default:
		if e.Organizer.Self {
			it.Own = true
			if me := e.selfEmail(); me != "" {
				it.Actor = "email:" + me
			}
		} else {
			it.Actor = organizer
			it.Addressed = typ == TypeChanged && resp == "needsAction"
		}
	}
	return it
}

// calendarTitle describes the change from the item type and the person's response. It never uses the summary,
// a name or the description.
func calendarTitle(typ, resp string, moved bool) string {
	switch typ {
	case TypeCreated:
		return "Created"
	case TypeChanged:
		if moved {
			return "Rescheduled"
		}
		return "Changed"
	case TypeCancelled:
		return "Cancelled"
	case TypeResponseNeeded:
		return "Response needed"
	case TypeResponded:
		switch resp {
		case "accepted", "declined", "tentative":
			return "Responded: " + resp
		}
		return "Responded"
	case TypeUpcoming:
		return "Upcoming"
	}
	return ""
}

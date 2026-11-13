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

// CalendarFields is Calendar's fixed field list (DEC-6). attendees is a count; attendee_addresses the attendees'
// addresses (DEC-37). body is the description, read only when a sealed rule lists it. title is the summary, with the
// meeting's day when the summary alone does not tell one meeting from another (a recurring meeting's instance, or a
// summary that more than one event in the window has). meet_link is the meeting's video link.
var CalendarFields = []string{"title", "summary", "start", "end", "organizer", "attendees", "attendee_addresses", "response", "link", "meet_link", "body"}

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

func calendarPath(p string, q url.Values) bool {
	if p == "/calendar/v3/calendars/primary/events" {
		return onlyKeys(q, eventListKeys...)
	}
	id, ok := strings.CutPrefix(p, "/calendar/v3/calendars/primary/events/")
	return ok && idPattern.MatchString(id) && onlyKeys(q, "fields")
}

// eventListKeys are the query keys an events list may carry. eventTypes asks only for meetings, not working
// locations and the other blocks Calendar records as events.
var eventListKeys = []string{"singleEvents", "showDeleted", "timeMin", "timeMax", "updatedMin", "maxResults", "fields", "pageToken", "eventTypes"}

// keptEventTypes are the event types Beacon reads. The API's other types are not meetings.
var keptEventTypes = []string{"default", "fromGmail"}

// skippedEventType reports an event Calendar records that is not a meeting: a working location (all-day "Home"),
// an out-of-office block, a focus-time block, or a birthday. Empty is kept: a response that omits the field is a
// normal event.
func skippedEventType(t string) bool {
	switch t {
	case "workingLocation", "outOfOffice", "focusTime", "birthday":
		return true
	default:
		return false
	}
}

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
	// S is a hash of the summary alone, so a summary that more than one event has is dated in the title.
	S string `json:"s,omitempty"`
	C bool   `json:"c,omitempty"`
	// Start and End place the event, so an entry is dropped once the event is over, and a full re-read knows which
	// events it should have seen.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
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
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	ETag        string  `json:"etag"`
	Summary     string  `json:"summary"`
	Description string  `json:"description"`
	Location    string  `json:"location"`
	Start       calTime `json:"start"`
	End         calTime `json:"end"`
	Created     string  `json:"created"`
	Updated     string  `json:"updated"`
	HTMLLink    string  `json:"htmlLink"`
	EventType   string  `json:"eventType"`
	// RecurringEventID is the series an instance belongs to. The instance keeps its own id, and its own subject.
	RecurringEventID string `json:"recurringEventId"`
	HangoutLink      string `json:"hangoutLink"`
	ConferenceData   *struct {
		EntryPoints []struct {
			EntryPointType string `json:"entryPointType"`
			URI            string `json:"uri"`
		} `json:"entryPoints"`
	} `json:"conferenceData"`
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
		S: summaryHash(e.Summary),
		C: e.Status == "cancelled",
	}
}

// summaryHash is "" for an event with no summary: events with none are not one repeated meeting.
func summaryHash(summary string) string {
	s := strings.ToLower(oneLine(summary))
	if s == "" {
		return ""
	}
	return hash("summary", s)
}

// meetLink is the event's video link: Meet's hangoutLink, else the conference's video entry point. Only an https
// link is kept.
func (e *calEvent) meetLink() string {
	link := e.HangoutLink
	if link == "" && e.ConferenceData != nil {
		for _, p := range e.ConferenceData.EntryPoints {
			if p.EntryPointType == "video" {
				link = p.URI
				break
			}
		}
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return link
}

// datedTitle is the summary with the meeting's day in the calendar's zone: "Tech stand-up · Thu 12 Nov".
func datedTitle(summary string, start calTime, tz *time.Location) string {
	summary = oneLine(summary)
	if summary == "" {
		return ""
	}
	if tz == nil {
		tz = time.UTC
	}
	var day time.Time
	if start.DateTime != "" {
		t, err := time.Parse(time.RFC3339, start.DateTime)
		if err != nil {
			return summary
		}
		day = t.In(tz)
	} else if start.Date != "" {
		t, err := time.Parse("2006-01-02", start.Date)
		if err != nil {
			return summary
		}
		day = t
	} else {
		return summary
	}
	return summary + " · " + day.Format("Mon 2 Jan")
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
	gone := isStatus(err, http.StatusGone) && !floor.IsZero()
	if gone {
		// updatedMin is too old for the API. Read the whole window; the fingerprints keep unchanged events quiet.
		events, tz, err = c.list(ctx, now, time.Time{})
	}
	if err != nil {
		return source.Collected{}, err
	}
	switch {
	case gone:
		// A full re-read of the window holds every event in it, deleted ones included. A known event in the
		// window that is not there was deleted, or moved out. Each is read by id to tell which.
		missing, err := c.missing(ctx, now, events, cur.Events)
		if err != nil {
			return source.Collected{}, err
		}
		events = append(events, missing...)
	case !first:
		// An event Beacon knows that moved into the past or beyond the window is not in that list. It is read
		// without the window, so the move is a change and not a silence.
		moved, err := c.movedOut(ctx, floor, events, cur.Events)
		if err != nil {
			return source.Collected{}, err
		}
		events = append(events, moved...)
	}
	obs := c.observed()
	var items []source.Item
	var pending []calPending
	var times []time.Time
	for i := range events {
		e := &events[i]
		if !idPattern.MatchString(e.ID) {
			continue
		}
		if skippedEventType(e.EventType) {
			// Not a meeting. Drop it, and forget one a previous cursor tracked, so it is not read again.
			delete(cur.Events, e.ID)
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
		if start := e.Start.at(tz); !start.IsZero() {
			next.Start = start.Format(time.RFC3339)
		} else if known {
			next.Start = prev.Start
		}
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
		pending = append(pending, calPending{e: e, typ: typ, prev: prev, known: known, next: next})
	}
	// A summary is repeated when more than one event Beacon knows in the window has it, this pass's included.
	summaries := map[string]int{}
	for _, s := range cur.Events {
		if s.S != "" && !s.C {
			summaries[s.S]++
		}
	}
	for _, p := range pending {
		dated := p.e.RecurringEventID != "" || summaries[p.next.S] > 1
		items = append(items, c.item(p.e, p.typ, p.prev, p.known, p.next, tz, obs, dated))
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

// calPending is an event that yields an item this pass, before its title is known.
type calPending struct {
	e     *calEvent
	typ   string
	prev  calSeen
	known bool
	next  calSeen
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
	fields := "items(" + eventFields(c.bodies) + "),nextPageToken,timeZone"
	q := url.Values{
		"singleEvents": {"true"},
		"timeMin":      {now.Format(time.RFC3339)},
		"timeMax":      {now.Add(calendarWindow).Format(time.RFC3339)},
		"maxResults":   {"250"},
		"fields":       {fields},
		// A cancelled event must be seen as cancelled, not as missing.
		"showDeleted": {"true"},
		"eventTypes":  keptEventTypes,
	}
	if !updatedMin.IsZero() {
		q.Set("updatedMin", updatedMin.Format(time.RFC3339))
	}
	return listEvents(ctx, &c.common, q)
}

// missing reads, by id, each event the cursor places in the window that a full re-read of the window did not
// return. One that is gone is a cancelled event; one that moved is itself.
func (c *Calendar) missing(ctx context.Context, now time.Time, inWindow []calEvent, known map[string]calSeen) ([]calEvent, error) {
	seen := map[string]bool{}
	for _, e := range inWindow {
		seen[e.ID] = true
	}
	var out []calEvent
	for _, id := range sortedIDs(known) {
		s := known[id]
		if seen[id] || s.C || !idPattern.MatchString(id) {
			continue
		}
		end, err1 := parseTime(s.End)
		start, err2 := parseTime(s.Start)
		if err1 != nil || err2 != nil || !end.After(now) || !start.Before(now.Add(calendarWindow)) {
			continue
		}
		body, err := c.get(ctx, c.base+"/calendar/v3/calendars/primary/events/"+id+"?"+url.Values{"fields": {eventFields(c.bodies)}}.Encode())
		if isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusGone) {
			out = append(out, calEvent{ID: id, Status: "cancelled", ETag: "gone"})
			continue
		}
		if errors.Is(err, errPermission) {
			// The grant can no longer read this one event. It is forgotten rather than retried on every full
			// re-read, so it cannot stall the source; if it comes back, it is new to Beacon.
			delete(known, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		var e calEvent
		if err := json.Unmarshal(body, &e); err != nil || e.ID != id {
			return nil, errors.New("calendar: an event could not be read")
		}
		out = append(out, e)
	}
	return out, nil
}

func sortedIDs(m map[string]calSeen) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func eventFields(bodies bool) string {
	f := "id,status,etag,summary,location,start,end,created,updated,htmlLink,eventType,recurringEventId,hangoutLink,conferenceData(entryPoints(entryPointType,uri)),organizer(email,self),creator(email,self),attendees(email,self,responseStatus)"
	if bodies {
		f += ",description"
	}
	return f
}

// movedOut reads events updated since floor with no time window, not expanded, and keeps those Beacon knows that the
// windowed list did not return. A moved instance of a recurring event is its own event here, under its instance id.
func (c *Calendar) movedOut(ctx context.Context, floor time.Time, inWindow []calEvent, known map[string]calSeen) ([]calEvent, error) {
	if len(known) == 0 {
		return nil, nil
	}
	fields := "items(" + eventFields(c.bodies) + "),nextPageToken,timeZone"
	q := url.Values{
		"singleEvents": {"false"},
		"showDeleted":  {"true"},
		"updatedMin":   {floor.Format(time.RFC3339)},
		"maxResults":   {"250"},
		"fields":       {fields},
		"eventTypes":   keptEventTypes,
	}
	all, _, err := listEvents(ctx, &c.common, q)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range inWindow {
		seen[e.ID] = true
	}
	var out []calEvent
	for _, e := range all {
		if _, ok := known[e.ID]; ok && !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out, nil
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

func (c *Calendar) item(e *calEvent, typ string, prev calSeen, known bool, next calSeen, tz *time.Location, obs time.Time, dated bool) source.Item {
	resp := e.response()
	attendees := attendeeAddresses(e)
	title := oneLine(e.Summary)
	if dated {
		title = datedTitle(e.Summary, e.Start, tz)
	}
	fields := map[string]string{
		"title":              title,
		"summary":            oneLine(e.Summary),
		"start":              e.Start.String(),
		"end":                e.End.String(),
		"organizer":          strings.ToLower(e.Organizer.Email),
		"attendees":          attendeeCount(e),
		"attendee_addresses": strings.Join(attendees, ", "),
		"response":           resp,
		"link":               e.HTMLLink,
		"meet_link":          e.meetLink(),
	}
	local := &source.Local{
		Text:      map[string]string{"summary": fields["summary"]},
		Addresses: map[string][]string{"organizer": {fields["organizer"]}, "attendee_addresses": attendees},
	}
	if c.bodies && e.Description != "" {
		fields["body"] = e.Description
		local.Text["body"] = e.Description
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
		Native:     opaque(e.ID),
		Local:      local,
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
	// Calendar does not say who made a change. An own act closes the person's asks, so it is own only when it is
	// provably theirs: their response, or an event their account created. Organising an event proves nothing about
	// who changed or cancelled it: a co-organiser or a delegate can. The organizer is the actor only when it is
	// someone else.
	if e.Organizer.Self {
		organizer = ""
	}
	switch typ {
	case TypeResponded:
		it.Own = true
		if me := e.selfEmail(); me != "" {
			it.Actor = "email:" + me
		}
	case TypeCreated:
		if e.Creator.Self {
			it.Own = true
			if me := e.selfEmail(); me != "" {
				it.Actor = "email:" + me
			}
		} else {
			it.Actor = organizer
		}
	case TypeResponseNeeded:
		it.Addressed = true
		it.Actor = organizer
	default:
		it.Actor = organizer
		it.Addressed = typ == TypeChanged && resp == "needsAction" && !e.Organizer.Self
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

// attendeeAddresses are the attendees' addresses, lowercased, each once, sorted (DEC-37).
func attendeeAddresses(e *calEvent) []string {
	var out []string
	for _, a := range e.Attendees {
		out = append(out, strings.ToLower(strings.TrimSpace(a.Email)))
	}
	return uniqueSorted(out)
}

// opaque is id when it can be a sealed event's native id: printable, without @ (Signal DEC-264). Otherwise "".
func opaque(id string) string {
	if id == "" || strings.Contains(id, "@") || len(id) > 200 {
		return ""
	}
	for _, r := range id {
		if r < 0x21 || r > 0x7e {
			return ""
		}
	}
	return id
}

// attendeeCount is how many attendees the event lists, or "" for an event with none listed, which is the person's
// own event or one Google returned without its details.
func attendeeCount(e *calEvent) string {
	if len(e.Attendees) == 0 {
		return ""
	}
	return strconv.Itoa(len(e.Attendees))
}

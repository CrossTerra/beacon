package google

import (
	"context"
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

// DriveFields is Drive's fixed field list (DEC-6). title is the file name. body is comment text or meeting notes
// text, read only when a rule lists it.
var DriveFields = []string{"title", "link", "file_type", "author", "meeting", "meeting_start", "body"}

// DriveStateFields is Drive's state field list (DEC-28), for comments.
var DriveStateFields = []string{"resolved", "assignee"}

// Drive item types (DEC-18).
const (
	TypeMentioned    = "mentioned"
	TypeAssigned     = "assigned"
	TypeCommented    = "commented"
	TypeResolved     = "resolved"
	TypeNotesCreated = "notes_created"
)

const (
	// driveFirst is how far back a first run reads.
	driveFirst = 7 * 24 * time.Hour
	// driveViewed is how far back a file the person opened counts as recently active.
	driveViewed = 14 * 24 * time.Hour
	// notesWindow is how far back meetings are read for notes.
	notesWindow = 7 * 24 * time.Hour
	// notesBodyMax is how much of a meeting notes file is read when a rule lists body.
	notesBodyMax = 32 << 10
	// geminiNotes marks a meeting notes attachment.
	geminiNotes = "notes by gemini"
)

// mimeTypes are the files whose comments are read: Docs, Sheets and Slides.
var mimeTypes = map[string]string{
	"application/vnd.google-apps.document":     "document",
	"application/vnd.google-apps.spreadsheet":  "spreadsheet",
	"application/vnd.google-apps.presentation": "presentation",
}

// Drive reads comments that concern the person, and meeting notes from meetings they attended.
type Drive struct{ common }

// NewDrive builds the Drive source. It reads no credential.
func NewDrive(o Options) (*Drive, error) {
	c, err := newCommon(o, "drive", APIBase, drivePath)
	if err != nil {
		return nil, err
	}
	return &Drive{c}, nil
}

func drivePath(p string, q url.Values) bool {
	switch p {
	case "/drive/v3/about":
		return onlyKeys(q, "fields")
	case "/drive/v3/files":
		return onlyKeys(q, "q", "fields", "pageSize", "corpora", "includeItemsFromAllDrives", "supportsAllDrives", "orderBy", "pageToken")
	case "/calendar/v3/calendars/primary/events":
		return onlyKeys(q, eventListKeys...)
	}
	rest, ok := strings.CutPrefix(p, "/drive/v3/files/")
	if !ok {
		return false
	}
	id, suffix, _ := strings.Cut(rest, "/")
	if !idPattern.MatchString(id) {
		return false
	}
	switch suffix {
	case "":
		// Metadata only: alt=media would download the file.
		return onlyKeys(q, "fields", "supportsAllDrives")
	case "comments":
		return onlyKeys(q, "startModifiedTime", "pageSize", "fields", "pageToken")
	case "export":
		// Meeting notes as plain text, read only when a rule lists body.
		return onlyKeys(q, "mimeType") && q.Get("mimeType") == "text/plain"
	}
	return false
}

// Fields implements source.Source.
func (d *Drive) Fields() []string { return append([]string(nil), DriveFields...) }

// StateFields implements source.Source.
func (d *Drive) StateFields() []string { return append([]string(nil), DriveStateFields...) }

type driveFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MimeType    string `json:"mimeType"`
	WebViewLink string `json:"webViewLink"`
	CreatedTime string `json:"createdTime"`
	Trashed     bool   `json:"trashed"`
}

type driveAuthor struct {
	DisplayName string `json:"displayName"`
	Me          bool   `json:"me"`
}

type driveReply struct {
	ID          string      `json:"id"`
	CreatedTime string      `json:"createdTime"`
	Author      driveAuthor `json:"author"`
	Mentioned   []string    `json:"mentionedEmailAddresses"`
	Assignee    string      `json:"assigneeEmailAddress"`
	Deleted     bool        `json:"deleted"`
	Action      string      `json:"action"`
	Content     string      `json:"content"`
}

type driveComment struct {
	driveReply
	Resolved bool         `json:"resolved"`
	Replies  []driveReply `json:"replies"`
}

// Collect implements source.Source. The cursor is a time, as GitHub's and Jira's. A nil cursor reads the last 7
// days: unresolved comments that mention or are assigned to the person, and meeting notes. No own acts.
func (d *Drive) Collect(ctx context.Context, cursor []byte) (source.Collected, error) {
	if _, err := d.grant(ctx, []string{ScopeDriveReadonly}, []string{ScopeCalendarEvents, ScopeCalendarRead}); err != nil {
		return source.Collected{}, err
	}
	d.clock.Reset()
	first := len(cursor) == 0
	var since time.Time
	if !first {
		t, err := parseTime(string(cursor))
		if err != nil {
			return source.Collected{}, errors.New("drive: the cursor is not a time")
		}
		since = t
	}
	now := d.now().UTC()
	floor := now.Add(-driveFirst)
	if !first {
		floor = since.Add(-source.Lookback)
	}
	var about struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := d.getJSON(ctx, d.base+"/drive/v3/about?"+url.Values{"fields": {"user(emailAddress)"}}.Encode(), &about); err != nil {
		return source.Collected{}, err
	}
	me := strings.ToLower(about.User.EmailAddress)
	if me == "" {
		return source.Collected{}, errors.New("drive: the account could not be read")
	}
	if err := d.checkIdentity(me); err != nil {
		return source.Collected{}, err
	}
	obs := d.observed()
	files, err := d.files(ctx, now, floor)
	if err != nil {
		return source.Collected{}, err
	}
	var items []source.Item
	var times []time.Time
	for _, f := range files {
		comments, err := d.comments(ctx, f.ID, floor)
		if unavailable(err) {
			continue
		}
		if err != nil {
			return source.Collected{Items: items}, err
		}
		for _, c := range comments {
			for _, it := range d.commentItems(f, c, me, floor, first, obs) {
				items = append(items, it)
				times = append(times, it.OccurredAt)
			}
		}
	}
	notes, err := d.notes(ctx, now, floor, first, me, obs)
	if err != nil {
		return source.Collected{Items: items}, err
	}
	items = append(items, notes...)
	for _, it := range notes {
		times = append(times, it.OccurredAt)
	}
	next, err := d.clock.Next(since, d.now(), times)
	if errors.Is(err, source.ErrClock) && first && len(times) == 0 {
		next, err = []byte(now.Format(time.RFC3339)), nil
	}
	if err != nil {
		return source.Collected{Items: items}, err
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	return source.Collected{Items: items, Next: next}, nil
}

func (d *Drive) getJSON(ctx context.Context, raw string, dest any) error {
	body, err := d.get(ctx, raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return errors.New("drive: a response could not be read")
	}
	return nil
}

// files lists the Docs, Sheets and Slides whose comments are read this pass. A comment does not reliably change a
// file's modifiedTime, so a file's comments are read when it is recently active by either measure: modified since
// floor, or opened by the person in the last 14 days (at most 200 files, most recent first). comments.list with
// startModifiedTime then says which comments changed. A mention on a file the person has neither opened recently nor
// seen modified is not found.
func (d *Drive) files(ctx context.Context, now, floor time.Time) ([]driveFile, error) {
	var kinds []string
	for m := range mimeTypes {
		kinds = append(kinds, "mimeType = '"+m+"'")
	}
	sort.Strings(kinds)
	types := " and trashed = false and (" + strings.Join(kinds, " or ") + ")"
	modified, err := d.listFiles(ctx, "modifiedTime > '"+floor.Format(time.RFC3339)+"'"+types, "modifiedTime desc", 50)
	if err != nil {
		return nil, err
	}
	viewed, err := d.listFiles(ctx, "viewedByMeTime > '"+now.Add(-driveViewed).Format(time.RFC3339)+"'"+types, "viewedByMeTime desc", 2)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []driveFile
	for _, f := range append(modified, viewed...) {
		if !seen[f.ID] {
			seen[f.ID] = true
			out = append(out, f)
		}
	}
	return out, nil
}

// listFiles pages through a files query. A modified-files query that does not end within maxPages fails the pass;
// the recently-viewed query is capped by design and stops there.
func (d *Drive) listFiles(ctx context.Context, query, order string, maxPages int) ([]driveFile, error) {
	q := url.Values{
		"q":                         {query},
		"fields":                    {"nextPageToken,files(id,name,mimeType,webViewLink)"},
		"pageSize":                  {"100"},
		"corpora":                   {"user"},
		"includeItemsFromAllDrives": {"true"},
		"supportsAllDrives":         {"true"},
		"orderBy":                   {order},
	}
	var out []driveFile
	for pages := 0; pages < maxPages; pages++ {
		var page struct {
			Files         []driveFile `json:"files"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := d.getJSON(ctx, d.base+"/drive/v3/files?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		for _, f := range page.Files {
			if idPattern.MatchString(f.ID) {
				out = append(out, f)
			}
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
	if strings.HasPrefix(query, "viewedByMeTime") {
		return out, nil
	}
	return nil, errors.New("drive: file pagination did not end")
}

func (d *Drive) comments(ctx context.Context, fileID string, floor time.Time) ([]driveComment, error) {
	who := "id,createdTime,author(displayName,me),mentionedEmailAddresses,assigneeEmailAddress,deleted"
	reply := who + ",action"
	if d.bodies {
		who += ",content"
		reply += ",content"
	}
	q := url.Values{
		"startModifiedTime": {floor.Format(time.RFC3339)},
		"pageSize":          {"100"},
		"fields":            {"nextPageToken,comments(" + who + ",resolved,replies(" + reply + "))"},
	}
	var out []driveComment
	for pages := 0; pages < 50; pages++ {
		var page struct {
			Comments      []driveComment `json:"comments"`
			NextPageToken string         `json:"nextPageToken"`
		}
		if err := d.getJSON(ctx, d.base+"/drive/v3/files/"+fileID+"/comments?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		out = append(out, page.Comments...)
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
	return nil, errors.New("drive: comment pagination did not end")
}

// commentItems yields one item per comment or reply created since floor that concerns the person: assigned to
// them, mentioning them, or their own. A first run yields only unresolved comments by others.
func (d *Drive) commentItems(f driveFile, c driveComment, me string, floor time.Time, first bool, obs time.Time) []source.Item {
	if !idPattern.MatchString(c.ID) || (first && c.Resolved) {
		return nil
	}
	var out []source.Item
	emit := func(r driveReply, reply bool) {
		if r.Deleted || !idPattern.MatchString(r.ID) {
			return
		}
		at, err := parseTime(r.CreatedTime)
		if err != nil || at.Before(floor) {
			return
		}
		var typ string
		switch {
		case r.Author.Me && first:
			return
		case r.Author.Me && r.Action == "resolve":
			typ = TypeResolved
		case r.Author.Me:
			typ = TypeCommented
		case strings.EqualFold(r.Assignee, me):
			typ = TypeAssigned
		case containsFold(r.Mentioned, me):
			typ = TypeMentioned
		default:
			return
		}
		keyID := f.ID + ":comment:" + c.ID
		version := c.ID
		if reply {
			keyID = f.ID + ":reply:" + c.ID + ":" + r.ID
			version = r.ID
		}
		key, version := source.FitKey(d.name+":"+keyID, version)
		fields := map[string]string{
			"title":     oneLine(f.Name),
			"link":      commentLink(f.WebViewLink, c.ID),
			"file_type": fileType(f.MimeType),
			"author":    oneLine(r.Author.DisplayName),
		}
		if d.bodies && r.Content != "" {
			fields["body"] = r.Content
		}
		for k, v := range fields {
			if v == "" {
				delete(fields, k)
			}
		}
		state := map[string]string{"resolved": strconv.FormatBool(c.Resolved)}
		if c.Assignee != "" {
			state["assignee"] = strings.ToLower(c.Assignee)
		}
		it := source.Item{
			Source:     d.name,
			Subject:    "drive:" + d.person + ":" + f.ID,
			Key:        key,
			Type:       typ,
			EventTitle: driveTitle(typ, reply),
			OccurredAt: at,
			Fields:     fields,
			State:      state,
			ObservedAt: &obs,
			Version:    version,
		}
		if r.Author.Me {
			it.Own = true
			it.Actor = "email:" + me
		} else {
			it.Addressed = true
		}
		out = append(out, it)
	}
	emit(c.driveReply, false)
	for _, r := range c.Replies {
		emit(r, true)
	}
	return out
}

// notes finds meeting notes: events on the primary calendar that ended in the last 7 days, changed since floor, that
// the person organised or accepted, with a Google Doc attachment titled Notes by Gemini.
func (d *Drive) notes(ctx context.Context, now, floor time.Time, first bool, me string, obs time.Time) ([]source.Item, error) {
	fields := "items(id,status,summary,start,end,updated,organizer(self),attendees(self,responseStatus),attachments(fileId,title,mimeType)),nextPageToken,timeZone"
	list := func(timeMin time.Time, updatedMin time.Time) ([]calEvent, *time.Location, error) {
		q := url.Values{
			"singleEvents": {"true"},
			"timeMin":      {timeMin.Format(time.RFC3339)},
			"timeMax":      {now.Format(time.RFC3339)},
			"maxResults":   {"250"},
			"fields":       {fields},
		}
		if !updatedMin.IsZero() {
			q.Set("updatedMin", updatedMin.Format(time.RFC3339))
		}
		return listEvents(ctx, &d.common, q)
	}
	// Notes are usually attached while the meeting runs, which is the event's last update; the meeting has not
	// ended then. So a pass reads the meetings that ended since the window began (timeMin filters on the end), with
	// no updatedMin, and also those updated since then, for notes attached after the end. A first run reads every
	// meeting in the last 7 days.
	var events []calEvent
	var tz *time.Location
	var err error
	if first {
		events, tz, err = list(now.Add(-notesWindow), time.Time{})
	} else {
		// floor comes from the server's Date, which may run ahead of the local clock; timeMin must stay before
		// timeMax. An earlier timeMin only reads more, and keys keep notes already recorded quiet.
		since := floor
		if limit := now.Add(-source.Lookback); since.After(limit) {
			since = limit
		}
		events, tz, err = list(since, time.Time{})
		if err == nil {
			var later []calEvent
			later, _, err = list(now.Add(-notesWindow), floor)
			if isStatus(err, http.StatusGone) {
				// updatedMin is too old: read the whole week. Keys keep the notes already recorded quiet.
				later, _, err = list(now.Add(-notesWindow), time.Time{})
			}
			events = append(events, later...)
		}
	}
	if err != nil {
		return nil, err
	}
	var out []source.Item
	seen := map[string]bool{}
	for i := range events {
		e := &events[i]
		if e.Status == "cancelled" {
			continue
		}
		if end := e.End.at(tz); end.IsZero() || end.After(now) {
			continue
		}
		if !e.Organizer.Self && e.response() != "accepted" {
			continue
		}
		for _, a := range e.Attachments {
			if a.MimeType != "application/vnd.google-apps.document" || !strings.Contains(strings.ToLower(a.Title), geminiNotes) {
				continue
			}
			if !idPattern.MatchString(a.FileID) || seen[a.FileID] {
				continue
			}
			seen[a.FileID] = true
			it, ok, err := d.notesItem(ctx, a.FileID, e, obs)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, it)
			}
		}
	}
	return out, nil
}

func (d *Drive) notesItem(ctx context.Context, fileID string, e *calEvent, obs time.Time) (source.Item, bool, error) {
	var f driveFile
	q := url.Values{"fields": {"id,name,mimeType,webViewLink,createdTime,trashed"}, "supportsAllDrives": {"true"}}
	err := d.getJSON(ctx, d.base+"/drive/v3/files/"+fileID+"?"+q.Encode(), &f)
	if unavailable(err) {
		return source.Item{}, false, nil
	}
	if err != nil {
		return source.Item{}, false, err
	}
	if f.Trashed || f.ID != fileID {
		return source.Item{}, false, nil
	}
	at, err := parseTime(f.CreatedTime)
	if err != nil {
		return source.Item{}, false, nil
	}
	fields := map[string]string{
		"title":         oneLine(f.Name),
		"link":          f.WebViewLink,
		"file_type":     fileType(f.MimeType),
		"meeting":       oneLine(e.Summary),
		"meeting_start": e.Start.String(),
	}
	if d.bodies {
		text, err := d.export(ctx, fileID)
		if err != nil && !unavailable(err) {
			return source.Item{}, false, err
		}
		if text != "" {
			fields["body"] = text
		}
	}
	for k, v := range fields {
		if v == "" {
			delete(fields, k)
		}
	}
	key, version := source.FitKey(d.name+":notes:"+fileID, fileID)
	return source.Item{
		Source:     d.name,
		Subject:    "drive:" + d.person + ":" + fileID,
		Key:        key,
		Type:       TypeNotesCreated,
		EventTitle: driveTitle(TypeNotesCreated, false),
		OccurredAt: at,
		Fields:     fields,
		Version:    version,
	}, true, nil
}

// export reads the first 32 KiB of a notes file as plain text. It is called only when a rule lists body.
func (d *Drive) export(ctx context.Context, fileID string) (string, error) {
	raw := d.base + "/drive/v3/files/" + fileID + "/export?" + url.Values{"mimeType": {"text/plain"}}.Encode()
	data, err := d.call(ctx, http.MethodGet, raw, notesBodyMax, "text/plain")
	if err != nil {
		return "", err
	}
	// The limit can cut a character in two; the cut bytes are dropped.
	return strings.TrimPrefix(strings.ToValidUTF8(string(data), ""), "\ufeff"), nil
}

// driveTitle describes the change. It never uses the file name, a name or comment text.
func driveTitle(typ string, reply bool) string {
	switch typ {
	case TypeMentioned:
		return "Mentioned"
	case TypeAssigned:
		return "Assigned"
	case TypeCommented:
		if reply {
			return "Replied"
		}
		return "Commented"
	case TypeResolved:
		return "Resolved"
	case TypeNotesCreated:
		return "Meeting notes created"
	}
	return ""
}

func fileType(mime string) string {
	if t, ok := mimeTypes[mime]; ok {
		return t
	}
	return "other"
}

func commentLink(view, commentID string) string {
	if view == "" {
		return ""
	}
	sep := "?"
	if strings.Contains(view, "?") {
		sep = "&"
	}
	return view + sep + "disco=" + url.QueryEscape(commentID)
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), v) {
			return true
		}
	}
	return false
}

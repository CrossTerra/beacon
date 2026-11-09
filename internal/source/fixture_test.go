package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `{"source":"fixture","fields":["title"],"items":[
 {"subject":"fixture:a","key":"fixture:a:1","type":"opened","occurred_at":"2026-11-07T09:00:00Z","actor":"fixture:user:x","fields":{"title":"t"}}
]}`

func write(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFixture(t *testing.T) {
	f, err := LoadFixture(write(t, good))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.Collect(context.Background(), nil)
	items := got.Items
	if f.Name() != "fixture" || len(items) != 1 || items[0].Source != "fixture" || f.Hosts() != nil {
		t.Fatalf("%+v", items)
	}
}

func TestFixtureRejects(t *testing.T) {
	cases := map[string][2]string{
		"unknown key":        {`"fields":["title"]`, `"fields":["title"],"extra":1`},
		"field not in list":  {`"fields":{"title":"t"}`, `"fields":{"body":"t"}`},
		"key without source": {`"key":"fixture:a:1"`, `"key":"other:a:1"`},
		"bad type":           {`"type":"opened"`, `"type":"Opened!"`},
		"space in subject":   {`"subject":"fixture:a"`, `"subject":"fixture a"`},
		"no time":            {`"occurred_at":"2026-11-07T09:00:00Z",`, ``},
		"bad source name":    {`"source":"fixture"`, `"source":"Fixture"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := strings.Replace(good, c[0], c[1], 1)
			if s == good {
				t.Fatal("no change")
			}
			if _, err := LoadFixture(write(t, s)); err == nil {
				t.Fatalf("loaded %s", s)
			}
		})
	}
	dup := strings.Replace(good, "]}", `,{"subject":"fixture:a","key":"fixture:a:1","type":"opened","occurred_at":"2026-11-07T09:00:00Z"}]}`, 1)
	if _, err := LoadFixture(write(t, dup)); err == nil {
		t.Fatal("loaded a repeated key")
	}
}

func TestFixtureState(t *testing.T) {
	withState := strings.Replace(good, `"fields":["title"],`, `"fields":["title"],"state_fields":["status"],`, 1)
	snap := `"fields":{"title":"t"},"state":{"status":"open"},"observed_at":"2026-11-07T09:05:00Z","version":"1"`
	ok := strings.Replace(withState, `"fields":{"title":"t"}`, snap, 1)
	f, err := LoadFixture(write(t, ok))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.Collect(context.Background(), nil)
	items := got.Items
	if items[0].State["status"] != "open" || items[0].Version != "1" || f.StateFields()[0] != "status" {
		t.Fatalf("%+v", items[0])
	}
	cases := map[string]string{
		"state field not listed": strings.Replace(ok, `"state":{"status"`, `"state":{"colour"`, 1),
		"no observed_at":         strings.Replace(ok, `"observed_at":"2026-11-07T09:05:00Z",`, ``, 1),
		"no version":             strings.Replace(ok, `,"version":"1"`, ``, 1),
		"version not in key":     strings.Replace(ok, `"version":"1"`, `"version":"2"`, 1),
		"reserved state field":   strings.Replace(ok, `"state_fields":["status"]`, `"state_fields":["status","version"]`, 1),
		"two JSON values":        ok + ` {}`,
	}
	for name, s := range cases {
		if s == ok {
			t.Fatalf("%s: no change", name)
		}
		if _, err := LoadFixture(write(t, s)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

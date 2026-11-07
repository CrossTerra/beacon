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
	items, _ := f.Collect(context.Background())
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

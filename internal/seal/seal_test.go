package seal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/CrossTerra/beacon/internal/source"
)

func owner(t *testing.T, doc string) *OwnerList {
	t.Helper()
	var o OwnerList
	if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
		t.Fatal(err)
	}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	return &o
}

func TestWords(t *testing.T) {
	o := owner(t, `{words: [salary, "background check", {exact: PIP}, W-2]}`)
	for text, want := range map[string]bool{
		"Your SALARY review":           true,
		"salaryman":                    false,
		"re: salary.":                  true,
		"the Background  Check is due": true,
		"background checks":            false,
		"a PIP for Q4":                 true,
		"pip install":                  false,
		"your W-2 is ready":            true,
		"":                             false,
	} {
		got := o.Match(&source.Local{Text: map[string]string{"subject": text}}) != ""
		if got != want {
			t.Errorf("%q: matched %v, want %v", text, got, want)
		}
	}
}

func TestSendersAndLabels(t *testing.T) {
	o := owner(t, `{senders: [remotely.works, counsel@law.example], labels: [Confidential]}`)
	for addr, want := range map[string]bool{
		"pay@remotely.works":       true,
		"a@eu.remotely.works":      true,
		"a@notremotely.works":      false,
		"counsel@law.example":      true,
		"other@law.example":        false,
		"remotely.works@gmail.com": false,
	} {
		got := o.Match(&source.Local{Addresses: map[string][]string{"thread_participants": {"me@x.co", addr}}}) != ""
		if got != want {
			t.Errorf("%q: matched %v, want %v", addr, got, want)
		}
	}
	if o.Match(&source.Local{Labels: []string{"CATEGORY_UPDATES", "confidential"}}) == "" {
		t.Error("a label that sorts after a category label, in another case, should match")
	}
	if o.Match(&source.Local{Labels: []string{"Confidential-ish"}}) != "" {
		t.Error("a label matches whole")
	}
	if (*OwnerList)(nil).Match(&source.Local{Labels: []string{"Confidential"}}) != "" || o.Match(nil) != "" {
		t.Error("nothing matches without a list or local text")
	}
}

func TestValidate(t *testing.T) {
	for _, doc := range []string{`{senders: [Remotely.Works]}`, `{senders: ["@x.co"]}`, `{labels: [" "]}`, `{words: [""]}`, `{words: [{glob: x}]}`, `{words: [3]}`} {
		var o OwnerList
		err := yaml.Unmarshal([]byte(doc), &o)
		if err == nil {
			err = o.Validate()
		}
		if err == nil {
			t.Errorf("%s: accepted", doc)
		}
	}
}

func TestLevels(t *testing.T) {
	if Max(None, Agents) != Agents || Max(Owner, Agents) != Owner || Max(Agents, None) != Agents {
		t.Fatal("Max")
	}
	if !Valid("owner") || !Valid("owner+agents") || Valid("") || Valid("agents") {
		t.Fatal("Valid")
	}
}

func TestFloor(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenFloor(dir)
	if err != nil {
		t.Fatal(err)
	}
	const subj = "gmail:winton:thread:t1"
	if err := f.Raise(subj, Agents, true); err != nil {
		t.Fatal(err)
	}
	if err := f.Raise(subj, None, true); err != nil || f.Get(subj) != Agents {
		t.Fatalf("lowered: %v %v", err, f.Get(subj))
	}
	if err := f.Raise(subj, Owner, true); err != nil {
		t.Fatal(err)
	}
	if err := f.Raise(subj, Agents, true); err != nil || f.Get(subj) != Owner {
		t.Fatal("never goes down")
	}
	// Not persisted: held in memory only.
	if err := f.Raise("drive:winton:f9", Owner, false); err != nil || f.Get("drive:winton:f9") != Owner {
		t.Fatal("in-memory raise")
	}
	data, err := os.ReadFile(filepath.Join(dir, FloorFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "winton") || strings.Contains(string(data), "t1") {
		t.Fatalf("the floor holds a subject: %s", data)
	}
	info, _ := os.Stat(filepath.Join(dir, FloorFile))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	// A restart reads it back; the unpersisted raise is gone.
	g, err := ReadFloor(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g.Get(subj) != Owner || g.Get("drive:winton:f9") != None {
		t.Fatalf("after restart: %v %v", g.Get(subj), g.Get("drive:winton:f9"))
	}
	// A read-only floor raises in memory and writes nothing.
	if err := g.Raise("gmail:winton:thread:t2", Owner, true); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(dir, FloorFile))
	if string(again) != string(data) {
		t.Fatal("a read-only floor wrote")
	}
}

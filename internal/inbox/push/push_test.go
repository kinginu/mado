package push

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestItems(t *testing.T) {
	now := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	its, err := Items([]byte(`[{"text":"hello","author":{"username":"bob"},"ts":1760918400,"url":"https://x.com/b/status/1","kind":"tweet"},
		{"body":"second","from":"carol","created_at":"2026-10-19T10:00:00+09:00","title":"Subj"},
		{"message":"third","user":"dave","time":1760918400000}]`), now)
	if err != nil || len(its) != 3 {
		t.Fatalf("%v %v", its, err)
	}
	if its[0].Text != "hello" || its[0].Author != "bob" || its[0].Kind != "tweet" || its[0].URL != "https://x.com/b/status/1" ||
		!its[0].TS.Equal(time.Unix(1760918400, 0)) || !strings.HasPrefix(its[0].ExtID, "sha256:") {
		t.Fatalf("first %+v", its[0])
	}
	if its[1].Text != "second" || its[1].Title != "Subj" || its[1].Author != "carol" || its[1].Kind != "push" ||
		!its[1].TS.Equal(time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("second %+v", its[1])
	}
	if !its[2].TS.Equal(time.UnixMilli(1760918400000)) || its[2].Author != "dave" {
		t.Fatalf("third %+v", its[2])
	}
	// The same object with keys in another order is the same ext_id.
	a, _ := Items([]byte(`{"a":1,"text":"x"}`), now)
	b, _ := Items([]byte(`{"text":"x","a":1}`), now)
	if a[0].ExtID != b[0].ExtID || !a[0].TS.Equal(now) || !json.Valid(a[0].Raw) {
		t.Fatal("canonical ext_id")
	}
	for _, bad := range []string{``, `"x"`, `[1,2]`, `{`, `[null]`} {
		if _, err := Items([]byte(bad), now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTokenAndValidate(t *testing.T) {
	if !CheckToken("abcdefghijklmnop", "abcdefghijklmnop") || CheckToken("abc", "abcdefghijklmnop") || CheckToken("", "") {
		t.Fatal("token compare")
	}
	k := Kind{}
	keys := map[string]string{"ingest": "0123456789abcdef0123", "short": "x"}
	if err := k.Validate(json.RawMessage(`{}`), "ingest", keys); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cfg, ref string }{{`{}`, ""}, {`{}`, "missing"}, {`{}`, "short"}, {`{"x":1}`, "ingest"}} {
		if k.Validate(json.RawMessage(c.cfg), c.ref, keys) == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

package hub

import (
	"fmt"
	"path/filepath"
	"testing"
)

// Tail-first transcript loading (DESIGN §17.14).

const (
	evHuman     = `{"type":"user","message":{"role":"user","content":"prompt %d"}}`
	evHumanText = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"prompt %d"}]}}`
	evChatFile  = `{"type":"user","message":{"role":"user","content":[{"type":"mado_file","file_id":"f","name":"a.png","mime":"image/png"},{"type":"text","text":"look %d"}]}}`
	evResult    = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t%d","content":"ok"}]}}`
	evSubPrompt = `{"type":"user","parent_tool_use_id":"ag","message":{"role":"user","content":[{"type":"text","text":"sub %d"}]}}`
	evAssistant = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer %d"}]}}`
)

// seedEvents stores one event per entry of kinds (seq 1, 2, ...); 'H' is a human
// prompt, 'R' a tool_result, 'S' a subagent prompt, anything else assistant text.
func seedEvents(t *testing.T, st *Store, sid, kinds string) {
	t.Helper()
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range kinds {
		f := evAssistant
		switch k {
		case 'H':
			f = evHuman
		case 'R':
			f = evResult
		case 'S':
			f = evSubPrompt
		}
		if _, err := tx.Exec(`INSERT INTO events(session_id,seq,ts,raw) VALUES(?,?,?,?)`, sid, i+1, now(), fmt.Sprintf(f, i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seqRange(evs []StoredEvent) string {
	if len(evs) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d..%d/%d", evs[0].Seq, evs[len(evs)-1].Seq, len(evs))
}

func TestHumanTurn(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want bool
	}{
		{fmt.Sprintf(evHuman, 1), true},
		{fmt.Sprintf(evHumanText, 1), true},
		{fmt.Sprintf(evChatFile, 1), true},
		{fmt.Sprintf(evResult, 1), false},
		{fmt.Sprintf(evSubPrompt, 1), false},
		{fmt.Sprintf(evAssistant, 1), false},
		{`{"type":"user","parent_tool_use_id":null,"message":{"role":"user","content":"typed"}}`, true},
		{`{"type":"system","subtype":"init","text":"user"}`, false},
		{`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"t"}]}}`, false},
		{`not json "user"`, false},
	} {
		if got := humanTurn([]byte(c.raw)); got != c.want {
			t.Errorf("humanTurn(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestEventsBefore(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, c := range []struct {
		name, kinds string
		before      int64
		want        string
		more        bool
	}{
		// Fewer events than a page: everything, nothing before.
		{"short", "HAAR", 0, "1..4/4", false},
		{"empty", "", 0, "none", false},
		// The page starts at a human turn: no extension.
		{"aligned", "HARAAAAHARRA", 0, "8..12/5", true},
		// Extended back to the prompt at 7 (seq 8 is a tool result).
		{"extend", "HARAAAHRRRAA", 0, "7..12/6", true},
		// Tool results and a subagent prompt are not turns; the prompt is at 2.
		{"skip", "AHRRRSRRRRRRRA", 0, "2..14/13", true},
		// before cuts the newest events off.
		{"before", "HARAAAHRRRAAHAA", 13, "7..12/6", true},
		{"before-start", "HARAAAHRRRAAHAA", 7, "1..6/6", false},
		// No human turn in reach but the start is: the page reaches seq 1.
		{"to-start", "ARRRRRRRRRRR", 0, "1..12/12", false},
		// No human turn within three pages (15 events) before the page: cut at 5.
		{"cap", "H" + repeatRune('R', 20) + "RRRRR", 0, "22..26/5", true},
		// Exactly at the cap: the prompt 15 events back is still found.
		{"cap-edge", "AH" + repeatRune('R', 14) + "RRRRR", 0, "2..21/20", true},
	} {
		sid := "s-" + c.name
		seedEvents(t, st, sid, c.kinds)
		evs, more, err := st.EventsBefore(sid, c.before, 5)
		if err != nil {
			t.Fatal(err)
		}
		if got := seqRange(evs); got != c.want || more != c.more {
			t.Errorf("%s: got %s more=%v, want %s more=%v", c.name, got, more, c.want, c.more)
		}
		for i := 1; i < len(evs); i++ {
			if evs[i].Seq != evs[i-1].Seq+1 {
				t.Errorf("%s: not ascending and contiguous: %s", c.name, seqRange(evs))
			}
		}
	}
}

func repeatRune(r rune, n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

func TestAPIBlocksBefore(t *testing.T) {
	x := newChatHub(t)
	sid := x.newChat(t, "", "")
	// 2,500 events: a prompt every 50 (seq 1, 51, ...), the rest tool results and text.
	kinds := make([]rune, 2500)
	for i := range kinds {
		switch {
		case i%50 == 0:
			kinds[i] = 'H'
		case i%2 == 1:
			kinds[i] = 'A'
		default:
			kinds[i] = 'R'
		}
	}
	seedEvents(t, x.store, sid, string(kinds))
	type page struct {
		Blocks     []apiBlock
		First      int64
		Last       int64
		MoreBefore bool `json:"more_before"`
		More       bool
		Approvals  []apiApproval
	}
	var p page
	for _, q := range []string{"?before=0", "?before="} {
		p = page{}
		if st := x.getJSON(t, "/api/sessions/"+sid+"/blocks"+q, &p); st != 200 {
			t.Fatalf("%s: %d", q, st)
		}
		// The newest 1000 are 1501..2500; 1501 is a prompt already.
		if p.First != 1501 || p.Last != 2500 || !p.MoreBefore || p.Approvals == nil || p.Blocks[0].Kind != "user" || p.Blocks[0].Seq != 1501 {
			t.Fatalf("%s: first %d last %d more_before %v approvals %v first block %+v", q, p.First, p.Last, p.MoreBefore, p.Approvals, p.Blocks[0])
		}
	}
	// The page before 1501 is 501..1500, then 1..500 with nothing before.
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?before=1501", &p)
	if p.First != 501 || p.Last != 1500 || !p.MoreBefore {
		t.Fatalf("before=1501: first %d last %d more_before %v", p.First, p.Last, p.MoreBefore)
	}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?before=501", &p)
	if p.First != 1 || p.Last != 500 || p.MoreBefore || len(p.Blocks) != 500 {
		t.Fatalf("before=501: first %d last %d more_before %v blocks %d", p.First, p.Last, p.MoreBefore, len(p.Blocks))
	}
	// Mid-turn boundary: before=1530 has 530..1529 as its raw page; seq 530 is
	// assistant text, so the page extends to the prompt at 501.
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?before=1530", &p)
	if p.First != 501 || p.Last != 1529 || !p.MoreBefore || p.Blocks[0].Kind != "user" {
		t.Fatalf("before=1530: first %d last %d more_before %v", p.First, p.Last, p.MoreBefore)
	}
	// Forward paging is unchanged.
	p = page{}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?after=0", &p)
	if p.Last != 1000 || !p.More || p.Blocks[0].Seq != 1 {
		t.Fatalf("after=0: last %d more %v", p.Last, p.More)
	}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks?after=2000", &p)
	if p.Last != 2500 || p.More {
		t.Fatalf("after=2000: last %d more %v", p.Last, p.More)
	}
	// An empty session.
	empty := x.newChat(t, "", "")
	p = page{}
	x.getJSON(t, "/api/sessions/"+empty+"/blocks?before=0", &p)
	if p.First != 0 || p.Last != 0 || p.MoreBefore || p.Blocks == nil || len(p.Blocks) != 0 {
		t.Fatalf("empty: %+v", p)
	}
}

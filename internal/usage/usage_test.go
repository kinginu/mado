package usage

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func load(t *testing.T, path string) Stats {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, err := ReadJSONL(f)
	if err != nil {
		t.Fatal(err)
	}
	return a.Stats()
}

// The recorded single-model fixture: plain stream-json lines, no subagents.
func TestRecordedFixture(t *testing.T) {
	s := load(t, "../render/testdata/write_tool.jsonl")
	if len(s.Rows) != 1 || s.Rows[0].Model != "claude-haiku-5-5" || s.Rows[0].Subagent || s.Rows[0].Events != 2 || s.Rows[0].Responses != 2 {
		t.Fatalf("rows %+v", s.Rows)
	}
	if s.Rows[0].Output != 19 || s.Rows[0].Input != 2+9773+12693+2+263+22466 {
		t.Errorf("usage %+v", s.Rows[0])
	}
	if s.AgentCalls != 0 {
		t.Errorf("agent calls %d", s.AgentCalls)
	}
	if len(s.Tokens) != 1 || s.Tokens[0].Name != "claude-haiku-5-5" || s.Tokens[0].Out != 143 {
		t.Errorf("tokens %+v", s.Tokens)
	}
	if got := s.Short(); got != "claude-haiku-5-5 ×2" {
		t.Errorf("short %q", got)
	}
}

// A real aiand-pair session (reduced to its structure): GLM main, one background
// Explore agent on deepseek-v4-flash.
func TestRealAgentSplit(t *testing.T) {
	s := load(t, "testdata/agent_split.spool")
	if got := s.Short(); got != "glm-5.3 ×15, deepseek-v4-flash ×14 (sub)" {
		t.Errorf("short %q", got)
	}
	if s.AgentCalls != 1 {
		t.Errorf("agent calls %d", s.AgentCalls)
	}
	if !strings.Contains(s.Table(), "subagent  deepseek-ai/deepseek-v4-flash") {
		t.Errorf("table:\n%s", s.Table())
	}
}

// Synthetic: subagent turns, a duplicate event, the old Task name, and a resumed
// process whose cumulative modelUsage starts again.
func TestSyntheticSubagents(t *testing.T) {
	lines := []string{
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"zai-org/glm-5.3","usage":{"input_tokens":100,"output_tokens":0},"content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"subagent_type":"implementer"}}]}}`,
		`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m1","model":"zai-org/glm-5.3","usage":{"input_tokens":100,"output_tokens":7},"content":[{"type":"tool_use","id":"t1","name":"Agent","input":{}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"t1","message":{"id":"s1","model":"deepseek-ai/deepseek-v4-pro","content":[{"type":"text","text":"x"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"t1","message":{"id":"s2","model":"deepseek-ai/deepseek-v4-pro","content":[{"type":"tool_use","id":"t2","name":"Task","input":{}}]}}`,
		`{"type":"assistant","parent_tool_use_id":"t2","message":{"model":"Qwen/Qwen3-8B","content":[{"type":"text","text":"y"}]}}`,
		`{"type":"result","modelUsage":{"aiand-glm53":{"inputTokens":10,"outputTokens":5},"aiand-dsv4pro":{"inputTokens":20,"outputTokens":8}}}`,
		`{"type":"result","modelUsage":{"aiand-glm53":{"inputTokens":30,"outputTokens":9}}}`,
		`{"type":"result","modelUsage":{"aiand-glm53":{"inputTokens":4,"outputTokens":1}}}`,
		`not json`,
	}
	a := New()
	for _, l := range lines {
		a.Add([]byte(l))
	}
	a.Add([]byte(lines[2])) // replayed event: one more event, same response
	s := a.Stats()
	if s.AgentCalls != 2 {
		t.Errorf("agent calls %d", s.AgentCalls)
	}
	want := []Row{
		{Model: "zai-org/glm-5.3", Responses: 1, Events: 2, Input: 100, Output: 7},
		{Model: "deepseek-ai/deepseek-v4-pro", Subagent: true, Responses: 2, Events: 3},
		{Model: "Qwen/Qwen3-8B", Subagent: true, Responses: 1, Events: 1},
	}
	if len(s.Rows) != len(want) {
		t.Fatalf("rows %+v", s.Rows)
	}
	for i := range want {
		if s.Rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, s.Rows[i], want[i])
		}
	}
	tok := map[string]TokenRow{}
	for _, r := range s.Tokens {
		tok[r.Name] = r
	}
	if g := tok["aiand-glm53"]; g.Input != 34 || g.Out != 10 {
		t.Errorf("glm tokens across a restart %+v", g)
	}
	if d := tok["aiand-dsv4pro"]; d.Input != 20 || d.Out != 8 {
		t.Errorf("dsv4pro tokens %+v", d)
	}
	if got := s.Short(); got != "glm-5.3 ×2, deepseek-v4-pro ×3 (sub), Qwen3-8B ×1 (sub)" {
		t.Errorf("short %q", got)
	}
}

func TestPrices(t *testing.T) {
	ps := Prices{{Model: "zai-org/glm-5.3", Input: 0.6, Output: 2.2, CacheRead: 0.1}, {Model: "zai-org/*", Input: 1},
		{Model: "zai-org/glm-*", Input: 2}}
	if err := ps.Validate(); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]float64{"zai-org/glm-5.3": 0.6, "zai-org/glm-5.3-flash": 2, "zai-org/x": 1} {
		if p, ok := ps.Lookup(model); !ok || p.Input != want {
			t.Errorf("%s: %+v %v", model, p, ok)
		}
	}
	if _, ok := ps.Lookup("deepseek"); ok {
		t.Error("unknown model priced")
	}
	p, _ := ps.Lookup("zai-org/glm-5.3")
	if c := p.Cost(1_000_000, 1_000_000, 0, 500_000); c < 1.799 || c > 1.801 {
		t.Errorf("cost %v", c)
	}
	if (Prices{{Model: "x", Input: -1}}).Validate() == nil {
		t.Error("negative price accepted")
	}
}

func TestFormatMoney(t *testing.T) {
	for _, c := range []struct {
		cur  string
		x    float64
		want string
	}{
		{"JPY", 1234.4, "¥1,234"}, {"JPY", 0, "¥0"}, {"JPY", 999.5, "¥1,000"}, {"USD", 1.234, "$1.23"},
		{"USD", 1234567.891, "$1,234,567.89"}, {"EUR", 12.5, "EUR 12.50"}, {"USD", 0, "$0.00"},
	} {
		if got := FormatMoney(c.cur, c.x); got != c.want {
			t.Errorf("FormatMoney(%s, %v) = %q, want %q", c.cur, c.x, got, c.want)
		}
	}
}

// A mado_spawn marker starts a new claude process, whose modelUsage counts from zero
// even when it passes the earlier totals.
func TestSpawnMarkerKeepsEarlierTotals(t *testing.T) {
	a := New()
	res := func(in, out int) []byte {
		return []byte(fmt.Sprintf(`{"type":"result","modelUsage":{"m":{"inputTokens":%d,"outputTokens":%d}}}`, in, out))
	}
	a.Add([]byte(`{"type":"mado_spawn","pid":"1"}`))
	a.Add(res(10, 1))
	a.Add(res(20, 2))
	a.Add([]byte(`{"type":"mado_spawn","pid":"2"}`))
	a.Add(res(30, 3))
	if got := a.Stats().Tokens; len(got) != 1 || got[0].Input != 50 || got[0].Out != 5 {
		t.Fatalf("tokens %+v", got)
	}
}

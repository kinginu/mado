package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kinginu/mado/internal/proto"
)

// DESIGN §22: kind = "openai" on upstreams and chat models, no key, timeouts.
func TestOpenAIUpstreamConfig(t *testing.T) {
	dir := t.TempDir()
	load := func(extra string) (*Config, error) {
		p := filepath.Join(dir, "hub.toml")
		os.WriteFile(p, []byte("base_url = \"http://127.0.0.1:1\"\n"+extra), 0o600)
		return LoadConfig(p)
	}
	good := `
[[profiles]]
name = "local-mix"
kind = "router"
main = "local-glm"
[profiles.aliases]
fable = "local-glm"
opus = "ai-ds"
[profiles.upstreams.local-glm]
kind = "openai"
base_url = "http://gpu1:8000/v1"
model = "zai-org/GLM-5.1-FP8"
timeout_first_byte = "180s"
timeout_total = "30m"
context_window = 131072
[profiles.upstreams.ai-ds]
base_url = "https://api.aiand.com"
model = "deepseek-ai/deepseek-v4-pro"
key_ref = "aiand"

[[chat.models]]
name = "ollama"
kind = "openai"
base_url = "http://127.0.0.1:11434/v1"
model = "qwen3:8b"
timeout_first_byte = "120s"
`
	c, err := load(good)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Profiles[0].toProto()
	if err != nil {
		t.Fatal(err)
	}
	if u := p.Upstreams["local-glm"]; u.Kind != proto.UpstreamOpenAI || u.TimeoutFirstByte != "180s" || u.TimeoutTotal != "30m" || u.KeyRef != "" {
		t.Fatalf("upstream %+v", u)
	}
	if u := p.Upstreams["ai-ds"]; u.Kind != "" {
		t.Fatalf("default kind %q", u.Kind)
	}
	ms, err := c.Chat.ChatModels()
	if err != nil || ms[0].Kind != proto.UpstreamOpenAI || ms[0].TimeoutFirstByte != "120s" || ms[0].NativeDocuments() {
		t.Fatalf("chat models %+v %v", ms, err)
	}

	for bad, want := range map[string]string{
		strings.Replace(good, `kind = "openai"`, `kind = "vllm"`, 1):                                        "kind must be anthropic or openai",
		strings.Replace(good, `timeout_first_byte = "180s"`, `timeout_first_byte = "3"`, 1):                 "timeout_first_byte must be",
		strings.Replace(good, `timeout_total = "30m"`, `timeout_total = "1m"`, 1):                           "must not exceed",
		strings.Replace(good, `model = "qwen3:8b"`, "model = \"qwen3:8b\"\nauth_header = \"x-api-key\"", 1): "Bearer",
	} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestParseProfileOpenAI(t *testing.T) {
	sec := &Secrets{Keys: map[string]string{"aiand": "k"}}
	doc := `{"name":"local-mix","kind":"router","main":"local-glm","aliases":{"opus":"ai-ds"},"upstreams":{
	  "local-glm":{"base_url":"http://127.0.0.1:8000/v1","model":"glm","kind":"openai","timeout_first_byte":"180s"},
	  "ai-ds":{"base_url":"https://api.aiand.com","model":"deepseek-v4-pro","key_ref":"aiand","timeout_total":"20m"}}}`
	p, err := ParseProfile([]byte(doc), sec)
	if err != nil {
		t.Fatal(err)
	}
	if u := p.Upstreams["local-glm"]; u.Kind != "openai" || u.TimeoutFirstByte != "180s" {
		t.Fatalf("%+v", u)
	}
	if u := p.Upstreams["ai-ds"]; u.TimeoutTotal != "20m" {
		t.Fatalf("%+v", u)
	}
	// the editor's document carries the new fields back
	b, _ := json.Marshal(docOf(p))
	if !strings.Contains(string(b), `"kind":"openai"`) || !strings.Contains(string(b), `"timeout_total":"20m"`) {
		t.Fatalf("doc %s", b)
	}
	if _, err := ParseProfile([]byte(strings.Replace(doc, `"kind":"openai"`, `"kind":"ollama"`, 1)), sec); err == nil {
		t.Fatal("bad kind accepted")
	}
	m, err := ParseChatModel([]byte(`{"name":"lm","base_url":"http://127.0.0.1:1234/v1","model":"qwen","kind":"openai","timeout_first_byte":"90s"}`), sec)
	if err != nil || m.Kind != "openai" || m.TimeoutFirstByte != "90s" {
		t.Fatalf("%+v %v", m, err)
	}
}

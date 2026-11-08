package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/tools"
)

func TestToolsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hub.toml")
	write := func(tools string) {
		os.WriteFile(cfgPath, []byte(`base_url = "http://127.0.0.1:1"
[chat]
enabled = true
data_dir = "`+filepath.Join(dir, "files")+`"
`+tools), 0o600)
	}
	write(`[chat.tools]
fetch_max_chars = 5000
allow_private = true
[chat.tools.search]
kind = "searxng"
base_url = "http://searx.lan:8080"
allow_private = true
[chat.tools.image]
kind = "openai"
key_ref = "openai"
size = "1536x1024"
`)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	tc := cfg.Chat.Tools
	if tc.FetchMaxChars != 5000 || !tc.AllowPrivate || !tc.Search.AllowPrivate || tc.Image.Model != "gpt-image-1" || !tc.FetchEnabled() {
		t.Fatalf("tools config %+v", tc)
	}
	store, _ := OpenStore(filepath.Join(dir, "hub.db"))
	defer store.Close()
	if _, err := New(cfg, store, &Secrets{Keys: map[string]string{}}, Options{LocalLogin: true}); err == nil ||
		!strings.Contains(err.Error(), `key_ref "openai" not in secrets file`) {
		t.Fatalf("missing image key: %v", err)
	}
	h, err := New(cfg, store, &Secrets{Keys: map[string]string{"openai": "sk"}}, Options{LocalLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if got := strings.Join(h.tools.Names(), ","); got != "web_search,fetch_url,generate_image" || strings.Join(h.tools.CodeNames(), ",") != "web_search,fetch_url" {
		t.Fatalf("names %s", got)
	}
	for _, bad := range []string{"[chat.tools.search]\nkind = \"bing\"\n", "[chat.tools]\nfetch_max_chars = 1\n", "[chat.tools]\nsearch_kind = \"x\"\n"} {
		write(bad)
		if _, err := LoadConfig(cfgPath); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func toolUseStream(id, name, input string) string {
	var b strings.Builder
	ev := func(t, d string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", t, d) }
	ev("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"zai-org/glm-5.3","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"chatcmpl-tool-1","name":"`+name+`","input":{}}}`)
	j, _ := json.Marshal(input)
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(j)+`}}`)
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":100,"output_tokens":20}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

func TestChatGeneratesImage(t *testing.T) {
	img := pngData(t)
	oai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"created":1,"data":[{"b64_json":%q}],"usage":{"input_tokens":10,"output_tokens":1000}}`, base64.StdEncoding.EncodeToString(img))
	}))
	defer oai.Close()
	x := newChatHub(t)
	x.h.tools = &tools.Registry{Imager: &tools.OpenAIImages{BaseURL: oai.URL, Model: "gpt-image-1", Key: "sk"}}
	x.h.runner.Tools = x.h.tools
	n := 0
	x.llm.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		x.llm.mu.Lock()
		x.llm.reqs = append(x.llm.reqs, req)
		x.llm.mu.Unlock()
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, toolUseStream("msg_1", "generate_image", `{"prompt":"a red otter"}`))
			return
		}
		io.WriteString(w, chatStream("msg_2", "Here it is."))
	})
	sid := x.newChat(t, "glm-5.3", "")
	var detail struct{ Tools []string }
	x.getJSON(t, "/api/sessions/"+sid, &detail)
	if strings.Join(detail.Tools, ",") != "generate_image" {
		t.Fatalf("panel tools %v", detail.Tools)
	}
	if st := x.do(t, "POST", "/api/sessions/"+sid+"/send", map[string]any{"text": "draw an otter"}, nil); st != 202 {
		t.Fatalf("send %d", st)
	}
	waitIdle(t, x, sid)
	var blocks struct {
		Blocks []struct {
			Kind, Origin, Text string
			FileID             string `json:"file_id"`
		} `json:"blocks"`
	}
	x.getJSON(t, "/api/sessions/"+sid+"/blocks", &blocks)
	var kinds []string
	fileID := ""
	for _, b := range blocks.Blocks {
		kinds = append(kinds, b.Kind)
		if b.Kind == "file" {
			fileID = b.FileID
			if b.Origin != "generated" {
				t.Errorf("origin %q", b.Origin)
			}
		}
	}
	if strings.Join(kinds, ",") != "user,tool_use,tool_result,file,assistant,result" || fileID == "" {
		t.Fatalf("blocks %v", kinds)
	}
	resp, err := x.c.Get(x.srv.URL + "/files/" + fileID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || string(got) != string(img) {
		t.Fatalf("file %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	f, _ := x.store.File(fileID)
	if f.Origin != "generated" || f.SessionID != sid || f.OwnerSub == "" {
		t.Fatalf("file row %+v", f)
	}
	// The model got a short tool_result, the panel lists the image, usage has its row.
	if s := fmt.Sprint(x.llm.req(1)["messages"]); !strings.Contains(s, "was generated and is shown to the user") || strings.Contains(s, "base64") {
		t.Errorf("second request %s", s)
	}
	var d2 struct{ Files []map[string]any }
	x.getJSON(t, "/api/sessions/"+sid, &d2)
	if len(d2.Files) != 1 || d2.Files[0]["origin"] != "generated" {
		t.Errorf("panel files %v", d2.Files)
	}
	var cnt int
	x.store.db.QueryRow(`SELECT COUNT(*) FROM usage WHERE message_id=? AND model='gpt-image-1' AND output=1000`, "img_"+fileID).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("image usage rows %d", cnt)
	}
}

func TestCodeSessionToolCall(t *testing.T) {
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[{"title":"Sea otter","url":"https://en.wikipedia.org/wiki/Sea_otter","content":"A marine mammal."}]}`)
	}))
	defer search.Close()
	x := newM2(t)
	x.th.h.tools = &tools.Registry{Search: &tools.SearXNG{BaseURL: search.URL, Guard: tools.Guard{AllowPrivate: true}},
		Fetcher: &tools.Fetcher{MaxChars: 20000}, Imager: &tools.OpenAIImages{Model: "gpt-image-1"}}
	csrf := apiCSRF(t, x.c, x.th.srv.URL)
	var created struct{ ID string }
	apiDo(t, x.c, x.th.srv.URL, csrf, "POST", "/api/sessions", map[string]any{"kind": "code", "machine": x.mID, "cwd": "/proj/a",
		"profile": "subscription", "main_model": "opus"}, &created)
	st := x.fw.next(proto.TStartSession).(*proto.StartSession)
	if strings.Join(st.Tools, ",") != "web_search,fetch_url" {
		t.Fatalf("start_session tools %v", st.Tools)
	}
	x.fw.send(&proto.Ack{Ref: st.ID, OK: true})
	x.fw.send(&proto.ToolCall{SessionID: created.ID, CallID: "c1", Name: "web_search", Args: json.RawMessage(`{"query":"otters"}`)})
	r := x.fw.next(proto.TToolResult).(*proto.ToolResult)
	if r.CallID != "c1" || r.IsError || !strings.Contains(r.Content, "1. Sea otter\n   https://en.wikipedia.org/wiki/Sea_otter") {
		t.Fatalf("result %+v", r)
	}
	// The SSRF guard holds for code sessions too; generate_image is chat only.
	x.fw.send(&proto.ToolCall{SessionID: created.ID, CallID: "c2", Name: "fetch_url", Args: json.RawMessage(`{"url":"http://169.254.169.254/latest/meta-data/"}`)})
	r = x.fw.next(proto.TToolResult).(*proto.ToolResult)
	if !r.IsError || !strings.Contains(r.Content, "private") {
		t.Fatalf("metadata fetch %+v", r)
	}
	x.fw.send(&proto.ToolCall{SessionID: created.ID, CallID: "c3", Name: "generate_image", Args: json.RawMessage(`{"prompt":"x"}`)})
	r = x.fw.next(proto.TToolResult).(*proto.ToolResult)
	if !r.IsError || !strings.Contains(r.Content, "not enabled") {
		t.Fatalf("image from code %+v", r)
	}
	// A call for a session of another machine gets no answer.
	x.fw.send(&proto.ToolCall{SessionID: proto.NewID(), CallID: "c4", Name: "web_search", Args: json.RawMessage(`{"query":"x"}`)})
	x.fw.quiet(300 * time.Millisecond)
}

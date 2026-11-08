package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPrivate(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "127.9.9.9": true, "::1": true, "10.1.2.3": true, "172.16.0.1": true, "172.31.255.255": true,
		"192.168.1.1": true, "169.254.169.254": true, "100.64.0.1": true, "100.127.255.254": true, "0.0.0.0": true,
		"::": true, "fe80::1": true, "fc00::1": true, "fd12:3456::1": true, "::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true,
		"64:ff9b::a9fe:a9fe": true, "224.0.0.1": true, "255.255.255.255": true, "198.18.0.1": true,
		"8.8.8.8": false, "1.1.1.1": false, "172.32.0.1": false, "100.128.0.1": false, "2606:4700:4700::1111": false,
		"64:ff9b::808:808": false, "93.184.215.14": false,
	} {
		if got := Private(netip.MustParseAddr(ip)); got != want {
			t.Errorf("Private(%s) = %v", ip, got)
		}
	}
}

func TestGuardRefusesPrivateByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	defer srv.Close()
	f := &Fetcher{}
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), "file:///etc/passwd",
		"ftp://example.com/", "http://user:pw@example.com/", "/relative"} {
		if _, err := f.Fetch(context.Background(), u); err == nil {
			t.Errorf("fetched %s", u)
		}
	}
	_, err := f.Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("error %v", err)
	}
	f.Guard.AllowPrivate = true
	if out, err := f.Fetch(context.Background(), srv.URL); err != nil || !strings.Contains(out, "secret") {
		t.Fatalf("allow_private: %q %v", out, err)
	}
}

func TestGuardRechecksRedirects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("no 127.0.0.2:", err)
	}
	inner := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "internal") }))
	inner.Listener.Close()
	inner.Listener = ln
	inner.Start()
	defer inner.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/literal":
			http.Redirect(w, r, inner.URL+"/x", http.StatusFound)
		case "/ok":
			io.WriteString(w, "public page")
		default:
			http.Redirect(w, r, "/ok", http.StatusFound)
		}
	}))
	defer outer.Close()
	// 127.0.0.1 plays a public address, 127.0.0.2 a private one.
	g := Guard{Blocked: func(ip netip.Addr) bool { return ip == netip.MustParseAddr("127.0.0.2") }}
	f := &Fetcher{Guard: g}
	if out, err := f.Fetch(context.Background(), outer.URL+"/hop"); err != nil || !strings.Contains(out, "public page") ||
		!strings.Contains(out, "URL: "+outer.URL+"/ok") {
		t.Fatalf("public redirect: %q %v", out, err)
	}
	if _, err := f.Fetch(context.Background(), outer.URL+"/literal"); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("redirect to a private literal: %v", err)
	}
	// A name is checked when dialled: localhost resolves to a blocked address.
	g2 := Guard{Blocked: func(ip netip.Addr) bool { return ip.IsLoopback() }}
	if _, err := (&Fetcher{Guard: g2}).Fetch(context.Background(), "http://localhost:"+port+"/"); err == nil ||
		!strings.Contains(err.Error(), "private") {
		t.Fatalf("name resolving to a private address: %v", err)
	}
}

const page = `<!doctype html><html><head><meta charset="utf-8"><title>Otters | Zoo</title>
<script>var tracking = 1;</script><style>p{}</style></head><body>
<nav><a href="/">Home</a> <a href="/about">About</a></nav>
<article><h1>Sea otters</h1>
<p>Sea otters hold hands while they sleep so they do not drift apart. They live in the northern Pacific Ocean
and eat sea urchins, crabs and clams, which they open with stones.</p>
<p>An adult sea otter eats about a quarter of its body weight every day, because it has no blubber and needs the
energy to stay warm in cold water. Its fur is the densest of any animal.</p>
<ul><li>Length: about 1.2 m</li><li>Weight: up to 45 kg</li></ul>
<pre>  indented code</pre>
</article><footer>© Zoo</footer></body></html>`

func TestFetchHTMLAndText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, page)
		case "/sjis":
			w.Header().Set("Content-Type", "text/plain; charset=shift_jis")
			w.Write([]byte{0x82, 0xb1, 0x82, 0xf1, 0x82, 0xc9, 0x82, 0xbf, 0x82, 0xcd}) // こんにちは
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"a":1}`)
		case "/long":
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, strings.Repeat("あ", 3000))
		case "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0, 1, 2})
		case "/404":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := &Fetcher{Guard: Guard{AllowPrivate: true}, MaxChars: 1000}
	out, err := f.Fetch(context.Background(), srv.URL+"/page#frag")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Title: Otters | Zoo\n", "URL: " + srv.URL + "/page\n", "hold hands", "- Length: about 1.2 m", "  indented code"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, bad := range []string{"tracking", "p{}", "About"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q leaked into\n%s", bad, out)
		}
	}
	if out, _ := f.Fetch(context.Background(), srv.URL+"/sjis"); !strings.HasSuffix(out, "こんにちは") {
		t.Errorf("shift_jis: %q", out)
	}
	if out, _ := f.Fetch(context.Background(), srv.URL+"/json"); !strings.HasSuffix(out, `{"a":1}`) || strings.Contains(out, "Title:") {
		t.Errorf("json: %q", out)
	}
	out, _ = f.Fetch(context.Background(), srv.URL+"/long")
	if !strings.Contains(out, "[truncated: 1000 of 3000 characters shown]") || strings.Count(out, "あ") != 1000 {
		t.Errorf("truncation: %d chars", len([]rune(out)))
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/bin"); err == nil || !strings.Contains(err.Error(), "application/octet-stream") {
		t.Errorf("binary: %v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/404"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404: %v", err)
	}
}

func TestSearchBackends(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery+" "+r.Header.Get("X-Subscription-Token"))
		switch r.URL.Path {
		case "/searx/search":
			var res []map[string]string
			for i := 1; i <= 10; i++ {
				res = append(res, map[string]string{"title": fmt.Sprint("Result ", i), "url": fmt.Sprint("https://e.com/", i), "content": "snippet\n  text"})
			}
			json.NewEncoder(w).Encode(map[string]any{"results": res})
		case "/brave":
			io.WriteString(w, `{"web":{"results":[{"title":"Otters &amp; you","url":"https://o.org","description":"All about <strong>otters</strong>"}]}}`)
		case "/forbidden/search":
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	sx := &SearXNG{BaseURL: srv.URL + "/searx/", Guard: Guard{AllowPrivate: true}}
	res, err := sx.Search(context.Background(), "sea otters")
	if err != nil || len(res) != 8 || res[0].Title != "Result 1" {
		t.Fatalf("searxng %v %+v", err, res)
	}
	if got[0] != "/searx/search?format=json&q=sea+otters " {
		t.Errorf("searxng request %q", got[0])
	}
	text := FormatResults("sea otters", res)
	if !strings.HasPrefix(text, "1. Result 1\n   https://e.com/1\n   snippet text\n\n2. Result 2") || strings.Contains(text, "9.") {
		t.Errorf("format\n%s", text)
	}
	if FormatResults("x", nil) != "No results for x." {
		t.Error("empty format")
	}
	// The guard applies to the SearXNG connection too.
	if _, err := (&SearXNG{BaseURL: srv.URL}).Search(context.Background(), "q"); err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("private searxng without allow_private: %v", err)
	}
	if _, err := (&SearXNG{BaseURL: srv.URL + "/forbidden", Guard: Guard{AllowPrivate: true}}).Search(context.Background(), "q"); err == nil ||
		!strings.Contains(err.Error(), "json format") {
		t.Errorf("403: %v", err)
	}
	br := &Brave{Key: "bk", BaseURL: srv.URL + "/brave"}
	res, err = br.Search(context.Background(), "otters")
	if err != nil || len(res) != 1 || res[0].Title != "Otters & you" || res[0].Snippet != "All about otters" {
		t.Fatalf("brave %v %+v", err, res)
	}
	if got[len(got)-1] != "/brave?count=8&q=otters bk" {
		t.Errorf("brave request %q", got[len(got)-1])
	}
}

func pngBytes() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

func TestOpenAIImages(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/generations" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		if body["prompt"] == "bad" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"Your request was rejected by the safety system."}}`)
			return
		}
		fmt.Fprintf(w, `{"created":1,"data":[{"b64_json":%q}],"usage":{"total_tokens":300,"input_tokens":20,"output_tokens":280,"input_tokens_details":{"text_tokens":20,"image_tokens":0}}}`,
			base64.StdEncoding.EncodeToString(pngBytes()))
	}))
	defer srv.Close()
	o := &OpenAIImages{BaseURL: srv.URL, Model: "gpt-image-1", Key: "sk", Size: "1024x1024"}
	img, err := o.Generate(context.Background(), "an otter", "")
	if err != nil || img.Mime != "image/png" || img.Usage == nil || img.Usage.OutputTokens != 280 {
		t.Fatalf("%v %+v", err, img)
	}
	if auth != "Bearer sk" || body["model"] != "gpt-image-1" || body["size"] != "1024x1024" || body["output_format"] != "png" ||
		body["response_format"] != nil || body["n"] != float64(1) {
		t.Errorf("request %v %s", body, auth)
	}
	o.Model = "dall-e-3"
	if _, err := o.Generate(context.Background(), "an otter", "1792x1024"); err != nil || body["response_format"] != "b64_json" ||
		body["output_format"] != nil || body["size"] != "1792x1024" {
		t.Errorf("dall-e request %v %v", body, err)
	}
	if _, err := o.Generate(context.Background(), "bad", ""); err == nil || !strings.Contains(err.Error(), "safety system") ||
		strings.Contains(err.Error(), "sk") {
		t.Errorf("error %v", err)
	}
}

func TestConfigAndRegistry(t *testing.T) {
	keys := map[string]string{"brave": "b", "openai": "o"}
	var c Config
	if err := c.Validate(keys); err != nil {
		t.Fatal(err)
	}
	r := New(c, keys)
	if got := strings.Join(r.Names(), ","); got != "fetch_url" || c.Image.Kind != "none" || c.FetchMaxChars != 20000 {
		t.Fatalf("defaults: %s %+v", got, c)
	}
	off := false
	c = Config{Fetch: &off, Search: SearchConfig{Kind: "brave", KeyRef: "brave"}, Image: ImageConfig{Kind: "openai", KeyRef: "openai"}}
	if err := c.Validate(keys); err != nil {
		t.Fatal(err)
	}
	r = New(c, keys)
	if got := strings.Join(r.Names(), ","); got != "web_search,generate_image" || c.Image.Model != "gpt-image-1" ||
		c.Image.BaseURL != "https://api.openai.com" || strings.Join(r.CodeNames(), ",") != "web_search" {
		t.Fatalf("names %s %+v", got, c)
	}
	defs := r.Defs()
	if len(defs) != 2 || defs[1].Name != "generate_image" || !strings.Contains(string(defs[1].InputSchema), `"enum":["1024x1024","1536x1024","1024x1536","auto"]`) {
		t.Fatalf("defs %+v", defs)
	}
	for _, d := range defs {
		b, _ := json.Marshal(d)
		if strings.Contains(string(b), `"type":"web_search`) || !json.Valid(d.InputSchema) {
			t.Errorf("def %s", b)
		}
	}
	for _, bad := range []Config{
		{Search: SearchConfig{Kind: "google"}}, {Search: SearchConfig{Kind: "searxng", BaseURL: "file:///x"}},
		{Search: SearchConfig{Kind: "brave"}}, {Search: SearchConfig{Kind: "brave", KeyRef: "nope"}},
		{Image: ImageConfig{Kind: "openai"}}, {Image: ImageConfig{Kind: "openai", KeyRef: "openai", Size: "9x9"}},
		{Image: ImageConfig{Kind: "midjourney"}}, {FetchMaxChars: 10},
	} {
		if bad.Validate(keys) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// Run: unknown tools, bad input and validation errors are tool errors.
	ctx := context.Background()
	var te Error
	if _, err := r.Run(ctx, "fetch_url", json.RawMessage(`{"url":"https://x"}`)); !errors.As(err, &te) {
		t.Errorf("disabled tool: %v", err)
	}
	if _, err := r.Run(ctx, "web_search", json.RawMessage(`[1]`)); !errors.As(err, &te) {
		t.Errorf("bad input: %v", err)
	}
	if _, err := r.Run(ctx, "web_search", json.RawMessage(`{"query":"  "}`)); !errors.As(err, &te) {
		t.Errorf("empty query: %v", err)
	}
	if _, err := r.Run(ctx, "generate_image", json.RawMessage(`{"prompt":"x","size":"1x1"}`)); err == nil || !strings.Contains(err.Error(), "size must be") {
		t.Errorf("bad size: %v", err)
	}
	var nilReg *Registry
	if nilReg.Names() != nil {
		t.Error("nil registry has tools")
	}
}

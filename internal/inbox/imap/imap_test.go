package imap

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"golang.org/x/text/encoding/japanese"

	"github.com/kinginu/mado/internal/inbox"
)

var serverTLS *tls.Config

func TestMain(m *testing.M) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake imap"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	serverTLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	testTLSConfig = &tls.Config{RootCAs: pool}
	os.Exit(m.Run())
}

// fake is an in-process IMAP server (go-imap's in-memory backend) that records the
// client's commands.
type fake struct {
	user *imapmemserver.User
	addr *net.TCPAddr
	mu   sync.Mutex
	log  bytes.Buffer
}

func (f *fake) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.log.Write(p)
}

func (f *fake) traffic() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.log.String()
}

func newFake(t *testing.T, starttls bool, folders ...string) *fake {
	t.Helper()
	mem := imapmemserver.New()
	f := &fake{user: imapmemserver.NewUser("me@gmail.com", "app-password")}
	mem.AddUser(f.user)
	for _, name := range append([]string{"INBOX"}, folders...) {
		if err := f.user.Create(name, nil); err != nil {
			t.Fatal(err)
		}
	}
	opts := &imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:        goimap.CapSet{goimap.CapIMAP4rev1: {}},
		Logger:      nopLogger{},
		DebugWriter: f,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if starttls {
		opts.TLSConfig = serverTLS
	} else {
		ln = tls.NewListener(ln, serverTLS)
	}
	srv := imapserver.New(opts)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	f.addr = ln.Addr().(*net.TCPAddr)
	return f
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

type lit struct{ *bytes.Reader }

func (f *fake) add(t *testing.T, folder string, when time.Time, msg string, flags ...goimap.Flag) {
	t.Helper()
	b := []byte(strings.ReplaceAll(msg, "\n", "\r\n"))
	if _, err := f.user.Append(folder, lit{bytes.NewReader(b)}, &goimap.AppendOptions{Time: when, Flags: flags}); err != nil {
		t.Fatal(err)
	}
}

func (f *fake) flags(t *testing.T, folder string) string {
	t.Helper()
	sd, err := f.user.Status(folder, &goimap.StatusOptions{NumUnseen: true, NumMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d/%d", *sd.NumUnseen, *sd.NumMessages)
}

func (f *fake) collector(t *testing.T, extra string) inbox.Collector {
	t.Helper()
	cfg := fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"username":"me@gmail.com","allow_private":true%s}`, f.addr.Port, extra)
	if err := (Kind{}).Validate(json.RawMessage(cfg), "gmail", map[string]string{"gmail": "app-password"}); err != nil {
		t.Fatal(err)
	}
	c, err := Kind{}.New(inbox.Account{Name: "gmail", Kind: "imap", Config: json.RawMessage(cfg)}, "app-password", inbox.Env{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func poll(t *testing.T, c inbox.Collector, cur json.RawMessage) ([]inbox.Item, json.RawMessage, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	items, next, more, err := c.Poll(ctx, cur)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return items, next, more
}

func sjis(t *testing.T, s string) string {
	b, err := japanese.ShiftJIS.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jisWord(t *testing.T, s string) string {
	b, err := japanese.ISO2022JP.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return "=?ISO-2022-JP?B?" + base64.StdEncoding.EncodeToString(b) + "?="
}

const (
	msgOld = `From: Old <old@example.org>
To: me@gmail.com
Subject: too old
Date: Mon, 01 Sep 2026 10:00:00 +0000
Message-ID: <old@example.org>

ancient
`
	msgPlain = `From: =?UTF-8?B?5bGx55Sw5aSq6YOO?= <taro@example.jp>
To: me@gmail.com
Cc: other@example.org
Subject: =?UTF-8?Q?Caf=C3=A9_meeting?=
Date: Mon, 19 Oct 2026 09:00:00 +0900
Message-ID: <plain-1@example.jp>
MIME-Version: 1.0
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Let's meet at the caf=C3=A9 at 10.
Bring the =
slides.
`
	msgHTML = `From: News <news@example.com>
To: me@gmail.com
Subject: Weekly news
Message-ID: <html-1@example.com>
MIME-Version: 1.0
Content-Type: text/html; charset=utf-8

<html><head><style>p{color:red}</style><script>alert(1)</script></head><body><p>Hello <b>reader</b></p><p>Second paragraph</p></body></html>
`
	msgMultipart = `From: Alice <alice@example.org>
To: me@gmail.com
Subject: Report attached
Date: Tue, 20 Oct 2026 08:00:00 +0000
Message-ID: <multi-1@example.org>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: base64

UGxlYXNlIGZpbmQgdGhlIHJlcG9ydCBhdHRhY2hlZC4=
--inner
Content-Type: text/html; charset=utf-8

<p>Please find the <b>report</b> attached.</p>
--inner--
--outer
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename="report.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQKJSVFT0YK
--outer--
`
	msgReply = `From: Bob <bob@example.org>
To: me@gmail.com
Subject: Re: Report attached
Date: Tue, 20 Oct 2026 09:00:00 +0000
Message-ID: <reply-1@example.org>
In-Reply-To: <multi-1@example.org>
References: <multi-1@example.org> <other@example.org>
Content-Type: text/plain

Thanks!
`
	msgNoID = `From: carol@example.org
To: me@gmail.com
Subject: Re: =?UTF-8?B?6L+U5L+hOiBGd2Q6IGx1bmNo?=
Content-Type: text/plain

noted
`
)

func seed(t *testing.T, f *fake, now time.Time) {
	f.add(t, "INBOX", now.AddDate(0, 0, -30), msgOld)
	f.add(t, "INBOX", now.Add(-48*time.Hour), msgPlain)
	f.add(t, "INBOX", now.Add(-47*time.Hour), msgHTML, goimap.FlagFlagged)
	f.add(t, "INBOX", now.Add(-24*time.Hour), msgMultipart)
	f.add(t, "INBOX", now.Add(-23*time.Hour), `From: =?ISO-2022-JP?B?GyRCOzNFRBsoQg==?= <yamada@example.jp>
To: me@gmail.com
Subject: `+jisWord(t, "会議の件")+`
Date: Tue, 20 Oct 2026 10:00:00 +0900
Message-ID: <sjis-1@example.jp>
Content-Type: text/plain; charset=Shift_JIS
Content-Transfer-Encoding: 8bit

`+sjis(t, "明日の会議は十時からです。")+`
`)
	f.add(t, "INBOX", now.Add(-22*time.Hour), msgReply)
	f.add(t, "INBOX", now.Add(-21*time.Hour), msgNoID)
}

func TestPollGmailLike(t *testing.T) {
	now := time.Now()
	f := newFake(t, false)
	seed(t, f, now)
	c := f.collector(t, "")
	items, cur, more := poll(t, c, nil)
	if more || len(items) != 6 {
		t.Fatalf("first poll: %d items, more %v", len(items), more)
	}
	by := map[string]inbox.Item{}
	for _, it := range items {
		if it.Kind != "mail" {
			t.Errorf("kind %q", it.Kind)
		}
		by[it.Title] = it
	}
	var nc Cursor
	json.Unmarshal(cur, &nc)
	fc := nc.Folders["INBOX"]
	if fc.UIDValidity == 0 || fc.LastUID != 7 {
		t.Fatalf("cursor %s", cur)
	}

	p := by["Café meeting"]
	if p.Author != "山田太郎 <taro@example.jp>" || p.Text != "Let's meet at the café at 10.\nBring the slides." {
		t.Errorf("plain: %q %q", p.Author, p.Text)
	}
	if p.ExtID != fmt.Sprintf("INBOX/%d/2", fc.UIDValidity) || p.ThreadKey != "plain-1@example.jp" {
		t.Errorf("plain ids: %q %q", p.ExtID, p.ThreadKey)
	}
	if !p.TS.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("plain ts %v (want the Date header)", p.TS)
	}
	if p.URL != "" {
		t.Errorf("non-Gmail host got a URL %q", p.URL)
	}
	var raw rawMail
	if err := json.Unmarshal(p.Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Folder != "INBOX" || raw.UID != 2 || raw.Headers["Cc"] != "other@example.org" || raw.Headers["Message-ID"] != "<plain-1@example.jp>" || raw.Headers["From"] != "山田太郎 <taro@example.jp>" {
		t.Errorf("plain raw %s", p.Raw)
	}

	h := by["Weekly news"]
	if strings.Contains(h.Text, "alert") || strings.Contains(h.Text, "color") || !strings.Contains(h.Text, "Hello reader") || !strings.Contains(h.Text, "Second paragraph") {
		t.Errorf("html text %q", h.Text)
	}
	if h.Author != "News <news@example.com>" {
		t.Errorf("html author %q", h.Author)
	}
	json.Unmarshal(h.Raw, &raw)
	if len(raw.Flags) != 1 || raw.Flags[0] != `\Flagged` {
		t.Errorf("html flags %v", raw.Flags)
	}
	if h.TS.Sub(now.Add(-47*time.Hour)).Abs() > 2*time.Second {
		t.Errorf("html ts %v (want INTERNALDATE)", h.TS)
	}

	m := by["Report attached"]
	if m.Text != "Please find the report attached." || m.ThreadKey != "multi-1@example.org" {
		t.Errorf("multipart %q %q", m.Text, m.ThreadKey)
	}
	raw = rawMail{}
	json.Unmarshal(m.Raw, &raw)
	if len(raw.Attachments) != 1 || raw.Attachments[0].Filename != "report.pdf" || raw.Attachments[0].MIME != "application/pdf" || raw.Attachments[0].Size == 0 {
		t.Errorf("attachments %+v", raw.Attachments)
	}
	if strings.Contains(string(m.Raw), "JVBER") {
		t.Error("attachment body in raw")
	}

	j := by["会議の件"]
	if j.Text != "明日の会議は十時からです。" || j.Author != "山田 <yamada@example.jp>" {
		t.Errorf("shift_jis: %q %q", j.Text, j.Author)
	}

	r := by["Re: Report attached"]
	if r.ThreadKey != "multi-1@example.org" || r.ThreadKey != m.ThreadKey {
		t.Errorf("reply thread %q", r.ThreadKey)
	}
	n := by["Re: 返信: Fwd: lunch"]
	if n.ThreadKey != "subject:lunch" || n.Author != "carol@example.org" || n.Text != "noted" {
		t.Errorf("no-id: %q %q %q", n.ThreadKey, n.Author, n.Text)
	}
	if n.TS.IsZero() {
		t.Error("zero ts")
	}

	// Second poll: nothing new; then one new message.
	items, cur2, _ := poll(t, c, cur)
	if len(items) != 0 || string(cur2) != string(cur) {
		t.Fatalf("second poll: %d items, cursor %s → %s", len(items), cur, cur2)
	}
	f.add(t, "INBOX", now, `From: Dave <dave@example.org>
Subject: new one
Message-ID: <new@example.org>

fresh
`)
	items, cur3, _ := poll(t, c, cur2)
	if len(items) != 1 || items[0].Title != "new one" || !strings.HasSuffix(items[0].ExtID, "/8") {
		t.Fatalf("third poll: %+v", items)
	}
	json.Unmarshal(cur3, &nc)
	if nc.Folders["INBOX"].LastUID != 8 {
		t.Errorf("cursor %s", cur3)
	}

	// Read-only by construction: nothing became \Seen, no writing command was sent.
	if got := f.flags(t, "INBOX"); got != "8/8" {
		t.Errorf("unseen/total = %s", got)
	}
	tr := f.traffic()
	for _, bad := range []string{" STORE ", " EXPUNGE", " COPY ", " MOVE ", " SELECT ", " APPEND ", " DELETE "} {
		if strings.Contains(strings.ToUpper(tr), bad) {
			t.Errorf("client sent %q", bad)
		}
	}
	if !strings.Contains(tr, "EXAMINE") || !strings.Contains(tr, "BODY.PEEK") || !strings.Contains(tr, "LOGOUT") {
		t.Error("expected EXAMINE, BODY.PEEK and LOGOUT in the session")
	}
}

func TestUIDValidityChange(t *testing.T) {
	now := time.Now()
	f := newFake(t, false)
	f.add(t, "INBOX", now.Add(-time.Hour), msgPlain)
	c := f.collector(t, "")
	items, cur, _ := poll(t, c, nil)
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	old := items[0].ExtID
	// Recreating the mailbox changes UIDVALIDITY; UIDs restart at 1.
	if err := f.user.Delete("INBOX"); err != nil {
		t.Fatal(err)
	}
	f.user.Create("INBOX", nil)
	f.add(t, "INBOX", now.AddDate(0, 0, -10), msgOld)
	f.add(t, "INBOX", now.Add(-time.Hour), msgPlain)
	f.add(t, "INBOX", now.Add(-time.Minute), msgHTML)
	items, cur2, _ := poll(t, c, cur)
	if len(items) != 2 || items[0].ExtID == old || items[0].Title != "Café meeting" {
		t.Fatalf("after UIDVALIDITY change: %+v", items)
	}
	var a, b Cursor
	json.Unmarshal(cur, &a)
	json.Unmarshal(cur2, &b)
	if a.Folders["INBOX"].UIDValidity == b.Folders["INBOX"].UIDValidity || b.Folders["INBOX"].LastUID != 3 {
		t.Fatalf("cursors %s %s", cur, cur2)
	}
}

func TestMaxPerPollPaging(t *testing.T) {
	now := time.Now()
	f := newFake(t, false, "[Gmail]/Important")
	for i := 0; i < 5; i++ {
		f.add(t, "INBOX", now.Add(time.Duration(i-10)*time.Minute), fmt.Sprintf("From: x@example.org\nSubject: m%d\nMessage-ID: <m%d@x>\n\nbody %d\n", i, i, i))
	}
	f.add(t, "[Gmail]/Important", now.Add(-time.Minute), "From: y@example.org\nSubject: important\n\nimp\n")
	c := f.collector(t, `,"folders":["INBOX","[Gmail]/Important"],"max_per_poll":2`)
	var cur json.RawMessage
	var titles []string
	pages := 0
	for {
		items, next, more := poll(t, c, cur)
		pages++
		if len(items) > 3 {
			t.Fatalf("page of %d", len(items))
		}
		for _, it := range items {
			titles = append(titles, it.Title)
		}
		cur = next
		if !more {
			break
		}
		if pages > 10 {
			t.Fatal("no end")
		}
	}
	if got := strings.Join(titles, ","); got != "m0,m1,important,m2,m3,m4" || pages != 3 {
		t.Fatalf("pages %d: %s", pages, got)
	}
	items, _, more := poll(t, c, cur)
	if len(items) != 0 || more {
		t.Fatalf("repeat: %d %v", len(items), more)
	}
}

func TestMissingFolderKeepsOthers(t *testing.T) {
	f := newFake(t, false)
	f.add(t, "INBOX", time.Now(), msgPlain)
	c := f.collector(t, `,"folders":["Nope","INBOX"]`)
	items, next, _, err := c.Poll(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), `"Nope"`) || len(items) != 1 {
		t.Fatalf("items %d err %v", len(items), err)
	}
	var nc Cursor
	json.Unmarshal(next, &nc)
	if _, ok := nc.Folders["INBOX"]; !ok {
		t.Fatalf("cursor %s", next)
	}
}

func TestStartTLS(t *testing.T) {
	f := newFake(t, true)
	f.add(t, "INBOX", time.Now(), msgPlain)
	c := f.collector(t, `,"starttls":true`)
	items, _, _ := poll(t, c, nil)
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	if !strings.Contains(f.traffic(), "STARTTLS") {
		t.Error("no STARTTLS")
	}
}

func TestRefusals(t *testing.T) {
	f := newFake(t, false)
	ctx := context.Background()
	// Wrong password.
	cfg := fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"username":"me@gmail.com","allow_private":true}`, f.addr.Port)
	c, _ := Kind{}.New(inbox.Account{Config: json.RawMessage(cfg)}, "wrong", inbox.Env{})
	if _, _, _, err := c.Poll(ctx, nil); err == nil || !strings.Contains(err.Error(), "login") {
		t.Errorf("bad password: %v", err)
	}
	// Loopback without allow_private: the SSRF guard refuses before dialling.
	cfg = fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"username":"me@gmail.com"}`, f.addr.Port)
	c, _ = Kind{}.New(inbox.Account{Config: json.RawMessage(cfg)}, "app-password", inbox.Env{})
	if _, _, _, err := c.Poll(ctx, nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("loopback: %v", err)
	}
	// An untrusted certificate is refused (no production skip-verify).
	saved := testTLSConfig
	testTLSConfig = nil
	defer func() { testTLSConfig = saved }()
	cfg = fmt.Sprintf(`{"host":"127.0.0.1","port":%d,"username":"me@gmail.com","allow_private":true}`, f.addr.Port)
	c, _ = Kind{}.New(inbox.Account{Config: json.RawMessage(cfg)}, "app-password", inbox.Env{})
	if _, _, _, err := c.Poll(ctx, nil); err == nil || !strings.Contains(err.Error(), "tls") {
		t.Errorf("self-signed accepted: %v", err)
	}
}

func TestValidate(t *testing.T) {
	k := Kind{}
	keys := map[string]string{"gmail": "x"}
	good := `{"host":"imap.gmail.com","port":993,"username":"me@gmail.com","folders":["INBOX","[Gmail]/Starred"],"initial_days":7,"max_per_poll":200}`
	if err := k.Validate(json.RawMessage(good), "gmail", keys); err != nil {
		t.Fatal(err)
	}
	if err := k.Validate(k.Template(), "gmail", keys); err != nil {
		t.Fatalf("template: %v", err)
	}
	if err := k.Validate(json.RawMessage(`{"host":"mail.example.org","port":143,"starttls":true,"username":"u"}`), "gmail", nil); err != nil {
		t.Fatalf("starttls: %v", err)
	}
	if k.Validate(json.RawMessage(good), "", keys) == nil {
		t.Error("missing key_ref accepted")
	}
	if k.Validate(json.RawMessage(good), "nope", keys) == nil {
		t.Error("unknown key_ref accepted")
	}
	for _, bad := range []string{
		`{"host":"imap.gmail.com","port":143,"username":"u"}`, // plaintext
		`{"host":"imap.gmail.com","port":993,"starttls":true,"username":"u"}`,
		`{"host":"","username":"u"}`,
		`{"host":"imap.gmail.com/x","username":"u"}`,
		`{"host":"imap.gmail.com"}`,
		`{"host":"imap.gmail.com","username":"u","insecure":true}`,
		`{"host":"imap.gmail.com","username":"u","folders":["INBOX","INBOX"]}`,
		`{"host":"imap.gmail.com","username":"u","folders":["a\r\nb"]}`,
		`{"host":"imap.gmail.com","username":"u","initial_days":-1}`,
		`{"host":"imap.gmail.com","username":"u","max_per_poll":5000}`,
		`{"host":"imap.gmail.com","username":"u","port":70000}`,
	} {
		if k.Validate(json.RawMessage(bad), "gmail", keys) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"Re: Fwd: Hello":     "hello",
		"RE[2]: re: hello  ": "hello",
		"返信: 転送：会議":          "会議",
		"AW: Termin":         "termin",
		"Report":             "report",
	} {
		if got := normalizeSubject(in); got != want {
			t.Errorf("normalizeSubject(%q) = %q, want %q", in, got, want)
		}
	}
	c := &collector{cfg: Config{Host: "imap.gmail.com"}}
	if !c.isGmail() {
		t.Error("gmail not recognised")
	}
	it := c.toItem("INBOX", 1, &msgMeta{buf: &imapclientBuf}, &goimap.FetchItemBodySection{})
	if it.URL != "https://mail.google.com/mail/u/0/#search/rfc822msgid:a%2Bb%40example.org" {
		t.Errorf("gmail url %q", it.URL)
	}
}

// Package imap is the inbox kind "imap" (DESIGN §23.4): read-only mail collection over
// IMAP with TLS, built for Gmail with an app password. Per folder the cursor keeps
// UIDVALIDITY and the last UID seen; one connection per poll, EXAMINE only, every body
// fetch is BODY.PEEK, so no flag is ever set and nothing is moved or deleted.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	message "github.com/emersion/go-message"
	"github.com/emersion/go-message/charset" // registers the x/text charsets (Shift_JIS, ISO-2022-JP, …) with go-message
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/kinginu/mado/internal/inbox"
	"github.com/kinginu/mado/internal/tools"
)

func init() { inbox.Register(Kind{}) }

// Kind is the imap kind.
type Kind struct{}

// Config is the account config.
type Config struct {
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	Username     string   `json:"username"`
	Folders      []string `json:"folders"`
	InitialDays  int      `json:"initial_days"`
	MaxPerPoll   int      `json:"max_per_poll"`
	StartTLS     bool     `json:"starttls"`
	AllowPrivate bool     `json:"allow_private"`
}

// Cursor is the account cursor: one entry per folder.
type Cursor struct {
	Folders map[string]FolderCursor `json:"folders,omitempty"`
}

// FolderCursor is the state of one folder.
type FolderCursor struct {
	UIDValidity uint32 `json:"uidvalidity"`
	LastUID     uint32 `json:"last_uid"`
}

const (
	defaultInitialDays = 7
	maxInitialDays     = 3650
	defaultMaxPerPoll  = 200
	maxMaxPerPoll      = 1000
	maxFolders         = 50
	// maxPartBytes bounds the text part fetched per message (BODY.PEEK[n]<0.size>);
	// the core clips the decoded text to max_item_bytes anyway.
	maxPartBytes = 1 << 20
	dialTimeout  = 15 * time.Second
)

// testTLSConfig is set by tests only (a self-signed fake server); production always
// verifies the server certificate against the system roots.
var testTLSConfig *tls.Config

var headerFields = []string{"From", "To", "Cc", "Date", "Message-ID", "Subject", "References", "In-Reply-To"}

func (Kind) Name() string { return "imap" }

func (Kind) Template() json.RawMessage {
	return json.RawMessage(`{"host": "imap.gmail.com", "port": 993, "username": "you@gmail.com", "folders": ["INBOX"], "initial_days": 7, "max_per_poll": 200}`)
}

func decode(config json.RawMessage) (Config, error) {
	var c Config
	if err := inbox.DecodeStrict(config, &c); err != nil {
		return c, err
	}
	c.Host = strings.TrimSpace(c.Host)
	if c.Port == 0 {
		c.Port = 993
		if c.StartTLS {
			c.Port = 143
		}
	}
	if len(c.Folders) == 0 {
		c.Folders = []string{"INBOX"}
	}
	if c.InitialDays == 0 {
		c.InitialDays = defaultInitialDays
	}
	if c.MaxPerPoll == 0 {
		c.MaxPerPoll = defaultMaxPerPoll
	}
	return c, nil
}

func (Kind) Validate(config json.RawMessage, keyRef string, keys map[string]string) error {
	c, err := decode(config)
	if err != nil {
		return err
	}
	if c.Host == "" || strings.ContainsAny(c.Host, "/: \t@") && net.ParseIP(c.Host) == nil {
		return errors.New("config: host must be a host name or address (e.g. imap.gmail.com)")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("config: port must be between 1 and 65535")
	}
	if c.Port == 143 && !c.StartTLS {
		return errors.New("config: port 143 is plaintext IMAP; use 993 (implicit TLS) or set \"starttls\": true")
	}
	if c.StartTLS && c.Port == 993 {
		return errors.New("config: port 993 is implicit TLS; starttls is for port 143")
	}
	if strings.TrimSpace(c.Username) == "" {
		return errors.New("config: username is required")
	}
	if len(c.Folders) > maxFolders {
		return fmt.Errorf("config: at most %d folders", maxFolders)
	}
	seen := map[string]bool{}
	for _, f := range c.Folders {
		if strings.TrimSpace(f) == "" || strings.ContainsAny(f, "\r\n\x00") {
			return fmt.Errorf("config: bad folder name %q", f)
		}
		if seen[f] {
			return fmt.Errorf("config: folder %q listed twice", f)
		}
		seen[f] = true
	}
	if c.InitialDays < 1 || c.InitialDays > maxInitialDays {
		return fmt.Errorf("config: initial_days must be between 1 and %d", maxInitialDays)
	}
	if c.MaxPerPoll < 1 || c.MaxPerPoll > maxMaxPerPoll {
		return fmt.Errorf("config: max_per_poll must be between 1 and %d", maxMaxPerPoll)
	}
	if keyRef == "" {
		return errors.New("an imap account needs a key_ref: the secrets-file key whose value is the password (Gmail: an app password)")
	}
	if keys != nil {
		if _, ok := keys[keyRef]; !ok {
			return fmt.Errorf("key_ref %q is not in the secrets file", keyRef)
		}
	}
	return nil
}

func (Kind) New(a inbox.Account, key string, env inbox.Env) (inbox.Collector, error) {
	c, err := decode(a.Config)
	if err != nil {
		return nil, err
	}
	return &collector{cfg: c, password: key, env: env}, nil
}

type collector struct {
	cfg      Config
	password string
	env      inbox.Env
}

func (c *collector) now() time.Time {
	if c.env.Now != nil {
		return c.env.Now()
	}
	return time.Now()
}

// dial connects to the server through the same address check as the HTTP clients
// (tools.Private, after DNS resolution) unless allow_private is set.
func (c *collector) dial(ctx context.Context) (net.Conn, error) {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", c.cfg.Host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: dialTimeout}
	var last error = tools.ErrBlocked
	for _, ip := range ips {
		if !c.cfg.AllowPrivate && tools.Private(ip) {
			continue
		}
		conn, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(ip.Unmap(), uint16(c.cfg.Port)).String())
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

func (c *collector) tlsConfig() *tls.Config {
	var cfg *tls.Config
	if testTLSConfig != nil {
		cfg = testTLSConfig.Clone()
	} else {
		cfg = &tls.Config{}
	}
	cfg.ServerName = c.cfg.Host
	cfg.MinVersion = tls.VersionTLS12
	if !c.cfg.StartTLS {
		cfg.NextProtos = []string{"imap"}
	}
	return cfg
}

func (c *collector) connect(ctx context.Context) (*imapclient.Client, error) {
	raw, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect %s:%d: %w", c.cfg.Host, c.cfg.Port, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	opts := &imapclient.Options{
		TLSConfig:   c.tlsConfig(),
		WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader},
	}
	if c.cfg.StartTLS {
		cl, err := imapclient.NewStartTLS(raw, opts)
		if err != nil {
			return nil, fmt.Errorf("starttls: %w", err)
		}
		return cl, nil
	}
	tc := tls.Client(raw, opts.TLSConfig)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	return imapclient.New(tc, opts), nil
}

func (c *collector) Poll(ctx context.Context, raw json.RawMessage) ([]inbox.Item, json.RawMessage, bool, error) {
	var cur Cursor
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cur)
	}
	cl, err := c.connect(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	stop := context.AfterFunc(ctx, func() { cl.Close() })
	defer stop()
	defer cl.Close()
	if err := cl.WaitGreeting(); err != nil {
		return nil, nil, false, fmt.Errorf("greeting: %w", err)
	}
	if err := cl.Login(c.cfg.Username, c.password).Wait(); err != nil {
		return nil, nil, false, fmt.Errorf("login: %w", err)
	}
	next := Cursor{Folders: map[string]FolderCursor{}}
	var items []inbox.Item
	var folderErrs []error
	more := false
	for _, folder := range c.cfg.Folders {
		fc := cur.Folders[folder]
		got, nfc, fmore, err := c.pollFolder(cl, folder, fc)
		if err != nil {
			var ie *goimap.Error
			if !errors.As(err, &ie) || ctx.Err() != nil {
				// The connection is gone: keep nothing from this poll.
				if ctx.Err() != nil {
					err = fmt.Errorf("%w (%v)", err, ctx.Err())
				}
				return nil, nil, false, fmt.Errorf("folder %q: %w", folder, err)
			}
			// The server refused this folder (NO/BAD): the others still count.
			folderErrs = append(folderErrs, fmt.Errorf("folder %q: %w", folder, err))
			if _, ok := cur.Folders[folder]; ok {
				next.Folders[folder] = fc
			}
			continue
		}
		items = append(items, got...)
		next.Folders[folder] = nfc
		more = more || fmore
	}
	_ = cl.Logout().Wait()
	b, _ := json.Marshal(next)
	return items, b, more && len(folderErrs) == 0, errors.Join(folderErrs...)
}

func (c *collector) pollFolder(cl *imapclient.Client, folder string, fc FolderCursor) ([]inbox.Item, FolderCursor, bool, error) {
	sel, err := cl.Select(folder, &goimap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, fc, false, err
	}
	fresh := fc.UIDValidity == 0 || fc.UIDValidity != sel.UIDValidity
	if fresh {
		if fc.UIDValidity != 0 && c.env.Log != nil {
			c.env.Log.Info("imap: UIDVALIDITY changed, starting over", "folder", folder, "old", fc.UIDValidity, "new", sel.UIDValidity)
		}
		fc = FolderCursor{UIDValidity: sel.UIDValidity}
	}
	var crit goimap.SearchCriteria
	if fresh {
		crit.Since = c.now().AddDate(0, 0, -c.cfg.InitialDays)
	} else {
		var set goimap.UIDSet
		set.AddRange(goimap.UID(fc.LastUID+1), 0) // n+1:*
		crit.UID = []goimap.UIDSet{set}
	}
	var uids []uint32
	if fresh || sel.NumMessages > 0 {
		sd, err := cl.UIDSearch(&crit, nil).Wait()
		if err != nil {
			return nil, fc, false, err
		}
		for _, u := range sd.AllUIDs() {
			// "n:*" always matches the highest UID, even below n.
			if uint32(u) > fc.LastUID {
				uids = append(uids, uint32(u))
			}
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	more := false
	if len(uids) > c.cfg.MaxPerPoll {
		uids = uids[:c.cfg.MaxPerPoll]
		more = true
	}
	var items []inbox.Item
	if len(uids) > 0 {
		items, err = c.fetch(cl, folder, sel.UIDValidity, uids)
		if err != nil {
			return nil, fc, false, err
		}
		fc.LastUID = uids[len(uids)-1]
	}
	// Messages below UIDNEXT that the first search skipped (older than initial_days)
	// stay skipped.
	if fresh && !more && sel.UIDNext > 1 && uint32(sel.UIDNext)-1 > fc.LastUID {
		fc.LastUID = uint32(sel.UIDNext) - 1
	}
	return items, fc, more, nil
}

type msgMeta struct {
	buf  *imapclient.FetchMessageBuffer
	part []int
	sp   *goimap.BodyStructureSinglePart
	text string
}

func (c *collector) fetch(cl *imapclient.Client, folder string, uidValidity uint32, uids []uint32) ([]inbox.Item, error) {
	set := goimap.UIDSet{}
	for _, u := range uids {
		set.AddNum(goimap.UID(u))
	}
	hdr := &goimap.FetchItemBodySection{Specifier: goimap.PartSpecifierHeader, HeaderFields: headerFields, Peek: true}
	bufs, err := cl.Fetch(set, &goimap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, Envelope: true, RFC822Size: true,
		BodyStructure: &goimap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*goimap.FetchItemBodySection{hdr},
	}).Collect()
	if err != nil {
		return nil, err
	}
	sort.Slice(bufs, func(i, j int) bool { return bufs[i].UID < bufs[j].UID })
	metas := make([]*msgMeta, 0, len(bufs))
	// Second round: the chosen text part, grouped by part path, one UID FETCH each.
	groups := map[string][]*msgMeta{}
	for _, b := range bufs {
		if b.UID == 0 {
			continue
		}
		m := &msgMeta{buf: b}
		if b.BodyStructure != nil {
			m.part, m.sp = textPart(b.BodyStructure)
		}
		metas = append(metas, m)
		if m.sp != nil {
			k := partKey(m.part)
			groups[k] = append(groups[k], m)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g := groups[k]
		set := goimap.UIDSet{}
		byUID := map[goimap.UID]*msgMeta{}
		for _, m := range g {
			set.AddNum(m.buf.UID)
			byUID[m.buf.UID] = m
		}
		sec := &goimap.FetchItemBodySection{Part: g[0].part, Peek: true, Partial: &goimap.SectionPartial{Offset: 0, Size: maxPartBytes}}
		got, err := cl.Fetch(set, &goimap.FetchOptions{UID: true, BodySection: []*goimap.FetchItemBodySection{sec}}).Collect()
		if err != nil {
			return nil, err
		}
		for _, b := range got {
			m := byUID[b.UID]
			if m == nil {
				continue
			}
			body := b.FindBodySection(sec)
			if body == nil {
				// Some servers answer a partial fetch without the origin octet.
				body = b.FindBodySection(&goimap.FetchItemBodySection{Part: g[0].part})
			}
			m.text = decodeText(m.sp, body)
		}
	}
	items := make([]inbox.Item, 0, len(metas))
	for _, m := range metas {
		items = append(items, c.toItem(folder, uidValidity, m, hdr))
	}
	return items, nil
}

func partKey(p []int) string {
	s := make([]string, len(p))
	for i, n := range p {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

func isAttachment(sp *goimap.BodyStructureSinglePart) bool {
	if d := sp.Disposition(); d != nil && strings.EqualFold(d.Value, "attachment") {
		return true
	}
	return sp.Filename() != ""
}

// textPart picks the first text/plain part that is not an attachment, else the first
// such text/html part.
func textPart(bs goimap.BodyStructure) ([]int, *goimap.BodyStructureSinglePart) {
	var plainPath, htmlPath []int
	var plain, html *goimap.BodyStructureSinglePart
	bs.Walk(func(path []int, part goimap.BodyStructure) bool {
		sp, ok := part.(*goimap.BodyStructureSinglePart)
		if !ok || isAttachment(sp) {
			return true
		}
		switch sp.MediaType() {
		case "text/plain":
			if plain == nil {
				plain, plainPath = sp, append([]int(nil), path...)
			}
		case "text/html":
			if html == nil {
				html, htmlPath = sp, append([]int(nil), path...)
			}
		}
		return true
	})
	if plain != nil {
		return plainPath, plain
	}
	return htmlPath, html
}

// decodeText undoes the transfer encoding and the charset (go-message with the x/text
// charsets) and turns HTML into text. A truncated base64 tail or an unknown charset
// keeps what could be decoded.
func decodeText(sp *goimap.BodyStructureSinglePart, body []byte) string {
	if sp == nil || len(body) == 0 {
		return ""
	}
	var h textproto.Header
	params := map[string]string{}
	if cs := sp.Params["charset"]; cs != "" {
		params["charset"] = cs
	}
	h.Set("Content-Type", mime.FormatMediaType(sp.MediaType(), params))
	if sp.Encoding != "" {
		h.Set("Content-Transfer-Encoding", sp.Encoding)
	}
	var out []byte
	e, err := message.New(message.Header{Header: h}, bytes.NewReader(body))
	if e == nil {
		out = body
	} else {
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			out = body
		} else {
			out, _ = io.ReadAll(e.Body)
			if len(out) == 0 {
				out = body
			}
		}
	}
	s := strings.ToValidUTF8(string(out), "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if sp.MediaType() == "text/html" {
		return tools.HTMLText(s)
	}
	return strings.TrimSpace(s)
}

type attachment struct {
	Filename string `json:"filename,omitempty"`
	MIME     string `json:"mime"`
	Size     uint32 `json:"size"`
}

func attachments(bs goimap.BodyStructure) []attachment {
	var out []attachment
	if bs == nil {
		return nil
	}
	wd := &mime.WordDecoder{CharsetReader: charset.Reader}
	bs.Walk(func(path []int, part goimap.BodyStructure) bool {
		sp, ok := part.(*goimap.BodyStructureSinglePart)
		if !ok {
			return true
		}
		mt := sp.MediaType()
		if isAttachment(sp) || mt == "message/rfc822" || (!strings.HasPrefix(mt, "text/") && !strings.HasPrefix(mt, "multipart/")) {
			name := sp.Filename()
			if d, err := wd.DecodeHeader(name); err == nil {
				name = d
			}
			out = append(out, attachment{Filename: name, MIME: mt, Size: sp.Size})
		}
		return true
	})
	return out
}

func formatAddr(name, addr string) string {
	name = strings.TrimSpace(name)
	switch {
	case name != "" && addr != "":
		return name + " <" + addr + ">"
	case addr != "":
		return addr
	}
	return name
}

var subjectPrefix = regexp.MustCompile(`(?i)^\s*(re|fwd?|aw|wg|sv|vs|tr|返信|転送)\s*(\[\d+\]|\(\d+\))?\s*[:：]\s*`)

// normalizeSubject strips reply and forward prefixes (Re:, Fwd:, 返信:, 転送:, …),
// repeatedly, and folds case and white space.
func normalizeSubject(s string) string {
	for {
		t := subjectPrefix.ReplaceAllString(s, "")
		if t == s {
			break
		}
		s = t
	}
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

type rawMail struct {
	Folder       string            `json:"folder"`
	UIDValidity  uint32            `json:"uidvalidity"`
	UID          uint32            `json:"uid"`
	Flags        []string          `json:"flags"`
	InternalDate string            `json:"internal_date,omitempty"`
	Size         int64             `json:"size,omitempty"`
	Headers      map[string]string `json:"headers"`
	Attachments  []attachment      `json:"attachments"`
}

func (c *collector) toItem(folder string, uidValidity uint32, m *msgMeta, hdrSec *goimap.FetchItemBodySection) inbox.Item {
	b := m.buf
	var mh mail.Header
	if hb := b.FindBodySection(hdrSec); len(hb) > 0 {
		if h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(hb))); err == nil {
			mh = mail.Header{Header: message.Header{Header: h}}
		}
	}
	env := b.Envelope
	if env == nil {
		env = &goimap.Envelope{}
	}
	text := func(k string) string {
		if s, err := mh.Text(k); err == nil && s != "" {
			return strings.TrimSpace(s)
		}
		return strings.TrimSpace(mh.Get(k))
	}

	subject := text("Subject")
	if subject == "" {
		subject = env.Subject
	}
	author := ""
	if l, err := mh.AddressList("From"); err == nil && len(l) > 0 {
		author = formatAddr(l[0].Name, l[0].Address)
	} else if len(env.From) > 0 {
		author = formatAddr(env.From[0].Name, env.From[0].Addr())
	} else {
		author = text("From")
	}
	msgID := ""
	if id, err := mh.MessageID(); err == nil {
		msgID = id
	}
	if msgID == "" {
		msgID = strings.Trim(env.MessageID, "<> ")
	}
	ts, err := mh.Date()
	if err != nil || ts.IsZero() {
		ts = env.Date
	}
	if ts.IsZero() {
		ts = b.InternalDate
	}
	if ts.IsZero() {
		ts = c.now()
	}

	threadKey := ""
	if refs, err := mh.MsgIDList("References"); err == nil && len(refs) > 0 {
		threadKey = refs[0]
	} else if irt, err := mh.MsgIDList("In-Reply-To"); err == nil && len(irt) > 0 {
		threadKey = irt[0]
	} else if len(env.InReplyTo) > 0 {
		threadKey = env.InReplyTo[0]
	} else if msgID != "" {
		// A thread's first message: its replies name it first in References.
		threadKey = msgID
	} else if ns := normalizeSubject(subject); ns != "" {
		threadKey = "subject:" + ns
	}

	link := ""
	if msgID != "" && c.isGmail() {
		link = "https://mail.google.com/mail/u/0/#search/rfc822msgid:" + url.QueryEscape(msgID)
	}

	flags := make([]string, 0, len(b.Flags))
	for _, f := range b.Flags {
		flags = append(flags, string(f))
	}
	headers := map[string]string{}
	for _, k := range []string{"From", "To", "Cc", "Date", "Message-ID", "Subject", "References"} {
		if v := text(k); v != "" {
			headers[k] = v
		}
	}
	r := rawMail{
		Folder: folder, UIDValidity: uidValidity, UID: uint32(b.UID), Flags: flags,
		Size: b.RFC822Size, Headers: headers, Attachments: attachments(b.BodyStructure),
	}
	if r.Attachments == nil {
		r.Attachments = []attachment{}
	}
	if !b.InternalDate.IsZero() {
		r.InternalDate = b.InternalDate.UTC().Format(time.RFC3339)
	}
	rawJSON, _ := json.Marshal(r)
	return inbox.Item{
		ExtID:     fmt.Sprintf("%s/%d/%d", folder, uidValidity, b.UID),
		Kind:      "mail",
		Author:    author,
		Title:     subject,
		Text:      m.text,
		URL:       link,
		ThreadKey: threadKey,
		TS:        ts.UTC(),
		Raw:       rawJSON,
	}
}

func (c *collector) isGmail() bool {
	h := strings.ToLower(strings.TrimSuffix(c.cfg.Host, "."))
	return h == "imap.gmail.com" || h == "imap.googlemail.com"
}

// Package push is the inbox kind "push" (DESIGN §23.4): no poller; anything that can
// POST JSON (Angelic-Angel, scripts) sends items to /api/inbox/ingest/{account} with
// the account's key_ref value as a bearer token.
package push

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kinginu/mado/internal/inbox"
)

func init() { inbox.Register(Kind{}) }

// Kind is the push kind.
type Kind struct{}

// Limits of one ingest request.
const (
	MaxBody  = 256 << 10
	MaxItems = 500
)

func (Kind) Name() string { return "push" }

func (Kind) Template() json.RawMessage { return json.RawMessage(`{}`) }

func (Kind) Validate(config json.RawMessage, keyRef string, keys map[string]string) error {
	var c struct{}
	if err := inbox.DecodeStrict(config, &c); err != nil {
		return err
	}
	if keyRef == "" {
		return errors.New("a push account needs a key_ref: the secrets-file key whose value is the ingest token")
	}
	if keys != nil {
		v, ok := keys[keyRef]
		if !ok {
			return fmt.Errorf("key_ref %q is not in the secrets file", keyRef)
		}
		if len(v) < 16 {
			return fmt.Errorf("the ingest token (key_ref %q) must be at least 16 characters", keyRef)
		}
	}
	return nil
}

func (Kind) New(inbox.Account, string, inbox.Env) (inbox.Collector, error) {
	return nil, inbox.ErrPassive
}

// CheckToken compares a presented token with the account's, hashed and in constant
// time, so neither the length nor a prefix leaks.
func CheckToken(given, want string) bool {
	if want == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// Items turns an ingest body (a JSON object or an array of objects) into items.
func Items(body []byte, now time.Time) ([]inbox.Item, error) {
	body = bytes.TrimSpace(body)
	var objs []map[string]any
	switch {
	case len(body) > 0 && body[0] == '{':
		var o map[string]any
		if err := json.Unmarshal(body, &o); err != nil {
			return nil, errors.New("the body is not valid JSON")
		}
		objs = []map[string]any{o}
	case len(body) > 0 && body[0] == '[':
		if err := json.Unmarshal(body, &objs); err != nil {
			return nil, errors.New("the body must be a JSON object or an array of objects")
		}
	default:
		return nil, errors.New("the body must be a JSON object or an array of objects")
	}
	if len(objs) > MaxItems {
		return nil, fmt.Errorf("at most %d items per request", MaxItems)
	}
	out := make([]inbox.Item, 0, len(objs))
	for _, o := range objs {
		if o == nil {
			return nil, errors.New("every array element must be an object")
		}
		canon, _ := json.Marshal(o) // map keys sorted: a canonical form
		sum := sha256.Sum256(canon)
		it := inbox.Item{ExtID: "sha256:" + hex.EncodeToString(sum[:]), Raw: canon, Kind: "push"}
		it.Text = firstString(o, "text", "body", "message", "title")
		it.Title = firstString(o, "title", "subject")
		if it.Title == it.Text {
			it.Title = ""
		}
		it.Author = person(o, "author", "from", "user")
		it.URL = firstString(o, "url", "link")
		if k := firstString(o, "kind", "type"); k != "" {
			it.Kind = k
		}
		it.ThreadKey = firstString(o, "thread", "thread_key", "conversation_id")
		it.TS = when(o, now, "ts", "time", "created_at")
		out = append(out, it)
	}
	return out, nil
}

func firstString(o map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := o[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

// person reads an author as a string or an object with name, username or screen_name.
func person(o map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := o[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case map[string]any:
			if s := firstString(v, "name", "username", "screen_name", "handle", "email"); s != "" {
				return s
			}
		}
	}
	return ""
}

// when reads an RFC 3339 time or Unix seconds (milliseconds when large).
func when(o map[string]any, now time.Time, keys ...string) time.Time {
	for _, k := range keys {
		switch v := o[k].(type) {
		case string:
			for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, "2006-01-02 15:04:05"} {
				if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
					return t.UTC()
				}
			}
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return unix(f)
			}
		case float64:
			return unix(v)
		}
	}
	return now.UTC()
}

func unix(f float64) time.Time {
	if f > 1e12 {
		return time.UnixMilli(int64(f)).UTC()
	}
	return time.Unix(int64(f), 0).UTC()
}

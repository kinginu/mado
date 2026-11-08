// Package inbox collects items from outside services into mado (DESIGN §23):
// accounts, the kind registry, the scheduler that polls them, the item store with
// its full-text index, digests and tasks. Each kind lives in its own package under
// internal/inbox/<kind> and registers itself in init(); internal/inbox/all imports
// every kind.
package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Item is one normalized message, post, mail or notification from a collector.
type Item struct {
	ExtID, Kind, Author, Title, Text, URL, ThreadKey string
	TS                                               time.Time
	Raw                                              json.RawMessage
}

// Collector pulls items for one account.
type Collector interface {
	// Poll returns the items newer than the cursor (oldest first) and the next cursor.
	// A partial result with a cursor is fine; the scheduler calls again at once when
	// more is true.
	Poll(ctx context.Context, cursor json.RawMessage) (items []Item, next json.RawMessage, more bool, err error)
}

// Kind is one source type (rss, push, slack, …).
type Kind interface {
	Name() string
	// Validate checks config (unknown fields rejected) and that key_ref, when the kind
	// needs one, exists in keys.
	Validate(config json.RawMessage, keyRef string, keys map[string]string) error
	New(a Account, key string, env Env) (Collector, error)
}

// Templater is optionally implemented by a kind: the config JSON the account editor
// starts from.
type Templater interface {
	Template() json.RawMessage
}

// ErrPassive is returned by New for a kind without a poller (push): the scheduler
// starts no goroutine for its accounts and Poll now is refused.
var ErrPassive = errors.New("this kind receives items and has nothing to poll")

// Env is what a collector gets from the hub.
type Env struct {
	// HTTP is built on the SSRF guard (DESIGN §17.7) unless the account sets
	// allow_private.
	HTTP *http.Client
	Log  *slog.Logger
	Now  func() time.Time
}

// Account is one configured source (a row of inbox_accounts).
type Account struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	OwnerSub  string          `json:"owner_sub"`
	Config    json.RawMessage `json:"config"`
	KeyRef    string          `json:"key_ref"`
	Enabled   bool            `json:"enabled"`
	IntervalS int             `json:"interval_s"`
	Source    string          `json:"source"`
	Cursor    json.RawMessage `json:"-"`
	LastRun   string          `json:"last_run"`
	LastOK    string          `json:"last_ok"`
	LastError string          `json:"last_error"`
	BackoffS  int             `json:"backoff_s"`
	CreatedAt string          `json:"created_at"`
}

// AllowPrivate reads the allow_private flag every kind may have in its config.
func (a *Account) AllowPrivate() bool {
	var c struct {
		AllowPrivate bool `json:"allow_private"`
	}
	_ = json.Unmarshal(a.Config, &c)
	return c.AllowPrivate
}

// Account sources (same rule as profiles).
const (
	SourceConfig = "config"
	SourceUI     = "ui"
)

// Item states.
const (
	StateNew      = "new"
	StateRead     = "read"
	StateArchived = "archived"
)

// Task states and sources.
const (
	TaskProposed  = "proposed"
	TaskOpen      = "open"
	TaskDone      = "done"
	TaskDismissed = "dismissed"
	TaskByModel   = "model"
	TaskByUser    = "user"
)

// Limits.
const (
	MinInterval     = 60
	DefaultInterval = 900
	MaxBackoff      = 3600
	MaxRawBytes     = 256 << 10
	DefaultMaxItem  = 64 << 10
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidName: account names are [A-Za-z0-9._-], at most 64 bytes.
func ValidName(s string) bool { return nameRe.MatchString(s) }

var (
	kindsMu sync.RWMutex
	kinds   = map[string]Kind{}
)

// Register adds a kind; kinds call it from init().
func Register(k Kind) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if _, dup := kinds[k.Name()]; dup {
		panic("inbox: kind registered twice: " + k.Name())
	}
	kinds[k.Name()] = k
}

// Kinds lists the registered kind names, sorted.
func Kinds() []string {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	out := make([]string, 0, len(kinds))
	for n := range kinds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// KindByName returns a registered kind.
func KindByName(name string) (Kind, bool) {
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	k, ok := kinds[name]
	return k, ok
}

// Validate checks an account's shape and its kind's config.
func Validate(a *Account, keys map[string]string) error {
	if !ValidName(a.Name) {
		return fmt.Errorf("account name %q: use [A-Za-z0-9._-], at most 64 characters", a.Name)
	}
	k, ok := KindByName(a.Kind)
	if !ok {
		return fmt.Errorf("account %s: unknown kind %q (known: %v)", a.Name, a.Kind, Kinds())
	}
	if a.IntervalS == 0 {
		a.IntervalS = DefaultInterval
	}
	if a.IntervalS < MinInterval || a.IntervalS > 7*24*3600 {
		return fmt.Errorf("account %s: interval must be between 60 s and 7 days", a.Name)
	}
	if len(a.Config) == 0 {
		a.Config = json.RawMessage(`{}`)
	}
	if err := k.Validate(a.Config, a.KeyRef, keys); err != nil {
		return fmt.Errorf("account %s: %w", a.Name, err)
	}
	return nil
}

// DecodeStrict decodes a kind's config with unknown fields refused.
func DecodeStrict(config json.RawMessage, v any) error {
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("config: %v", err)
	}
	if dec.More() {
		return fmt.Errorf("config: trailing data")
	}
	return nil
}

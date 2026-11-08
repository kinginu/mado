package hub

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // [inbox.digest] timezone works on hosts without zoneinfo

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/inbox"
	_ "github.com/kinginu/mado/internal/inbox/all" // registers the kinds
)

// InboxConfig is [inbox] (DESIGN §23.9).
type InboxConfig struct {
	Enabled       bool                 `toml:"enabled"`
	MaxItemBytes  int                  `toml:"max_item_bytes"`
	RetentionDays int                  `toml:"retention_days"`
	Digest        InboxDigestConfig    `toml:"digest"`
	Accounts      []InboxAccountConfig `toml:"accounts"`
}

// InboxDigestConfig is [inbox.digest].
type InboxDigestConfig struct {
	Executor  string `toml:"executor"`   // claude | chat
	Model     string `toml:"model"`      // chat executor: a [[chat.models]] name; "" = the concierge model
	Machine   string `toml:"machine"`    // claude executor (M16): pin a worker
	MainModel string `toml:"main_model"` // claude executor (M16): --model
	// Schedule is local "HH:MM" times; omitted = 08:00 and 20:00, [] = no schedule.
	Schedule *[]string `toml:"schedule"`
	Timezone string    `toml:"timezone"`
	Language string    `toml:"language"`
	MaxItems int       `toml:"max_items"`
	MaxBytes int       `toml:"max_bytes"`
}

// InboxAccountConfig is one [[inbox.accounts]] entry, seeded like profiles.
type InboxAccountConfig struct {
	Name     string         `toml:"name"`
	Kind     string         `toml:"kind"`
	Owner    string         `toml:"owner"` // a user's sub or e-mail, resolved when used
	Interval string         `toml:"interval"`
	KeyRef   string         `toml:"key_ref"`
	Enabled  *bool          `toml:"enabled"`
	Config   map[string]any `toml:"config"`
}

// Digest executors.
const (
	ExecutorClaude = "claude"
	ExecutorChat   = "chat"
)

func (c *InboxConfig) applyDefaults() error {
	if c.MaxItemBytes == 0 {
		c.MaxItemBytes = inbox.DefaultMaxItem
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 90
	}
	if c.MaxItemBytes < 1024 || c.MaxItemBytes > 1<<20 || c.RetentionDays < 1 {
		return fmt.Errorf("[inbox]: max_item_bytes 1024..1048576, retention_days >= 1")
	}
	d := &c.Digest
	if d.Executor == "" {
		d.Executor = ExecutorClaude
	}
	if d.Executor != ExecutorClaude && d.Executor != ExecutorChat {
		return fmt.Errorf("[inbox.digest] executor must be claude or chat")
	}
	if d.Model != "" && !chat.ValidModelName(d.Model) {
		return fmt.Errorf("[inbox.digest] model must be a [[chat.models]] name")
	}
	if d.MainModel == "" {
		d.MainModel = "sonnet"
	}
	if !validJobModel(d.MainModel) {
		return fmt.Errorf("[inbox.digest] main_model must be fable, opus, sonnet, haiku or a claude-* model id")
	}
	if d.Machine != "" && !validName(d.Machine) {
		return fmt.Errorf("[inbox.digest] machine must be a machine name")
	}
	if d.Schedule == nil {
		d.Schedule = &[]string{"08:00", "20:00"}
	}
	for _, s := range *d.Schedule {
		if _, _, ok := inbox.ParseClock(s); !ok {
			return fmt.Errorf("[inbox.digest] schedule: %q is not HH:MM", s)
		}
	}
	if _, err := d.Location(); err != nil {
		return err
	}
	if d.Language == "" {
		d.Language = "en"
	}
	if len(d.Language) > 32 {
		return fmt.Errorf("[inbox.digest] language: a language name or code")
	}
	if d.MaxItems == 0 {
		d.MaxItems = 400
	}
	if d.MaxBytes == 0 {
		d.MaxBytes = 300 << 10
	}
	if d.MaxItems < 1 || d.MaxItems > 5000 || d.MaxBytes < 4096 || d.MaxBytes > 4<<20 {
		return fmt.Errorf("[inbox.digest]: max_items 1..5000, max_bytes 4096..4194304")
	}
	seen := map[string]bool{}
	for i := range c.Accounts {
		a, err := c.Accounts[i].account()
		if err != nil {
			return err
		}
		if err := inbox.Validate(&a, nil); err != nil { // key_refs are checked against the secrets file in New
			return fmt.Errorf("[[inbox.accounts]] %w", err)
		}
		k := strings.ToLower(a.OwnerSub) + "\x00" + a.Name
		if seen[k] {
			return fmt.Errorf("[[inbox.accounts]] %s: the name is used twice for one owner", a.Name)
		}
		seen[k] = true
	}
	return nil
}

// Location is the digest schedule's time zone ("" = the host's).
func (d *InboxDigestConfig) Location() (*time.Location, error) {
	if d.Timezone == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return nil, fmt.Errorf("[inbox.digest] timezone %q: %v", d.Timezone, err)
	}
	return loc, nil
}

// ScheduleTimes is the schedule after defaults.
func (d *InboxDigestConfig) ScheduleTimes() []string {
	if d.Schedule == nil {
		return []string{"08:00", "20:00"}
	}
	return *d.Schedule
}

func (a InboxAccountConfig) account() (inbox.Account, error) {
	out := inbox.Account{Name: a.Name, Kind: a.Kind, OwnerSub: strings.TrimSpace(a.Owner), KeyRef: a.KeyRef, Enabled: true,
		Source: inbox.SourceConfig}
	if out.OwnerSub == "" {
		return out, fmt.Errorf("[[inbox.accounts]] %s: owner (a user's sub or e-mail) is required", a.Name)
	}
	if a.Enabled != nil {
		out.Enabled = *a.Enabled
	}
	if a.Interval != "" {
		d, err := time.ParseDuration(a.Interval)
		if err != nil || d%time.Second != 0 {
			return out, fmt.Errorf("[[inbox.accounts]] %s: interval %q is not a duration such as \"15m\"", a.Name, a.Interval)
		}
		out.IntervalS = int(d / time.Second)
	}
	cfg := a.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return out, fmt.Errorf("[[inbox.accounts]] %s: config: %v", a.Name, err)
	}
	out.Config = b
	return out, nil
}

var reJobModel = regexp.MustCompile(`^claude-[a-z0-9.-]{1,64}$`)

// validJobModel accepts what the worker accepts as a job's --model (DESIGN §23.6).
func validJobModel(m string) bool {
	switch m {
	case "fable", "opus", "sonnet", "haiku":
		return true
	}
	return reJobModel.MatchString(m)
}

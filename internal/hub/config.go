package hub

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/kinginu/mado/internal/chat"
	"github.com/kinginu/mado/internal/policy"
	"github.com/kinginu/mado/internal/proto"
	"github.com/kinginu/mado/internal/roles"
	"github.com/kinginu/mado/internal/router"
	"github.com/kinginu/mado/internal/tools"
	"github.com/kinginu/mado/internal/usage"
)

// DefaultAdminGroup is the IdP group whose members are admins.
const DefaultAdminGroup = "mado_admins"

// Config is hub.toml.
type Config struct {
	Listen      string          `toml:"listen"`
	BaseURL     string          `toml:"base_url"`
	DBPath      string          `toml:"db_path"`
	SecretsFile string          `toml:"secrets_file"`
	OIDC        OIDCConfig      `toml:"oidc"`
	Allow       AllowConfig     `toml:"allow"`
	Profiles    []ProfileConfig `toml:"profiles"`
	Chat        ChatConfig      `toml:"chat"`
	Prices      usage.Prices    `toml:"prices"`
	Budgets     BudgetsConfig   `toml:"budgets"`
	Auto        AutoConfig      `toml:"auto"`
	Concierge   ConciergeConfig `toml:"concierge"`
	Inbox       InboxConfig     `toml:"inbox"`
}

// ConciergeConfig is [concierge] (DESIGN §20.1). Optional: without it the concierge
// uses the chat default model.
type ConciergeConfig struct {
	// Model is a [[chat.models]] name; "" means the chat default.
	Model string `toml:"model"`
}

func (c *ConciergeConfig) applyDefaults() error {
	if c.Model != "" && !chat.ValidModelName(c.Model) {
		return fmt.Errorf("[concierge] model must be a [[chat.models]] name")
	}
	return nil
}

// AutoConfig is [auto] (DESIGN §18.4): the mado auto mode classifier and extra rules.
type AutoConfig struct {
	// Classifier is a [[chat.models]] name; "" means rules only.
	Classifier string `toml:"classifier"`
	// Allow and Deny are RE2 regexes added to the default rules.
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
	// MaxClassifierPerTurn is deprecated and ignored: the per-turn classifier cap was
	// removed (DESIGN §18.6). The key is still accepted so an old hub.toml loads.
	MaxClassifierPerTurn int `toml:"max_classifier_per_turn"`
}

func (a *AutoConfig) applyDefaults() error {
	if a.Classifier != "" && !chat.ValidModelName(a.Classifier) {
		return fmt.Errorf("[auto] classifier must be a [[chat.models]] name")
	}
	_, err := policy.New(policy.Config{Allow: a.Allow, Deny: a.Deny})
	return err
}

// BudgetsConfig is [budgets] (DESIGN §17.6, §17.11). Prices and budgets are plain
// numbers in Currency; mado converts nothing.
type BudgetsConfig struct {
	Currency string `toml:"currency"` // ISO 4217 code, default "USD"
	// DefaultMonthly is the budget of a user without their own; omitted means no
	// limit (a hub upgraded to M7 must not start refusing work by itself).
	DefaultMonthly *float64 `toml:"default_monthly"`
}

func (b *BudgetsConfig) applyDefaults() error {
	b.Currency = strings.ToUpper(strings.TrimSpace(b.Currency))
	if b.Currency == "" {
		b.Currency = "USD"
	}
	if len(b.Currency) != 3 || strings.Trim(b.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return fmt.Errorf("[budgets] currency must be a 3-letter code such as USD or JPY")
	}
	if b.DefaultMonthly != nil && (*b.DefaultMonthly < 0 || *b.DefaultMonthly != *b.DefaultMonthly) {
		return fmt.Errorf("[budgets] default_monthly must be >= 0")
	}
	return nil
}

// ChatConfig is [chat] (DESIGN §17.1).
type ChatConfig struct {
	Enabled       bool              `toml:"enabled"`
	DataDir       string            `toml:"data_dir"`
	MaxFileMB     int               `toml:"max_file_mb"`
	UserQuotaMB   int               `toml:"user_quota_mb"`
	MaxToolRounds int               `toml:"max_tool_rounds"`
	Models        []ChatModelConfig `toml:"models"`
	// Tools is [chat.tools] (DESIGN §17.7). Code sessions use the same registry.
	Tools tools.Config `toml:"tools"`
}

// ChatModelConfig is one [[chat.models]] entry.
type ChatModelConfig struct {
	Name          string `toml:"name"`
	Label         string `toml:"label"`
	BaseURL       string `toml:"base_url"`
	Model         string `toml:"model"`
	KeyRef        string `toml:"key_ref"`
	AuthHeader    string `toml:"auth_header"`
	ContextWindow int    `toml:"context_window"`
	MaxOutput     int    `toml:"max_output"`
	Vision        bool   `toml:"vision"`
	Documents     *bool  `toml:"documents"` // default: the value of vision
	Default       bool   `toml:"default"`
	// Kind, TimeoutFirstByte, TimeoutTotal: DESIGN §22.1.
	Kind             string `toml:"kind"`
	TimeoutFirstByte string `toml:"timeout_first_byte"`
	TimeoutTotal     string `toml:"timeout_total"`
}

func (c ChatModelConfig) toModel() chat.Model {
	return chat.Model{Name: c.Name, Label: c.Label, BaseURL: c.BaseURL, Model: c.Model, KeyRef: c.KeyRef,
		AuthHeader: c.AuthHeader, ContextWindow: c.ContextWindow, MaxOutput: c.MaxOutput, Vision: c.Vision,
		Documents: c.Documents, Default: c.Default, Kind: c.Kind, TimeoutFirstByte: c.TimeoutFirstByte,
		TimeoutTotal: c.TimeoutTotal}
}

// ChatModels is the [[chat.models]] list, normalized.
func (c ChatConfig) ChatModels() ([]chat.Model, error) {
	out := make([]chat.Model, 0, len(c.Models))
	for _, m := range c.Models {
		out = append(out, m.toModel())
	}
	return out, chat.ValidateModels(out)
}

// applyChatDefaults fills in the [chat] defaults.
func (c *ChatConfig) applyDefaults() error {
	if c.DataDir == "" {
		c.DataDir = "~/.local/share/mado/files"
	}
	c.DataDir = expandHome(c.DataDir)
	if c.MaxFileMB == 0 {
		c.MaxFileMB = 20
	}
	if c.UserQuotaMB == 0 {
		c.UserQuotaMB = 1024
	}
	if c.MaxToolRounds == 0 {
		c.MaxToolRounds = 8
	}
	if c.MaxFileMB < 1 || c.MaxFileMB > 1024 || c.UserQuotaMB < 1 || c.MaxToolRounds < 1 || c.MaxToolRounds > 64 {
		return fmt.Errorf("[chat]: max_file_mb 1..1024, user_quota_mb >= 1, max_tool_rounds 1..64")
	}
	if _, err := c.ChatModels(); err != nil {
		return err
	}
	return c.Tools.Validate(nil) // key_refs are checked against the secrets file in New
}

type OIDCConfig struct {
	Issuer           string   `toml:"issuer"`
	ClientID         string   `toml:"client_id"`
	ClientSecretFile string   `toml:"client_secret_file"` // optional: public client + PKCE works without
	RedirectURL      string   `toml:"redirect_url"`       // default base_url + /callback
	Scopes           []string `toml:"scopes"`
	AdminGroup       string   `toml:"admin_group"` // IdP group whose members are admins
}

type AllowConfig struct {
	Subs   []string `toml:"subs"`
	Emails []string `toml:"emails"`
}

type ProfileConfig struct {
	Name      string                    `toml:"name"`
	Kind      string                    `toml:"kind"`
	Main      string                    `toml:"main"`
	Aliases   map[string]string         `toml:"aliases"`
	Upstreams map[string]UpstreamConfig `toml:"upstreams"`
	Roles     *RolesConfig              `toml:"roles"`
}

// RolesConfig is [profiles.roles] (DESIGN §5). Omitted keys take the defaults.
type RolesConfig struct {
	Enabled     *bool  `toml:"enabled"`
	Implementer string `toml:"implementer"`
	Researcher  string `toml:"researcher"`
	Simple      string `toml:"simple"`
	Prompt      string `toml:"prompt"`
	Enforce     string `toml:"enforce"` // off | edits (default) | all
}

func (r *RolesConfig) toProto() *proto.Roles {
	if r == nil {
		return nil
	}
	return &proto.Roles{Enabled: r.Enabled, Implementer: r.Implementer, Researcher: r.Researcher, Simple: r.Simple, Prompt: r.Prompt, Enforce: r.Enforce}
}

type UpstreamConfig struct {
	BaseURL          string `toml:"base_url"`
	Model            string `toml:"model"`
	KeyRef           string `toml:"key_ref"`
	AuthHeader       string `toml:"auth_header"`
	ContextWindow    int    `toml:"context_window"`
	MaxOutput        int    `toml:"max_output"`
	Kind             string `toml:"kind"` // anthropic (default) | openai (DESIGN §22)
	TimeoutFirstByte string `toml:"timeout_first_byte"`
	TimeoutTotal     string `toml:"timeout_total"`
}

// Secrets is the secrets file: [keys] name = "value". Mode must be 0600.
type Secrets struct {
	Keys map[string]string `toml:"keys"`
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func LoadConfig(path string) (*Config, error) { return loadConfig(path, nil) }

// DefaultLocalSecrets is the secrets file of `mado local` when hub.toml names none.
const DefaultLocalSecrets = "~/.config/mado/secrets.toml"

// LoadLocalConfig is LoadConfig for `mado local` (DESIGN §19.2): listen defaults to
// 127.0.0.1:<port>, base_url to http://<listen>, secrets_file to DefaultLocalSecrets.
// port >= 0 replaces the port of listen (and of a loopback base_url); -1 keeps the file's.
func LoadLocalConfig(path string, defaultPort, port int) (*Config, error) {
	return loadConfig(path, func(c *Config) error {
		if c.Listen == "" {
			c.Listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(defaultPort))
		}
		if port >= 0 {
			host, _, err := net.SplitHostPort(c.Listen)
			if err != nil {
				return fmt.Errorf("listen %q: %w", c.Listen, err)
			}
			c.Listen = net.JoinHostPort(host, strconv.Itoa(port))
			if u, err := url.Parse(c.BaseURL); err == nil && u.Scheme == "http" && loopbackHost(u.Hostname()) {
				c.BaseURL = ""
			}
		}
		if c.BaseURL == "" {
			c.BaseURL = "http://" + c.Listen
		}
		if c.SecretsFile == "" {
			c.SecretsFile = DefaultLocalSecrets
		}
		return nil
	})
}

func loadConfig(path string, defaults func(*Config) error) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("%s: unknown keys %v", path, und)
	}
	if defaults != nil {
		if err := defaults(&c); err != nil {
			return nil, err
		}
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8740"
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("base_url must be an absolute http(s) URL")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.DBPath == "" {
		c.DBPath = "~/.local/share/mado/hub.db"
	}
	c.DBPath = expandHome(c.DBPath)
	c.SecretsFile = expandHome(c.SecretsFile)
	c.OIDC.ClientSecretFile = expandHome(c.OIDC.ClientSecretFile)
	if c.OIDC.RedirectURL == "" {
		c.OIDC.RedirectURL = c.BaseURL + "/callback"
	}
	if len(c.OIDC.Scopes) == 0 {
		c.OIDC.Scopes = []string{"openid", "profile", "email", "groups"}
	}
	if c.OIDC.AdminGroup == "" {
		c.OIDC.AdminGroup = DefaultAdminGroup
	}
	for _, p := range c.Profiles {
		if _, err := p.toProto(); err != nil {
			return nil, err
		}
	}
	if err := c.Chat.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.Prices.Validate(); err != nil {
		return nil, err
	}
	if err := c.Budgets.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.Auto.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.Concierge.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.Inbox.applyDefaults(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (p ProfileConfig) toProto() (proto.Profile, error) {
	out := proto.Profile{Name: p.Name, Kind: p.Kind, Main: p.Main, Aliases: p.Aliases,
		Upstreams: map[string]proto.Upstream{}, Roles: p.Roles.toProto()}
	if err := roles.Validate(out.Roles); err != nil {
		return out, fmt.Errorf("profile %q: %w", p.Name, err)
	}
	for k, u := range p.Upstreams {
		out.Upstreams[k] = proto.Upstream{BaseURL: u.BaseURL, Model: u.Model, KeyRef: u.KeyRef, AuthHeader: u.AuthHeader,
			ContextWindow: u.ContextWindow, MaxOutput: u.MaxOutput, Kind: u.Kind, TimeoutFirstByte: u.TimeoutFirstByte,
			TimeoutTotal: u.TimeoutTotal}
	}
	switch p.Kind {
	case proto.KindRouter:
		if err := router.Validate(out); err != nil {
			return out, fmt.Errorf("profile %q: %w", p.Name, err)
		}
	case proto.KindSubscription:
		out.Upstreams = nil
	default:
		return out, fmt.Errorf("profile %q: kind must be router or subscription", p.Name)
	}
	if !validName(p.Name) {
		return out, fmt.Errorf("profile name %q: use [A-Za-z0-9_-]", p.Name)
	}
	return out, nil
}

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// LoadSecrets reads the secrets file once. A missing path means no keys.
func LoadSecrets(path string) (*Secrets, error) {
	s := &Secrets{Keys: map[string]string{}}
	if path == "" {
		return s, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be mode 0600", path)
	}
	if _, err := toml.DecodeFile(path, s); err != nil {
		return nil, fmt.Errorf("secrets file: parse error") // never echo content
	}
	return s, nil
}

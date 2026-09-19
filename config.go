package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Choice is one option offered on an inline keyboard.
type Choice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Config struct {
	BotToken       string  `json:"bot_token"`
	ChatID         int64   `json:"chat_id"`
	AllowedUserIDs []int64 `json:"allowed_user_ids"`
	// AdminUserIDs may run the whole gateway: the session list, new sessions,
	// every topic. Anyone else the bot answers is a guest, confined to the
	// isolated topics they were given. Empty means every allowed user is an
	// admin, which is what a one-person install wants.
	AdminUserIDs   []int64  `json:"admin_user_ids,omitempty"`
	DefaultCwd     string   `json:"default_cwd"`
	WorkspaceRoots []string `json:"workspace_roots"`
	StatePath      string   `json:"state_path"`
	APIBase        string   `json:"api_base,omitempty"`
	BridgeCmd      []string `json:"bridge_cmd"`
	// IsolatedRoot is where sandboxed sessions get their folders, one per
	// agent and then one per session. Nothing outside it is visible to them.
	IsolatedRoot string `json:"isolated_root,omitempty"`

	EditIntervalMS  int  `json:"edit_interval_ms"`
	MaxMessageChars int  `json:"max_message_chars"`
	ShowThinking    bool `json:"show_thinking"`
	ShowTools       bool `json:"show_tools"`

	ClaudeModels      []Choice `json:"claude_models"`
	CodexModels       []Choice `json:"codex_models"`
	CodexEfforts      []Choice `json:"codex_efforts"`
	AntigravityModels []Choice `json:"antigravity_models"`

	path string
}

const DefaultConfigPath = "/etc/tg-agent-gateway/config.json"

func DefaultConfig() *Config {
	return &Config{
		DefaultCwd:      "/root",
		WorkspaceRoots:  []string{"/root"},
		StatePath:       "/var/lib/tg-agent-gateway/state.json",
		IsolatedRoot:    "/root/isolated",
		APIBase:         "https://api.telegram.org",
		BridgeCmd:       []string{"node", "/opt/tg-agent-gateway/bridge/index.mjs"},
		EditIntervalMS:  2500,
		MaxMessageChars: 3800,
		ShowThinking:    false,
		ShowTools:       true,
		ClaudeModels: []Choice{
			{ID: "", Label: "Default"},
			{ID: "opus", Label: "Opus"},
			{ID: "sonnet", Label: "Sonnet"},
			{ID: "haiku", Label: "Haiku"},
		},
		CodexModels: []Choice{
			{ID: "", Label: "Default"},
			{ID: "gpt-6-astra", Label: "gpt-6-astra"},
			{ID: "gpt-5.1-codex", Label: "gpt-5.1-codex"},
			{ID: "gpt-5.1-codex-mini", Label: "codex-mini"},
		},
		AntigravityModels: []Choice{
			{ID: "", Label: "Default"},
			{ID: "gemini-3.1-pro-high", Label: "Gemini 3.1 Pro (High)"},
			{ID: "gemini-3.8-flash-high", Label: "Gemini 3.8 Flash (High)"},
		},
		CodexEfforts: []Choice{
			{ID: "", Label: "Default"},
			{ID: "low", Label: "Low"},
			{ID: "medium", Label: "Medium"},
			{ID: "high", Label: "High"},
			{ID: "xhigh", Label: "xhigh"},
		},
	}
}

func LoadConfig(path string) (*Config, error) {
	c := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c.ClaudeModels, c.CodexModels, c.CodexEfforts, c.AntigravityModels = nil, nil, nil, nil
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	d := DefaultConfig()
	if len(c.ClaudeModels) == 0 {
		c.ClaudeModels = d.ClaudeModels
	}
	if len(c.CodexModels) == 0 {
		c.CodexModels = d.CodexModels
	}
	if len(c.CodexEfforts) == 0 {
		c.CodexEfforts = d.CodexEfforts
	}
	if len(c.AntigravityModels) == 0 {
		c.AntigravityModels = d.AntigravityModels
	}
	if c.BotToken == "" {
		return nil, errors.New("bot_token is required")
	}
	if c.ChatID == 0 {
		return nil, errors.New("chat_id is required (the forum supergroup, e.g. -1001234567890)")
	}
	if len(c.AllowedUserIDs) == 0 {
		return nil, errors.New("allowed_user_ids must list at least one Telegram user id")
	}
	if c.DefaultCwd == "" {
		c.DefaultCwd = "/root"
	}
	if len(c.WorkspaceRoots) == 0 {
		c.WorkspaceRoots = []string{"/"}
	}
	if c.EditIntervalMS < 1000 {
		c.EditIntervalMS = 1000
	}
	if c.MaxMessageChars < 500 || c.MaxMessageChars > 3900 {
		c.MaxMessageChars = 3800
	}
	if len(c.BridgeCmd) == 0 {
		c.BridgeCmd = d.BridgeCmd
	}
	if c.APIBase == "" {
		c.APIBase = "https://api.telegram.org"
	}
	if c.IsolatedRoot == "" {
		c.IsolatedRoot = d.IsolatedRoot
	}
	c.path = path
	return c, nil
}

func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) UserAllowed(id int64) bool {
	for _, u := range c.AllowedUserIDs {
		if u == id {
			return true
		}
	}
	return false
}

// IsAdmin reports whether this user runs the gateway rather than being a
// guest in one topic of it. With no admin list configured, every allowed
// user is an admin. Explicit administrators must also be in the allowlist.
func (c *Config) IsAdmin(id int64) bool {
	if !c.UserAllowed(id) {
		return false
	}
	if len(c.AdminUserIDs) == 0 {
		return true
	}
	for _, u := range c.AdminUserIDs {
		if u == id {
			return true
		}
	}
	return false
}

// PathAllowed keeps /cd and file writes inside the configured roots, following
// symlinks first so a link cannot lead out of them.
func (c *Config) PathAllowed(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	for _, root := range c.WorkspaceRoots {
		rr, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if r, err := filepath.EvalSymlinks(rr); err == nil {
			rr = r
		}
		if abs == rr || strings.HasPrefix(abs, strings.TrimSuffix(rr, "/")+"/") {
			return true
		}
	}
	return false
}

// Models is only a fallback for when the agent itself cannot be asked; the
// live list comes from the agent.
func (c *Config) Models(agent string) []Choice {
	switch agent {
	case "codex":
		return c.CodexModels
	case "antigravity":
		return c.AntigravityModels
	}
	return c.ClaudeModels
}

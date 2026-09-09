package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Session is what the gateway remembers about one forum topic.
type Session struct {
	ThreadID int    `json:"thread_id"`
	Title    string `json:"title"`               // what the topic is called: "Claude • VPN App"
	Name     string `json:"name"`                // just the part you chose: "VPN App"
	AutoName bool   `json:"auto_name,omitempty"` // the name came from the folder, so it follows /cd
	Agent    string `json:"agent"`               // claude | codex
	Cwd      string `json:"cwd"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Ref      string `json:"ref"` // the agent's own session/thread id, for resume
	Verbose  bool   `json:"verbose"`

	// Config, as offered by the Config button. Empty or zero means "leave the
	// agent's own default alone", except PermMode, whose default is the
	// full-access mode this gateway is built for.
	PermMode     string  `json:"perm_mode,omitempty"`      // claude
	Thinking     int     `json:"thinking,omitempty"`       // claude: maxThinkingTokens
	MaxTurns     int     `json:"max_turns,omitempty"`      // claude
	BudgetUSD    float64 `json:"budget_usd,omitempty"`     // claude
	UserSettings bool    `json:"user_settings,omitempty"`  // claude: load ~/.claude
	Fallback     string  `json:"fallback_model,omitempty"` // claude: switch to this when a message is flagged
	Sandbox      string  `json:"sandbox,omitempty"`        // codex
	Approval     string  `json:"approval,omitempty"`       // codex
	WebSearch    string  `json:"web_search,omitempty"`     // codex: on | off
	Network      string  `json:"network,omitempty"`        // codex: on | off

	Cost     float64   `json:"cost"`
	Turns    int       `json:"turns"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
}

type Store struct {
	path string
	mu   sync.Mutex
	m    map[int]*Session
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: path, m: map[int]*Session{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		var wrapper struct {
			Topics map[string]*Session `json:"topics"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return nil, err
		}
		// A session with thread id 0 comes from an older gateway that ran in
		// the General topic, or from a topic that was deleted. It is kept so
		// that startup can give it a topic of its own.
		for _, sess := range wrapper.Topics {
			if sess != nil {
				s.m[sess.ThreadID] = sess
			}
		}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	wrapper := struct {
		Topics map[string]*Session `json:"topics"`
	}{Topics: map[string]*Session{}}
	for id, sess := range s.m {
		wrapper.Topics[itoa(id)] = sess
	}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Get(threadID int) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[threadID]
	if sess == nil {
		return nil
	}
	cp := *sess
	return &cp
}

func (s *Store) Put(sess *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sess
	s.m[sess.ThreadID] = &cp
	_ = s.saveLocked()
}

// Update mutates a session under the lock and persists it.
func (s *Store) Update(threadID int, fn func(*Session)) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[threadID]
	if sess == nil {
		return nil
	}
	fn(sess)
	_ = s.saveLocked()
	cp := *sess
	return &cp
}

func (s *Store) Delete(threadID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, threadID)
	_ = s.saveLocked()
}

func (s *Store) List() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.m))
	for _, sess := range s.m {
		cp := *sess
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out
}

// RecentDirs lists the working directories in use, most recent first.
func (s *Store) RecentDirs(limit int) []string {
	seen := map[string]bool{}
	var out []string
	for _, sess := range s.List() {
		if sess.Cwd == "" || seen[sess.Cwd] {
			continue
		}
		seen[sess.Cwd] = true
		out = append(out, sess.Cwd)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func itoa(i int) string {
	return json.Number(intToString(i)).String()
}

func intToString(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

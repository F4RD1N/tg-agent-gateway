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

	// Isolated means the session runs behind a sandbox: its own folder under
	// the isolated root, no sight of this machine's files, memories or
	// settings, and no way out of that folder. Root is that folder; Cwd may
	// move about inside it.
	Isolated bool   `json:"isolated,omitempty"`
	Root     string `json:"root,omitempty"`
	// OwnerID, when set, is the one Telegram user this session belongs to.
	// Administrators can also use and manage it. Other users cannot, even
	// when they appear in the allowed list.
	OwnerID int64 `json:"owner_id,omitempty"`

	// Config, as offered by the Config button. Empty or zero means "leave the
	// agent's own default alone", except PermMode, whose default is the
	// full-access mode this gateway is built for.
	PermMode       string  `json:"perm_mode,omitempty"`        // claude
	Thinking       int     `json:"thinking,omitempty"`         // claude: maxThinkingTokens
	MaxTurns       int     `json:"max_turns,omitempty"`        // claude
	BudgetUSD      float64 `json:"budget_usd,omitempty"`       // claude
	NoUserSettings bool    `json:"no_user_settings,omitempty"` // claude: ignore ~/.claude
	Fallback       string  `json:"fallback_model,omitempty"`   // claude: switch to this when a message is flagged
	Sandbox        string  `json:"sandbox,omitempty"`          // codex
	Approval       string  `json:"approval,omitempty"`         // codex
	// Fast is Codex's priority service tier: "on" asks for it, "off" refuses
	// it, empty leaves the machine's own setting alone.
	Fast      string `json:"fast,omitempty"`       // codex
	WebSearch string `json:"web_search,omitempty"` // codex: on | off
	Network   string `json:"network,omitempty"`    // codex: on | off

	Cost     float64   `json:"cost"`
	Turns    int       `json:"turns"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`

	// DetachedAt is set on a session whose topic was deleted while the
	// conversation was deliberately kept. It lives in the archive from then
	// on and can be resumed into a new topic.
	DetachedAt time.Time `json:"detached_at,omitempty"`
}

type Store struct {
	path string
	mu   sync.Mutex
	m    map[int]*Session
	// kept holds sessions whose topic was deleted on purpose, keyed by the
	// id of the topic they used to live in. They are not topics any more, so
	// startup must not give them one.
	kept map[string]*Session
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: path, m: map[int]*Session{}, kept: map[string]*Session{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		var wrapper struct {
			Topics   map[string]*Session `json:"topics"`
			Detached map[string]*Session `json:"detached"`
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
		for id, sess := range wrapper.Detached {
			if sess != nil {
				s.kept[id] = sess
			}
		}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	wrapper := struct {
		Topics   map[string]*Session `json:"topics"`
		Detached map[string]*Session `json:"detached,omitempty"`
	}{Topics: map[string]*Session{}, Detached: map[string]*Session{}}
	for id, sess := range s.m {
		wrapper.Topics[itoa(id)] = sess
	}
	for id, sess := range s.kept {
		wrapper.Detached[id] = sess
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

// Detach moves a session out of its topic and into the archive, so the topic
// can be deleted while the conversation stays resumable. It returns the
// archived copy, or nil if there was nothing in that topic.
func (s *Store) Detach(threadID int) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[threadID]
	if sess == nil {
		return nil
	}
	delete(s.m, threadID)
	sess.DetachedAt = time.Now()
	s.kept[itoa(threadID)] = sess
	_ = s.saveLocked()
	cp := *sess
	return &cp
}

// Kept lists the sessions whose topics were deleted on purpose, newest first.
func (s *Store) Kept() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.kept))
	for _, sess := range s.kept {
		cp := *sess
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DetachedAt.After(out[j].DetachedAt) })
	return out
}

// TakeKept removes an archived session and returns it, for resuming into a
// topic of its own.
func (s *Store) TakeKept(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.kept[id]
	if sess == nil {
		return nil
	}
	delete(s.kept, id)
	_ = s.saveLocked()
	cp := *sess
	return &cp
}

// PutKept returns a session to the archive, for when resuming it could not be
// finished and losing it would be worse than leaving it where it was.
func (s *Store) PutKept(id string, sess *Session) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sess
	s.kept[id] = &cp
	_ = s.saveLocked()
}

// KeptID is the archive key for a session: the topic it used to live in.
func KeptID(sess *Session) string { return itoa(sess.ThreadID) }

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

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

// ModelInfo is one row of an agent's real model list.
type ModelInfo struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Description   string   `json:"description"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort"`
}

// Event is one normalised message from the Node bridge.
type Event struct {
	Type    string `json:"type"`
	SID     string `json:"sid"`
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
	Model   string `json:"model,omitempty"`
	Text    string `json:"text,omitempty"`
	Message string `json:"message,omitempty"`

	// tool
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail,omitempty"`
	Status string `json:"status,omitempty"`

	// file
	Path string `json:"path,omitempty"`
	Kind string `json:"kind,omitempty"`
	OK   bool   `json:"ok,omitempty"`

	// todo
	Items []struct {
		Text string `json:"text"`
		Done bool   `json:"done"`
	} `json:"items,omitempty"`

	// models
	Models []ModelInfo `json:"models,omitempty"`

	// done
	Cost       float64 `json:"cost,omitempty"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	Turns      int     `json:"turns,omitempty"`
	Subtype    string  `json:"subtype,omitempty"`
	Queued     int     `json:"queued,omitempty"`
	Tokens     *struct {
		Input  int `json:"input"`
		Output int `json:"output"`
	} `json:"tokens,omitempty"`
}

// Command is one line written to the bridge.
type Command struct {
	Type   string `json:"type"`
	SID    string `json:"sid,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Resume string `json:"resume,omitempty"`
	Text   string `json:"text,omitempty"`

	PermMode     string  `json:"perm_mode,omitempty"`
	Thinking     int     `json:"thinking,omitempty"`
	MaxTurns     int     `json:"max_turns,omitempty"`
	BudgetUSD    float64 `json:"budget_usd,omitempty"`
	UserSettings bool    `json:"user_settings,omitempty"`
	Fallback     string  `json:"fallback_model,omitempty"`
	Sandbox      string  `json:"sandbox,omitempty"`
	Approval     string  `json:"approval,omitempty"`
	WebSearch    string  `json:"web_search,omitempty"`
	Network      string  `json:"network,omitempty"`
}

// Bridge owns the Node sidecar: one process, many sessions, restarted if it
// ever dies. Events are fanned out to whoever is listening for that sid.
type Bridge struct {
	argv []string

	mu      sync.Mutex
	stdin   io.WriteCloser
	cmd     *exec.Cmd
	subs    map[string]chan Event
	started bool
	alive   bool
	restart int
}

func NewBridge(argv []string) *Bridge {
	return &Bridge{argv: argv, subs: map[string]chan Event{}}
}

func (b *Bridge) Start() error {
	b.mu.Lock()
	if b.started {
		b.mu.Unlock()
		return nil
	}
	b.started = true
	b.mu.Unlock()
	return b.spawn()
}

func (b *Bridge) spawn() error {
	cmd := exec.Command(b.argv[0], b.argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}
	b.mu.Lock()
	b.cmd = cmd
	b.stdin = stdin
	b.alive = true
	b.mu.Unlock()
	log.Printf("bridge started (pid %d)", cmd.Process.Pid)

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			log.Printf("bridge stderr: %s", truncate(sc.Text(), 400))
		}
	}()

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 256*1024), 8<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal(line, &ev); err != nil {
				log.Printf("bridge: bad line: %s", truncate(string(line), 200))
				continue
			}
			b.dispatch(ev)
		}
		err := cmd.Wait()
		b.mu.Lock()
		b.alive = false
		b.restart++
		attempt := b.restart
		for sid, ch := range b.subs {
			select {
			case ch <- Event{Type: "error", SID: sid, Message: "the agent bridge stopped; the next message restarts it"}:
			default:
			}
			select {
			case ch <- Event{Type: "done", SID: sid, Subtype: "bridge_down"}:
			default:
			}
		}
		b.mu.Unlock()
		log.Printf("bridge exited (%v), restarting", err)
		delay := time.Duration(attempt) * time.Second
		if delay > 15*time.Second {
			delay = 15 * time.Second
		}
		time.Sleep(delay)
		if err := b.spawn(); err != nil {
			log.Printf("bridge restart failed: %v", err)
		}
	}()
	return nil
}

func (b *Bridge) dispatch(ev Event) {
	b.mu.Lock()
	ch := b.subs[ev.SID]
	b.mu.Unlock()
	if ch == nil {
		if ev.Type != "hello" && ev.Type != "pong" && ev.Type != "ok" {
			log.Printf("bridge: unrouted %s for sid %q: %s", ev.Type, ev.SID, truncate(ev.Message+ev.Text, 200))
		}
		return
	}
	select {
	case ch <- ev:
	case <-time.After(30 * time.Second):
		log.Printf("bridge: dropping %s for sid %s (listener stuck)", ev.Type, ev.SID)
	}
}

// Subscribe hands out a fresh channel for sid and makes it the live one, so a
// turn never inherits events left over from the turn before it.
func (b *Bridge) Subscribe(sid string) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan Event, 256)
	b.subs[sid] = ch
	return ch
}

// Unsubscribe drops the channel only if it is still the live one. A turn that
// is slow to finish must not tear down the subscription of the turn that
// followed it, which would leave that one waiting for events forever.
func (b *Bridge) Unsubscribe(sid string, ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.subs[sid]; ok && (ch == nil || cur == ch) {
		delete(b.subs, sid)
	}
}

func (b *Bridge) Send(c Command) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.alive || b.stdin == nil {
		return fmt.Errorf("the agent bridge is not running")
	}
	_, err = b.stdin.Write(append(data, '\n'))
	return err
}

func (b *Bridge) Alive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.alive
}

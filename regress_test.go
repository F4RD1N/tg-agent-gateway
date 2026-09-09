package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// A kill must free the topic even if the bridge never sends a paired "done".
func TestKillAloneFreesTheTopic(t *testing.T) {
	gw, _, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 101, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.mu.Lock()
	gw.running[101] = true
	gw.mu.Unlock()
	turn := gw.NewTurn(gw.store.Get(101))
	sid := sidOf(101)
	ch := gw.bridge.Subscribe(sid)
	go func() {
		time.Sleep(150 * time.Millisecond)
		ch <- Event{Type: "killed", SID: sid, Message: "killed"}
	}()
	done := make(chan bool, 1)
	go func() { gw.pump(gw.store.Get(101), turn, "hi", nil); done <- true }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never ended after a kill")
	}
	gw.mu.Lock()
	running := gw.running[101]
	gw.mu.Unlock()
	if running {
		t.Fatal("the topic is still marked busy after a kill")
	}
}

// Two things arriving at once must not start two turns in one topic.
func TestOnlyOneTurnPerTopic(t *testing.T) {
	gw, _, work := newTestGateway(t)
	sess := &Session{ThreadID: 102, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()}
	gw.store.Put(sess)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); gw.submit(gw.store.Get(102), "SLOW one") }()
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond)
	gw.mu.Lock()
	n := len(gw.running)
	gw.mu.Unlock()
	if n != 1 {
		t.Fatalf("expected one running turn, got %d", n)
	}
}

// Changing the agent while a turn runs must be refused, not applied under it.
func TestNoAgentSwitchWhileRunning(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 103, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(103, "SLOW one"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "thinking about it", 10*time.Second)
	gw.handleUpdate(press(103, 1002, "agt:codex"))
	f.waitFor(t, "sendMessage", "while it is working", 5*time.Second)
	if s := gw.store.Get(103); s == nil || s.Agent != "claude" {
		t.Fatalf("the agent should not have changed mid-turn: %+v", gw.store.Get(103))
	}
	gw.handleUpdate(press(103, 1002, "kill"))
}

// The cached skill list is shared, so callers must get their own copy.
func TestSkillCacheIsNotSharedMutable(t *testing.T) {
	gw, _, _ := newTestGateway(t)
	a, err := gw.fetchSkills("claude")
	if err != nil {
		t.Fatal(err)
	}
	b, err := gw.fetchSkills("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("no skills came back")
	}
	a[0].Name = "mutated"
	if b[0].Name == "mutated" {
		t.Fatal("two callers share one backing array")
	}
}

// splitHTML must always shrink the message, whatever it is given.
func TestSplitHTMLAlwaysMakesProgress(t *testing.T) {
	cases := []string{
		"<a href=\"" + strings.Repeat("x", 400) + "\">link</a>",
		strings.Repeat("<b>", 200) + "text",
		strings.Repeat("plain ", 200),
		"<pre>" + strings.Repeat("code\n", 200) + "</pre>",
	}
	for _, in := range cases {
		head, tail := splitHTML(in, 100)
		if head == "" {
			t.Errorf("head is empty for %q", truncate(in, 40))
		}
		if len([]rune(tail)) >= len([]rune(in)) && head == "" {
			t.Errorf("no progress for %q", truncate(in, 40))
		}
	}
}

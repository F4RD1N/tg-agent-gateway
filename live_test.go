package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveAgents drives the real SDK bridge (real Claude Code and Codex turns)
// into the real renderer, with only Telegram faked. It costs a little money,
// so it is opt-in:
//
//	AGENT_LIVE=1 go test -run TestLiveAgents -v -timeout 10m
func TestLiveAgents(t *testing.T) {
	if os.Getenv("AGENT_LIVE") == "" {
		t.Skip("set AGENT_LIVE=1 to run turns against the real agents")
	}
	for _, agent := range []string{"claude", "codex", "antigravity"} {
		t.Run(agent, func(t *testing.T) {
			gw, f, work := newTestGateway(t)
			// Swap the fake bridge for the real one.
			gw.bridge = NewBridge([]string{"node", "bridge/index.mjs"})
			if err := gw.bridge.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(15 * time.Second)
			for !gw.bridge.Alive() && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			thread := 31
			gw.store.Put(&Session{ThreadID: thread, Agent: agent, Cwd: work, Created: time.Now(), LastUsed: time.Now()})

			gw.handleUpdate(msg(thread, "Run the shell command `echo LIVE-"+strings.ToUpper(agent)+"-OK`, then reply with exactly the word FINISHED."))
			c := f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "FINISHED", 5*time.Minute)
			// The narration is what shows; the command behind it must not.
			if txt, _ := c.Params["text"].(string); strings.Contains(txt, "echo LIVE-") {
				t.Errorf("the command line leaked into the message: %q", truncate(txt, 200))
			}

			var sess *Session
			deadline = time.Now().Add(60 * time.Second)
			for time.Now().Before(deadline) {
				sess = gw.store.Get(thread)
				if sess != nil && sess.Ref != "" && sess.Turns > 0 {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if sess == nil || sess.Ref == "" {
				t.Fatalf("no resumable session id was stored: %+v", sess)
			}
			t.Logf("%s session %s, %d turn(s), $%.4f", agent, sess.Ref, sess.Turns, sess.Cost)

			// A second turn must resume the same conversation, so count from here.
			mark := f.count()
			gw.handleUpdate(msg(thread, "What word did you just say? Reply with only that word."))
			deadline = time.Now().Add(5 * time.Minute)
			var s2 *Session
			for time.Now().Before(deadline) {
				s2 = gw.store.Get(thread)
				if s2 != nil && s2.Turns >= 2 {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			if s2 == nil || s2.Turns < 2 {
				t.Fatalf("second turn was not recorded: %+v", s2)
			}
			if s2.Ref != sess.Ref {
				t.Errorf("resume should keep the same session id: %s -> %s", sess.Ref, s2.Ref)
			}
			found := false
			for _, c := range f.since(mark) {
				if txt, _ := c.Params["text"].(string); strings.Contains(txt, "FINISHED") {
					found = true
					break
				}
			}
			if !found {
				t.Error("the resumed turn did not remember the word from the first turn")
			}
		})
	}
}

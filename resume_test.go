package main

import (
	"testing"
	"time"
)

// A topic can be pointed at any conversation id, which is the way back when
// an agent abandons a thread.
func TestResumeCommandAttachesAConversation(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 111, Agent: "codex", Cwd: work, Ref: "old-one", Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(111, "/session"))
	f.waitFor(t, "sendMessage", "old-one", 3*time.Second)

	gw.handleUpdate(msg(111, "/resume 01a07940-34b2-7a03-aacf-127a81ee3f2b"))
	f.waitFor(t, "sendMessage", "01a07940-34b2-7a03-aacf-127a81ee3f2b", 3*time.Second)
	if s := gw.store.Get(111); s == nil || s.Ref != "01a07940-34b2-7a03-aacf-127a81ee3f2b" {
		t.Fatalf("the conversation was not attached: %+v", gw.store.Get(111))
	}
}

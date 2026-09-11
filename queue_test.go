package main

import (
	"strings"
	"testing"
	"time"
)

// ---------- writing to a topic that is already working ----------

func TestABusyTopicOffersTheThreeChoices(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "Busy", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	// SLOW keeps the fake agent working, so the next message lands mid-turn.
	gw.handleUpdate(msg(thread, "SLOW please"))
	f.waitFor(t, "sendMessage", "thinking about it", 5*time.Second)

	gw.handleUpdate(msg(thread, "and also fix the header"))
	offer := f.waitFor(t, "sendMessage", "still working", 3*time.Second)
	b := buttons(offer)
	for _, want := range []string{"q:add", "q:now", "q:drop"} {
		if !has(b, want) {
			t.Fatalf("a message sent mid-turn should offer %s, got %v", want, b)
		}
	}
	if !strings.Contains(text(offer), "fix the header") {
		t.Fatalf("the offer should quote the message it is about: %s", text(offer))
	}
}

func TestQueuedMessagesRunInOrder(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "Queue", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	gw.handleUpdate(msg(thread, "SLOW please"))
	f.waitFor(t, "sendMessage", "thinking about it", 5*time.Second)

	// Two messages, both queued.
	gw.handleUpdate(msg(thread, "first extra"))
	one := f.waitFor(t, "sendMessage", "first extra", 3*time.Second)
	gw.handleUpdate(press(thread, one.ID, "q:add"))
	f.waitForAny(t, []string{"editMessageText"}, "number 1 in line", 3*time.Second)

	gw.handleUpdate(msg(thread, "second extra"))
	two := f.waitFor(t, "sendMessage", "second extra", 3*time.Second)
	gw.handleUpdate(press(thread, two.ID, "q:add"))
	f.waitForAny(t, []string{"editMessageText"}, "number 2 in line", 3*time.Second)

	if n := gw.queueLen(thread); n != 2 {
		t.Fatalf("both messages should be waiting, got %d", n)
	}

	// Ending the turn takes the first one, and finishing that takes the second.
	gw.handleUpdate(press(thread, 1, "stop"))
	f.waitFor(t, "sendMessage", "first extra", 8*time.Second)
	deadline := time.Now().Add(15 * time.Second)
	for gw.queueLen(thread) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := gw.queueLen(thread); n != 0 {
		t.Fatalf("the queue should have drained, %d left", n)
	}
}

func TestStopAndApplyEndsTheTurnAndTakesItsButtons(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "Apply", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	gw.handleUpdate(msg(thread, "SLOW please"))
	live := f.waitFor(t, "sendMessage", "thinking about it", 5*time.Second)
	if b := buttons(live); !has(b, "stop") || !has(b, "kill") {
		t.Fatalf("a running turn carries Stop and Kill, got %v", b)
	}

	gw.handleUpdate(msg(thread, "do this instead"))
	offer := f.waitFor(t, "sendMessage", "do this instead", 3*time.Second)

	before := f.count()
	gw.handleUpdate(press(thread, offer.ID, "q:now"))
	f.waitForAny(t, []string{"editMessageText"}, "Stopping the current step", 3*time.Second)

	// The turn's own message loses its buttons rather than keeping live
	// controls on a log that has stopped.
	var cleared bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !cleared {
		for _, c := range f.since(before) {
			if c.Method != "editMessageReplyMarkup" && c.Method != "editMessageText" {
				continue
			}
			id, _ := c.Params["message_id"].(float64)
			if int(id) != live.ID {
				continue
			}
			if _, ok := c.Params["reply_markup"]; !ok {
				cleared = true
			}
		}
		if !cleared {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !cleared {
		t.Fatal("stopping to apply a new message should take Stop and Kill off the old one")
	}

	// And the new message runs on its own.
	f.waitFor(t, "sendMessage", "do this instead", 10*time.Second)
}

func TestDismissSendsNothing(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "Drop", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	gw.handleUpdate(msg(thread, "SLOW please"))
	f.waitFor(t, "sendMessage", "thinking about it", 5*time.Second)
	gw.handleUpdate(msg(thread, "never mind this"))
	offer := f.waitFor(t, "sendMessage", "never mind this", 3*time.Second)

	gw.handleUpdate(press(thread, offer.ID, "q:drop"))
	f.waitForAny(t, []string{"editMessageText"}, "not sent", 3*time.Second)
	if n := gw.queueLen(thread); n != 0 {
		t.Fatalf("a dismissed message must not be queued, got %d", n)
	}
}

// ---------- workflows ----------

func TestWorkflowsListAndLiveView(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "Flow", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	gw.handleUpdate(msg(thread, "/workflows"))
	list := f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "the last 7 runs", 6*time.Second)
	b := buttons(list)
	if !has(b, "wf:0") || !has(b, "wf:reload") {
		t.Fatalf("the list should offer each run and a refresh, got %v", b)
	}
	if !strings.Contains(text(list), "rollout-0") {
		t.Fatalf("the list should name the runs: %s", text(list))
	}

	gw.handleUpdate(press(thread, targetID(list), "wf:0"))
	view := f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "review:bugs", 6*time.Second)
	if !strings.Contains(text(view), "live") {
		t.Fatalf("a running workflow should say it is live: %s", text(view))
	}
	if vb := buttons(view); !has(vb, "wfr:wf_fake-0") {
		t.Fatalf("the live view should offer a refresh of that run, got %v", vb)
	}

	// The watcher redraws it, and stops once the run is no longer live.
	settled := f.waitForAny(t, []string{"editMessageText"}, "completed", 20*time.Second)
	if strings.Contains(text(settled), "this updates itself") {
		t.Fatal("a finished workflow should stop calling itself live")
	}
}

func TestWorkflowsAreClaudeOnly(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("codex", work, "NoFlow", 0)
	if err != nil {
		t.Fatal(err)
	}
	gw.handleUpdate(msg(sess.ThreadID, "/workflows"))
	c := f.waitFor(t, "sendMessage", "Workflows are a Claude Code thing", 3*time.Second)
	if c == nil {
		t.Fatal("Codex has no workflows and should say so")
	}
}

// ---------- ultracode ----------

func TestUltracodeIsOfferedAsAnEffort(t *testing.T) {
	models := []ModelInfo{{
		ID: "default", Label: "Default",
		Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultracode"},
	}}
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "Effort", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	gw.presentEfforts(thread, 0, sess, models, 0)
	c := f.waitFor(t, "sendMessage", "How hard should it think?", 3*time.Second)
	if !strings.Contains(text(c), "Ultracode is xhigh thinking plus orchestration") {
		t.Fatalf("the effort menu should explain ultracode: %s", text(c))
	}
	labels := buttonLabels(c)
	found := false
	for _, l := range labels {
		if strings.Contains(l, "Ultracode") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ultracode should be on a button, got %v", labels)
	}

	// Picking it sticks to the session and reaches the agent.
	gw.handleUpdate(press(thread, c.ID, "eff:5"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(thread); s != nil && s.Effort == "ultracode" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("choosing ultracode should set the session's effort, got %q", gw.store.Get(thread).Effort)
}

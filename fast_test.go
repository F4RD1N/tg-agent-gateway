package main

import (
	"strings"
	"testing"
	"time"
)

func TestFastTogglesACodexSession(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("codex", work, "Speed", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	// A session that has not chosen follows the machine, and says so.
	if got := gw.store.Get(thread).Fast; got != fastAuto {
		t.Fatalf("a new session should not have chosen a speed, got %q", got)
	}

	gw.handleUpdate(msg(thread, "/fast"))
	on := f.waitFor(t, "sendMessage", "Fast is on", 3*time.Second)
	if gw.store.Get(thread).Fast != fastOn {
		t.Fatal("/fast should turn it on from the machine's setting")
	}
	if b := buttons(on); !has(b, "fast:off") || !has(b, "fast:auto") {
		t.Fatalf("the answer should offer the other states, got %v", b)
	}

	// Toggling again turns it off rather than back to following the machine:
	// a topic that asked for fast and changed its mind wants standard.
	gw.handleUpdate(msg(thread, "/fast"))
	f.waitFor(t, "sendMessage", "Fast is off", 3*time.Second)
	if gw.store.Get(thread).Fast != fastOff {
		t.Fatal("toggling again should turn it off")
	}

	gw.handleUpdate(press(thread, 1, "fast:auto"))
	f.waitFor(t, "sendMessage", "Following this machine", 3*time.Second)
	if gw.store.Get(thread).Fast != fastAuto {
		t.Fatal("the third state hands the decision back to the machine's config")
	}
}

func TestFastWordsSetItOutright(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("codex", work, "Words", 0)
	if err != nil {
		t.Fatal(err)
	}
	thread := sess.ThreadID

	gw.handleUpdate(msg(thread, "/fast on"))
	f.waitFor(t, "sendMessage", "Fast is on", 3*time.Second)
	gw.handleUpdate(msg(thread, "/fast on"))
	if gw.store.Get(thread).Fast != fastOn {
		t.Fatal("asking for on twice should leave it on, not toggle it off")
	}
	gw.handleUpdate(msg(thread, "/fast nonsense"))
	f.waitFor(t, "sendMessage", "Usage:", 3*time.Second)
}

func TestFastIsCodexOnly(t *testing.T) {
	gw, f, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "NoFast", 0)
	if err != nil {
		t.Fatal(err)
	}
	gw.handleUpdate(msg(sess.ThreadID, "/fast"))
	c := f.waitFor(t, "sendMessage", "Fast is a Codex setting", 3*time.Second)
	if !strings.Contains(text(c), "effort levels are the dial") {
		t.Fatalf("it should point Claude users at what they do have: %s", text(c))
	}
	if gw.store.Get(sess.ThreadID).Fast != "" {
		t.Fatal("a Claude session must not be given a Codex setting")
	}
}

func TestFastReachesTheAgent(t *testing.T) {
	gw, _, work := newTestGateway(t)
	sess, err := gw.createSession("codex", work, "Wire", 0)
	if err != nil {
		t.Fatal(err)
	}
	ns := gw.store.Update(sess.ThreadID, func(s *Session) { s.Fast = fastOn })
	if c := gw.command("prompt", ns, "hello"); c.Fast != "on" {
		t.Fatalf("the speed must travel with every turn, got %q", c.Fast)
	}
	// And it shows in the header, so a topic never hides which lane it is in.
	if !strings.Contains(gw.sessionHeader(ns), "fast") {
		t.Fatalf("the header should say the session is fast: %s", gw.sessionHeader(ns))
	}
	off := gw.store.Update(sess.ThreadID, func(s *Session) { s.Fast = fastAuto })
	if strings.Contains(gw.sessionHeader(off), "fast") {
		t.Fatal("a session that has not chosen should claim nothing about speed")
	}
}

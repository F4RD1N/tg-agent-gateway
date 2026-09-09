package main

import (
	"testing"
	"time"
)

// Pressing a button on a menu whose topic has since been ended must not take
// the gateway down.
func TestButtonsOnADeletedSessionDoNotPanic(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 95, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(95, "/config"))
	f.waitFor(t, "sendMessage", "Claude Code config", 3*time.Second)

	// The topic is ended from somewhere else, then the old menu is pressed.
	gw.store.Delete(95)
	for _, data := range []string{
		"cf:perm:plan", "verbose", "eff:none", "mdl:0", "agt:codex",
		"cfg:perm", "cfg", "clear:yes", "end:close", "stop", "kill", "skills", "model",
	} {
		gw.handleUpdate(press(95, 1001, data))
	}
	// Still answering: the gateway survived every one of them.
	gw.handleUpdate(msg(0, "/help"))
	f.waitFor(t, "sendMessage", "Agent gateway", 5*time.Second)
}

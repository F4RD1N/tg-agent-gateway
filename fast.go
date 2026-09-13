package main

import (
	"strings"
)

// Codex sells a priority lane for its models: the same model, served about
// twice as fast, against a larger share of the plan's usage. It is a config
// setting rather than a model or an effort level, which is why it gets a
// switch of its own instead of a place in the model picker.
//
// Three states, not two. "on" and "off" decide for this topic; empty follows
// whatever ~/.codex/config.toml says, so a machine already set up to run fast
// is not quietly overridden by a session that never asked either way.

const (
	fastOn   = "on"
	fastOff  = "off"
	fastAuto = ""
)

// toggleFast flips the speed of a Codex session, or sets it outright when the
// command carries a word.
func (gw *Gateway) toggleFast(thread int, sess *Session, arg string) {
	if sess.Agent != "codex" {
		gw.reply(thread, "⚡ Fast is a Codex setting: its priority tier serves the same model about twice as quickly.\n\n"+
			"<i>"+agentLabel(sess.Agent)+" has no equivalent here. For Claude, the effort levels are the dial.</i>",
			Rows([]Button{{Text: "⚡ Effort", CallbackData: "effort"}}))
		return
	}
	want := ""
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on", "yes", "fast":
		want = fastOn
	case "off", "no", "standard", "slow":
		want = fastOff
	case "auto", "default", "machine":
		want = fastAuto
	case "":
		// No word given, so it is a toggle: anything but on becomes on.
		if sess.Fast == fastOn {
			want = fastOff
		} else {
			want = fastOn
		}
	default:
		gw.reply(thread, "Usage: <code>/fast</code> to toggle, or <code>/fast on</code>, <code>/fast off</code>, <code>/fast auto</code>.", nil)
		return
	}
	if gw.busyHere(thread) {
		return
	}
	ns := gw.store.Update(thread, func(x *Session) { x.Fast = want })
	// The topic may have been ended while this menu was open.
	if ns == nil {
		return
	}
	// The next turn carries the whole config, so nothing else has to be sent;
	// the worker rebuilds its Codex client when the setting changes.
	_ = gw.bridge.Send(gw.command("start", ns, ""))
	gw.reply(thread, fastText(ns), fastKeyboard(ns))
}

func fastText(sess *Session) string {
	switch sess.Fast {
	case fastOn:
		return "⚡ <b>Fast is on</b>\n<i>Codex's priority tier: the same model, about twice the speed, against more of your usage allowance.</i>"
	case fastOff:
		return "🐢 <b>Fast is off</b>\n<i>Standard speed for this topic, whatever this machine is set to.</i>"
	}
	return "⚙️ <b>Following this machine's setting</b>\n<i>Speed comes from <code>service_tier</code> in ~/.codex/config.toml.</i>"
}

func fastKeyboard(sess *Session) *Keyboard {
	mark := func(state string, label string) string {
		if sess.Fast == state {
			return "✓ " + label
		}
		return label
	}
	return Rows(
		[]Button{
			{Text: mark(fastOn, "⚡ Fast"), CallbackData: "fast:on"},
			{Text: mark(fastOff, "🐢 Standard"), CallbackData: "fast:off"},
		},
		[]Button{{Text: mark(fastAuto, "⚙️ This machine's setting"), CallbackData: "fast:auto"}},
	)
}

// fastLabel is how the speed shows up in a session header, and says nothing
// at all when the topic has not chosen.
func fastLabel(sess *Session) string {
	if sess.Agent != "codex" {
		return ""
	}
	switch sess.Fast {
	case fastOn:
		return "⚡ fast"
	case fastOff:
		return "🐢 standard"
	}
	return ""
}

func fastButton(sess *Session) Button {
	label := "⚡ Fast: off"
	switch sess.Fast {
	case fastOn:
		label = "⚡ Fast: on"
	case fastAuto:
		label = "⚡ Fast"
	}
	return Button{Text: label, CallbackData: "fast:"}
}

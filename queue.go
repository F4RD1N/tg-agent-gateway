package main

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

// What happens when you write to a topic that is already working.
//
// Sending the message straight through would interleave it with the turn in
// flight, and refusing it outright means retyping later. So the bot asks:
// queue it for when this turn ends, stop the turn and run it now, or forget
// it. A queue can hold as many messages as you like and they run in order.

type pendingPrompt struct {
	text   string
	images []string
	at     time.Time
}

// offerQueue is what a message gets instead of an answer while the session is
// busy.
func (gw *Gateway) offerQueue(sess *Session, text string, images []string) {
	thread := sess.ThreadID
	gw.mu.Lock()
	waiting := len(gw.queued[thread])
	gw.mu.Unlock()

	head := "⏳ <b>" + agentLabel(sess.Agent) + " is still working.</b>"
	if waiting > 0 {
		head += fmt.Sprintf("\n<i>%s already waiting</i>", plural(waiting, "message", "messages"))
	}
	body := head + "\n\n" + blockquote(text, images)

	m := gw.reply(thread, body, Rows(
		[]Button{{Text: "➕ Add to Queue", CallbackData: "q:add"}},
		[]Button{{Text: "⏹ Stop and Apply", CallbackData: "q:now"}},
		[]Button{{Text: "✖️ Dismiss", CallbackData: "q:drop"}},
	))
	if m == nil {
		return
	}
	gw.rememberMenu(m.MessageID, &pending{
		kind: "queue", agent: sess.Agent, threadID: thread,
		prompt: text, images: images,
	})
}

func blockquote(text string, images []string) string {
	shown := strings.TrimSpace(text)
	if shown == "" && len(images) > 0 {
		shown = "(the attachment, with no words)"
	}
	s := "<blockquote>" + html.EscapeString(truncate(shown, 500)) + "</blockquote>"
	if n := len(images); n > 0 {
		s += fmt.Sprintf("\n<i>with %s</i>", plural(n, "image", "images"))
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// enqueue adds a prompt to the back of a topic's queue and says where it landed.
func (gw *Gateway) enqueue(thread int, p pendingPrompt) int {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	p.at = time.Now()
	gw.queued[thread] = append(gw.queued[thread], p)
	return len(gw.queued[thread])
}

// jump puts a prompt at the front, for a message that is replacing the turn
// being stopped rather than waiting behind it.
func (gw *Gateway) jump(thread int, p pendingPrompt) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	p.at = time.Now()
	gw.queued[thread] = append([]pendingPrompt{p}, gw.queued[thread]...)
}

// takeQueued pops the next prompt for a topic.
func (gw *Gateway) takeQueued(thread int) (pendingPrompt, bool) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	list := gw.queued[thread]
	if len(list) == 0 {
		return pendingPrompt{}, false
	}
	next := list[0]
	if len(list) == 1 {
		delete(gw.queued, thread)
	} else {
		gw.queued[thread] = list[1:]
	}
	return next, true
}

func (gw *Gateway) queueLen(thread int) int {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return len(gw.queued[thread])
}

func (gw *Gateway) clearQueue(thread int) int {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	n := len(gw.queued[thread])
	delete(gw.queued, thread)
	return n
}

// drainQueue starts the next queued prompt, if there is one. It runs when a
// turn ends, so a queue works its way through on its own.
func (gw *Gateway) drainQueue(sess *Session) {
	next, ok := gw.takeQueued(sess.ThreadID)
	if !ok {
		return
	}
	// Read the session again: the queued message runs against whatever the
	// topic is now, not what it was when the message was written.
	fresh := gw.store.Get(sess.ThreadID)
	if fresh == nil {
		return
	}
	left := gw.queueLen(sess.ThreadID)
	note := "▶️ <i>next from the queue</i>\n" + blockquote(next.text, next.images)
	if left > 0 {
		note += fmt.Sprintf("\n<i>%s still waiting</i>", plural(left, "message", "messages"))
	}
	gw.reply(sess.ThreadID, note, nil)
	gw.submitPrompt(fresh, next.text, next.images)
}

// endTurnKeyboard takes the Stop and Kill buttons off the message of the turn
// running in this topic. Stopping a turn to apply something else should not
// leave live-looking controls on the log it just finished writing.
func (gw *Gateway) endTurnKeyboard(thread int) {
	gw.mu.Lock()
	msgID := gw.turnMsg[thread]
	gw.mu.Unlock()
	if msgID != 0 {
		_ = gw.tg.EditKeyboard(gw.ctx, gw.cfg.ChatID, msgID, nil)
	}
}

// showQueue lists what a topic has waiting.
func (gw *Gateway) showQueue(thread int) {
	gw.mu.Lock()
	list := append([]pendingPrompt(nil), gw.queued[thread]...)
	gw.mu.Unlock()
	if len(list) == 0 {
		gw.reply(thread, "Nothing is queued here.", nil)
		return
	}
	var b strings.Builder
	b.WriteString("<b>Queued</b> — " + plural(len(list), "message", "messages") + "\n")
	for i, p := range list {
		fmt.Fprintf(&b, "\n<b>%d.</b> %s", i+1, html.EscapeString(truncate(strings.TrimSpace(p.text), 160)))
		if n := len(p.images); n > 0 {
			fmt.Fprintf(&b, " <i>(+%s)</i>", plural(n, "image", "images"))
		}
	}
	gw.reply(thread, b.String(), Rows(
		[]Button{{Text: "🧹 Clear the queue", CallbackData: "q:clear"}},
	))
}

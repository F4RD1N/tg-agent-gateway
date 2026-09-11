package main

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"
)

// ---------- markdown -> Telegram HTML ----------
//
// Telegram accepts a small HTML subset: b i u s code pre a blockquote. Agent
// output is markdown, so it is converted rather than sent raw, and everything
// that is not a recognised construct is escaped.

var (
	reFence      = regexp.MustCompile("(?s)```([a-zA-Z0-9_+-]*)\\n?(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`\n]+)`")
	reBold       = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reItalic     = regexp.MustCompile(`(^|[\s(])\*([^*\n]+)\*($|[\s).,!?;:])`)
	reHeading    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reLink       = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	reBullet     = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)
)

const codePlaceholder = "\x00CODE%d\x00"

// mdToHTML converts a markdown fragment to the Telegram HTML subset.
func mdToHTML(s string) string {
	var blocks []string
	// Pull fenced code out first so nothing else rewrites its contents.
	s = reFence.ReplaceAllStringFunc(s, func(m string) string {
		g := reFence.FindStringSubmatch(m)
		lang, body := g[1], g[2]
		body = strings.TrimRight(body, "\n")
		var b string
		if lang != "" {
			b = `<pre><code class="language-` + html.EscapeString(lang) + `">` + html.EscapeString(body) + `</code></pre>`
		} else {
			b = "<pre>" + html.EscapeString(body) + "</pre>"
		}
		blocks = append(blocks, b)
		return fmt.Sprintf(codePlaceholder, len(blocks)-1)
	})
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		g := reInlineCode.FindStringSubmatch(m)
		blocks = append(blocks, "<code>"+html.EscapeString(g[1])+"</code>")
		return fmt.Sprintf(codePlaceholder, len(blocks)-1)
	})

	s = html.EscapeString(s)
	s = reHeading.ReplaceAllString(s, "<b>$1</b>")
	s = reBold.ReplaceAllString(s, "<b>$1</b>")
	s = reItalic.ReplaceAllString(s, "$1<i>$2</i>$3")
	s = reLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
	s = reBullet.ReplaceAllString(s, "$1• ")

	for i, b := range blocks {
		s = strings.ReplaceAll(s, fmt.Sprintf(codePlaceholder, i), b)
	}
	return s
}

// ---------- turn rendering ----------
//
// A turn is one message, edited while the agent works. What it shows is the
// agent's own narration, one paragraph per thing it set out to do:
//
//     ✅️ I'll set the project up and wire the pieces together.
//     ✅️ The layout is in place. Adding the parts it depends on.
//     ✳️ Everything is built. I'm checking that it works.
//
// Done above, current at the bottom. The commands behind those sentences are
// not shown: the agent already said what it is doing, in better words than a
// command line ever will. Switch Details on for the tool list.

const (
	markDone    = "✅️"
	markRunning = "✳️"
	markFailed  = "✖️"
)

// block is one thing the agent said. A new one starts whenever it goes off to
// work, so each block reads as one step of the job.
type block struct {
	text   string
	sealed bool
}

// Turn renders one agent turn into Telegram, sealing the message and
// continuing in a new one when it grows past the size limit.
type Turn struct {
	gw       *Gateway
	chatID   int64
	threadID int
	agent    string
	verbose  bool

	blocks   []block
	notes    []string
	tools    []string // only kept for the Details view
	steps    int
	failed   bool
	msgID    int
	lastEdit time.Time
	lastSent string
	running  bool
	started  time.Time
}

func (gw *Gateway) NewTurn(sess *Session) *Turn {
	return &Turn{
		gw: gw, chatID: gw.cfg.ChatID, threadID: sess.ThreadID,
		agent: sess.Agent, verbose: sess.Verbose, running: true, started: time.Now(),
	}
}

func agentLabel(agent string) string {
	switch agent {
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	}
	return "Claude Code"
}

func (t *Turn) AddText(s string) {
	if s == "" {
		return
	}
	if n := len(t.blocks); n > 0 && !t.blocks[n-1].sealed {
		t.blocks[n-1].text += s
		return
	}
	t.blocks = append(t.blocks, block{text: s})
}

// seal closes the current paragraph, so whatever the agent says next starts a
// new step.
func (t *Turn) seal() {
	if n := len(t.blocks); n > 0 && strings.TrimSpace(t.blocks[n-1].text) != "" {
		t.blocks[n-1].sealed = true
	}
}

// SetStep records that the agent went off to do something. The work itself is
// not shown; it only ends the paragraph that announced it.
func (t *Turn) SetStep(status, tool, detail string) {
	if status == "start" {
		t.steps++
		_, phrase := describeStep(tool, detail)
		t.tools = append(t.tools, phrase)
		if len(t.tools) > 40 {
			t.tools = t.tools[len(t.tools)-40:]
		}
		t.seal()
		return
	}
	if status == "fail" {
		t.failed = true
		if n := len(t.tools); n > 0 {
			t.tools[n-1] += " (failed)"
		}
	}
}

// SetFileStep records an edit the agent made; codex reports these separately.
func (t *Turn) SetFileStep(kind, path string) {
	verb := "Editing"
	switch kind {
	case "add":
		verb = "Creating"
	case "delete":
		verb = "Deleting"
	}
	t.steps++
	t.tools = append(t.tools, verb+" "+shortTarget(path))
	t.seal()
}

// AddNote is for the few things that must stay on screen: errors, a stop, a
// kill, a compaction.
func (t *Turn) AddNote(html string) {
	t.seal()
	t.notes = append(t.notes, html)
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func (t *Turn) render() string {
	var parts []string
	// One paragraph, and nothing else happened: it is just an answer, so it
	// does not need a checklist mark.
	plain := len(t.blocks) == 1 && t.steps == 0
	for i, b := range t.blocks {
		body := strings.TrimSpace(b.text)
		if body == "" {
			continue
		}
		rendered := mdToHTML(body)
		if plain {
			parts = append(parts, rendered)
			continue
		}
		mark := markDone
		if i == len(t.blocks)-1 && t.running {
			mark = markRunning
		}
		parts = append(parts, mark+" "+rendered)
	}
	if len(parts) == 0 && t.running {
		parts = append(parts, "<i>"+agentLabel(t.agent)+" is working…</i>")
	}
	if t.verbose && len(t.tools) > 0 {
		var b strings.Builder
		b.WriteString("<blockquote expandable>")
		for i, tool := range t.tools {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("· " + html.EscapeString(tool))
		}
		b.WriteString("</blockquote>")
		parts = append(parts, b.String())
	}
	parts = append(parts, t.notes...)
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func (t *Turn) keyboard() *Keyboard {
	if !t.running {
		return nil
	}
	return Rows([]Button{
		{Text: "⏹ Stop", CallbackData: "stop"},
		{Text: "💀 Kill", CallbackData: "kill"},
	})
}

// Flush pushes the current state to Telegram, respecting the edit interval
// unless force is set.
func (t *Turn) Flush(force bool) {
	interval := t.gw.editInterval()
	if !force && time.Since(t.lastEdit) < interval {
		return
	}
	body := t.render()
	if body == "" {
		return
	}
	max := t.gw.cfg.MaxMessageChars
	for len([]rune(body)) > max {
		head, tail := splitHTML(body, max)
		t.push(head, false)
		// What was sent stays in the sealed message; the rest continues in a
		// fresh one, as a single block so the marks do not restart.
		t.msgID = 0
		t.lastSent = ""
		t.blocks = []block{{text: stripTags(tail)}}
		t.notes = nil
		t.tools = nil
		body = tail
	}
	t.push(body, t.running)
	t.lastEdit = time.Now()
}

func (t *Turn) push(body string, withKeyboard bool) {
	if body == t.lastSent && t.msgID != 0 {
		return
	}
	var kb *Keyboard
	if withKeyboard {
		kb = t.keyboard()
	}
	ctx := t.gw.ctx
	if t.msgID == 0 {
		m, err := t.gw.tg.Send(ctx, t.chatID, body, SendOpts{ThreadID: t.threadID, Keyboard: kb})
		if err != nil {
			// A malformed entity is the one error worth retrying as plain text.
			m2, err2 := t.gw.tg.Send(ctx, t.chatID, html.EscapeString(stripTags(body)), SendOpts{ThreadID: t.threadID, Keyboard: kb})
			if err2 != nil {
				logf("send failed: %v (fallback: %v)", err, err2)
				return
			}
			m = m2
		}
		t.msgID = m.MessageID
		t.lastSent = body
		// Remember which message carries this turn's Stop and Kill buttons, so
		// stopping it from elsewhere can take them off straight away.
		t.gw.mu.Lock()
		t.gw.turnMsg[t.threadID] = t.msgID
		t.gw.mu.Unlock()
		return
	}
	if err := t.gw.tg.Edit(ctx, t.chatID, t.msgID, body, kb); err != nil {
		if err2 := t.gw.tg.Edit(ctx, t.chatID, t.msgID, html.EscapeString(stripTags(body)), kb); err2 != nil {
			logf("edit failed: %v (fallback: %v)", err, err2)
			return
		}
	}
	t.lastSent = body
}

// Finish seals the turn: every paragraph is done, and a quiet footer goes on
// the end.
func (t *Turn) Finish(footer string) {
	t.running = false
	if footer != "" {
		t.notes = append(t.notes, "<i>"+footer+"</i>")
	}
	t.Flush(true)
	if t.msgID != 0 {
		_ = t.gw.tg.EditKeyboard(t.gw.ctx, t.chatID, t.msgID, nil)
	}
}

// splitHTML cuts a rendered message at a safe boundary below max runes,
// closing and reopening a <pre> block if the cut lands inside one.
func splitHTML(s string, max int) (head, tail string) {
	r := []rune(s)
	if len(r) <= max {
		return s, ""
	}
	cut := max
	// Prefer a newline, then a space, and never cut inside a tag.
	for i := max; i > max/2; i-- {
		if r[i] == '\n' {
			cut = i
			break
		}
	}
	seg := string(r[:cut])
	if strings.Count(seg, "<") != strings.Count(seg, ">") {
		back := cut
		for back > 0 && r[back-1] != '>' {
			back--
		}
		// Only take the earlier cut if it still leaves something to send;
		// otherwise the message would never shrink and Flush would spin.
		if back > 0 {
			cut = back
		}
	}
	head = string(r[:cut])
	tail = strings.TrimLeft(string(r[cut:]), "\n")
	// Balance a code block that the cut split in half.
	if strings.Count(head, "<pre>")+strings.Count(head, "<pre><code") > strings.Count(head, "</pre>") {
		head += "</code></pre>"
		head = strings.ReplaceAll(head, "</code></code></pre>", "</code></pre>")
		tail = "<pre>" + tail
	}
	return head, tail
}

var reTag = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	return html.UnescapeString(reTag.ReplaceAllString(s, ""))
}

func fmtDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%dms", ms)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func fmtTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

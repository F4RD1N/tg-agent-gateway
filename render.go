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

type itemKind int

const (
	itemText itemKind = iota
	itemTool
	itemNote
)

type turnItem struct {
	kind itemKind
	text string // markdown for itemText, ready HTML for the others
}

// Turn renders one agent turn into Telegram: a live message that is edited
// while the answer streams, sealed and continued in a new message when it
// grows past the size limit.
type Turn struct {
	gw       *Gateway
	chatID   int64
	threadID int
	agent    string
	verbose  bool

	items    []turnItem
	msgID    int
	lastEdit time.Time
	lastSent string
	running  bool
	stopping bool
	started  time.Time
}

func (gw *Gateway) NewTurn(sess *Session) *Turn {
	return &Turn{
		gw: gw, chatID: gw.cfg.ChatID, threadID: sess.ThreadID,
		agent: sess.Agent, verbose: sess.Verbose, running: true, started: time.Now(),
	}
}

func agentLabel(agent string) string {
	if agent == "codex" {
		return "Codex"
	}
	return "Claude Code"
}

func (t *Turn) AddText(s string) {
	if s == "" {
		return
	}
	if n := len(t.items); n > 0 && t.items[n-1].kind == itemText {
		t.items[n-1].text += s
		return
	}
	t.items = append(t.items, turnItem{kind: itemText, text: s})
}

func (t *Turn) AddTool(status, name, detail string) {
	icon := "⚙"
	switch status {
	case "ok":
		icon = "✓"
	case "fail":
		icon = "✗"
	}
	line := icon + " <b>" + html.EscapeString(name) + "</b>"
	if detail != "" {
		if status == "start" {
			line += " <code>" + html.EscapeString(truncate(detail, 200)) + "</code>"
		} else if t.verbose {
			line += "\n<blockquote expandable>" + html.EscapeString(truncate(detail, 700)) + "</blockquote>"
		}
	}
	// A finished call replaces the "started" line for the same tool, so the
	// message does not fill up with pairs.
	if status != "start" {
		for i := len(t.items) - 1; i >= 0; i-- {
			if t.items[i].kind != itemTool {
				continue
			}
			if strings.Contains(t.items[i].text, "<b>"+html.EscapeString(name)+"</b>") && strings.HasPrefix(t.items[i].text, "⚙") {
				old := t.items[i].text
				if status == "ok" && !t.verbose {
					t.items[i].text = strings.Replace(old, "⚙", "✓", 1)
				} else {
					head := old
					if idx := strings.Index(old, "\n"); idx > 0 {
						head = old[:idx]
					}
					t.items[i].text = strings.Replace(head, "⚙", icon, 1)
					if detail != "" && (t.verbose || status == "fail") {
						t.items[i].text += "\n<blockquote expandable>" + html.EscapeString(truncate(detail, 700)) + "</blockquote>"
					}
				}
				return
			}
		}
	}
	t.items = append(t.items, turnItem{kind: itemTool, text: line})
}

func (t *Turn) AddNote(html string) {
	t.items = append(t.items, turnItem{kind: itemNote, text: html})
}

func (t *Turn) render() string {
	var b strings.Builder
	for i, it := range t.items {
		if i > 0 {
			b.WriteString("\n")
		}
		switch it.kind {
		case itemText:
			b.WriteString(mdToHTML(strings.TrimRight(it.text, "\n")))
		default:
			b.WriteString(it.text)
		}
	}
	return strings.TrimSpace(b.String())
}

func (t *Turn) keyboard() *Keyboard {
	if !t.running {
		return nil
	}
	return Rows([]Button{{Text: "⏹ Stop", CallbackData: "stop"}})
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
		if t.running {
			body = "<i>" + agentLabel(t.agent) + " is working…</i>"
		} else {
			return
		}
	}
	max := t.gw.cfg.MaxMessageChars
	for len([]rune(body)) > max {
		head, tail := splitHTML(body, max)
		t.push(head, false)
		t.msgID = 0 // next push starts a new message
		t.items = []turnItem{{kind: itemText, text: ""}}
		t.items = nil
		body = tail
		t.lastSent = ""
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

// Finish seals the turn: last edit, footer, keyboard removed.
func (t *Turn) Finish(footer string) {
	t.running = false
	if footer != "" {
		t.AddNote("<i>" + footer + "</i>")
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
	for depth := 0; depth < 1; depth++ {
		seg := string(r[:cut])
		if strings.Count(seg, "<") != strings.Count(seg, ">") {
			for cut > 0 && r[cut-1] != '>' {
				cut--
			}
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

package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const helpText = `<b>Agent gateway</b>

Each topic in this group is one agent session. Write a message in a topic and it goes to that agent; the reply streams back into the same message.

<b>Sessions</b>
/new — start one (I create the topic)
/new My Project — name the topic yourself
/rename &lt;name&gt; — rename this topic
/sessions — list them
/end — finish this one

<b>This topic</b>
/model /agent /effort — pick with buttons
/mode — accept edits, plan, auto, manual…
/skills — run a skill or command
/config — permissions, thinking, limits, sandbox
/cd /pwd /ls — working folder
/get &lt;file&gt; — send me a file
/run &lt;command&gt; — run a shell command here
/stop — interrupt the current turn
/kill — kill the agent and everything it started
/clear — forget the conversation, keep the topic
/session — show this topic's conversation id
/resume &lt;id&gt; — continue another conversation here
/compact — ask the agent to compact its context
/status — what this session is
/verbose — show tool output and thinking

Send a file to a topic and it lands in that session's folder. Anything else starting with / goes to the agent, so Claude's own commands work.`

func (gw *Gateway) handleCommand(m *TGMessage, thread int, text string) {
	cmd, arg := splitCommand(text)
	sess := gw.store.Get(thread)

	switch cmd {
	case "start", "help":
		gw.reply(thread, helpText, Rows(
			[]Button{{Text: "✨ New session", CallbackData: "new"}, {Text: "📋 Sessions", CallbackData: "list"}},
		))
	case "id":
		gw.reply(thread, fmt.Sprintf("chat <code>%d</code>\nyou <code>%d</code>\ntopic <code>%d</code>", m.Chat.ID, m.From.ID, thread), nil)
	case "new":
		gw.cmdNew(thread, arg)
	case "sessions", "list":
		gw.cmdSessions(thread)
	case "session", "id-session":
		gw.needSession(thread, sess, func(s *Session) {
			id := s.Ref
			if id == "" {
				gw.reply(thread, "This topic has no conversation yet. Send a message and one starts.", nil)
				return
			}
			gw.reply(thread, "🧵 <b>"+agentLabel(s.Agent)+"</b> conversation\n<code>"+html.EscapeString(id)+"</code>\n\n"+
				"Point another topic at it with <code>/resume "+html.EscapeString(id)+"</code>.", nil)
		})
	case "resume", "attach":
		gw.needSession(thread, sess, func(s *Session) {
			if arg == "" {
				gw.reply(thread, "Usage: <code>/resume &lt;conversation id&gt;</code>\nUse <code>/session</code> to see this topic's own id.", nil)
				return
			}
			if gw.busyHere(thread) {
				return
			}
			id := strings.Fields(arg)[0]
			ns := gw.store.Update(thread, func(x *Session) { x.Ref = id })
			if ns == nil {
				return
			}
			gw.reply(thread, "🧵 this topic now continues <code>"+html.EscapeString(id)+"</code>.\nSend a message to pick it up.", gw.sessionKeyboard(ns))
		})
	case "compact":
		gw.needSession(thread, sess, func(s *Session) {
			text := "/compact"
			if arg != "" {
				text += " " + arg
			}
			gw.submit(s, text)
		})
	case "stop":
		gw.cmdStop(thread)
	case "kill":
		gw.needSession(thread, sess, func(s *Session) { gw.cmdKill(thread) })
	case "status":
		if sess == nil {
			gw.offerBind(thread, "No session here.")
			return
		}
		gw.reply(thread, gw.sessionHeader(sess), gw.sessionKeyboard(sess))
	case "model":
		gw.needSession(thread, sess, func(s *Session) { gw.showModels(thread, s, 0) })
	case "effort":
		gw.needSession(thread, sess, func(s *Session) { gw.showEfforts(thread, s, 0) })
	case "agent":
		gw.needSession(thread, sess, func(s *Session) { gw.showAgents(thread, s, 0) })
	case "skills", "skill":
		gw.needSession(thread, sess, func(s *Session) { gw.showSkills(thread, 0, s, 0) })
	case "mode":
		gw.needSession(thread, sess, func(s *Session) { gw.showConfigChoice(thread, 0, s, "perm") })
	case "config", "settings":
		gw.needSession(thread, sess, func(s *Session) { gw.showConfig(thread, 0, s) })
	case "verbose":
		gw.needSession(thread, sess, func(s *Session) {
			ns := gw.store.Update(thread, func(x *Session) { x.Verbose = !x.Verbose })
			// The topic may have been ended while this menu was open.
			if ns == nil {
				return
			}
			gw.reply(thread, "Tool output and thinking: <b>"+onOff(ns.Verbose)+"</b>", gw.sessionKeyboard(ns))
		})
	case "clear":
		gw.needSession(thread, sess, func(s *Session) {
			gw.reply(thread, "Start a fresh conversation in this topic? The folder and settings stay.", Rows(
				[]Button{{Text: "🧹 Yes, forget it", CallbackData: "clear:yes"}, {Text: "Cancel", CallbackData: "dismiss"}},
			))
		})
	case "end":
		gw.needSession(thread, sess, func(s *Session) { gw.askEnd(thread) })
	case "rename":
		gw.needSession(thread, sess, func(s *Session) {
			if arg == "" {
				gw.reply(thread, "Usage: <code>/rename New name</code>", nil)
				return
			}
			title := topicTitle(s.Agent, arg)
			if err := gw.tg.EditTopic(gw.ctx, gw.cfg.ChatID, thread, title); err != nil {
				gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
				return
			}
			gw.store.Update(thread, func(x *Session) {
				x.Name = arg
				x.AutoName = false
				x.Title = title
			})
			gw.reply(thread, "✏️ topic renamed to <b>"+html.EscapeString(title)+"</b>", nil)
		})
	case "pwd":
		gw.needSession(thread, sess, func(s *Session) {
			gw.reply(thread, "📁 <code>"+html.EscapeString(s.Cwd)+"</code>", nil)
		})
	case "cd":
		gw.needSession(thread, sess, func(s *Session) {
			if arg == "" {
				gw.showDirs(thread, s.Cwd, "cd", s.Agent, "", 0)
				return
			}
			gw.setCwd(thread, s, arg)
		})
	case "ls":
		gw.needSession(thread, sess, func(s *Session) {
			dir := s.Cwd
			if arg != "" {
				dir = resolvePath(s.Cwd, arg)
			}
			gw.runShell(thread, dir, "ls -la --color=never")
		})
	case "get":
		gw.needSession(thread, sess, func(s *Session) {
			if arg == "" {
				gw.reply(thread, "Usage: <code>/get path/to/file</code>", nil)
				return
			}
			p := resolvePath(s.Cwd, arg)
			if !gw.cfg.PathAllowed(p) {
				gw.reply(thread, "That path is outside the allowed roots.", nil)
				return
			}
			if err := gw.tg.SendDocument(gw.ctx, gw.cfg.ChatID, thread, p, "<code>"+html.EscapeString(p)+"</code>"); err != nil {
				gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
			}
		})
	case "run":
		gw.needSession(thread, sess, func(s *Session) {
			if arg == "" {
				gw.reply(thread, "Usage: <code>/run ls -la</code>", nil)
				return
			}
			go gw.runShell(thread, s.Cwd, arg)
		})
	default:
		// Unknown slash commands belong to the agent (Claude's /compact and
		// friends), so pass them through when there is a session.
		if sess != nil {
			gw.submit(sess, text)
			return
		}
		gw.offerBind(thread, "Unknown command, and there is no session here.")
	}
}

// configOption is one row of the Config screen: a setting and the values it
// can take, all on buttons.
type configOption struct {
	Key     string
	Icon    string
	Label   string
	Agent   string // claude | codex | "" for both
	Choices []Choice
	Current func(*Session) string
	Apply   func(*Session, string)
}

func configOptions() []configOption {
	return []configOption{
		{
			Key: "perm", Icon: "🔓", Label: "Mode", Agent: "claude",
			Choices: []Choice{
				{ID: "bypassPermissions", Label: "Full access"},
				{ID: "acceptEdits", Label: "Accept edits"},
				{ID: "plan", Label: "Plan mode"},
				{ID: "dontAsk", Label: "Don't ask"},
				{ID: "auto", Label: "Auto mode"},
				{ID: "default", Label: "Manual mode"},
			},
			Current: func(s *Session) string {
				if s.PermMode == "" {
					return "bypassPermissions"
				}
				return s.PermMode
			},
			Apply: func(s *Session, v string) { s.PermMode = v },
		},
		{
			Key: "think", Icon: "💭", Label: "Thinking budget", Agent: "claude",
			Choices: []Choice{
				{ID: "0", Label: "Default"},
				{ID: "8000", Label: "8k"},
				{ID: "16000", Label: "16k"},
				{ID: "32000", Label: "32k"},
				{ID: "64000", Label: "64k"},
			},
			Current: func(s *Session) string { return strconv.Itoa(s.Thinking) },
			Apply:   func(s *Session, v string) { s.Thinking, _ = strconv.Atoi(v) },
		},
		{
			Key: "turns", Icon: "🔁", Label: "Max turns", Agent: "claude",
			Choices: []Choice{
				{ID: "0", Label: "No limit"},
				{ID: "10", Label: "10"},
				{ID: "25", Label: "25"},
				{ID: "50", Label: "50"},
				{ID: "100", Label: "100"},
			},
			Current: func(s *Session) string { return strconv.Itoa(s.MaxTurns) },
			Apply:   func(s *Session, v string) { s.MaxTurns, _ = strconv.Atoi(v) },
		},
		{
			Key: "budget", Icon: "💰", Label: "Budget per turn", Agent: "claude",
			Choices: []Choice{
				{ID: "0", Label: "No limit"},
				{ID: "1", Label: "$1"},
				{ID: "5", Label: "$5"},
				{ID: "10", Label: "$10"},
				{ID: "25", Label: "$25"},
			},
			Current: func(s *Session) string { return strconv.Itoa(int(s.BudgetUSD)) },
			Apply: func(s *Session, v string) {
				n, _ := strconv.Atoi(v)
				s.BudgetUSD = float64(n)
			},
		},
		{
			// Claude Code calls this "switch models when a message is flagged":
			// when the model refuses, the turn is retried on the fallback model.
			Key: "fallback", Icon: "🛟", Label: "Switch model when flagged", Agent: "claude",
			Choices: []Choice{{ID: "", Label: "Off"}},
			Current: func(s *Session) string { return s.Fallback },
			Apply:   func(s *Session, v string) { s.Fallback = v },
		},
		{
			// Loads settings.json, CLAUDE.md and the machine's own
			// configuration. Skills are enabled either way.
			Key: "usercfg", Icon: "📚", Label: "Use ~/.claude settings", Agent: "claude",
			Choices: []Choice{
				{ID: "on", Label: "Use them"},
				{ID: "off", Label: "Ignore them"},
			},
			Current: func(s *Session) string {
				if s.NoUserSettings {
					return "off"
				}
				return "on"
			},
			Apply: func(s *Session, v string) { s.NoUserSettings = v == "off" },
		},
		{
			Key: "perm", Icon: "🔓", Label: "Mode", Agent: "antigravity",
			Choices: []Choice{
				{ID: "bypassPermissions", Label: "Full access"},
				{ID: "acceptEdits", Label: "Accept edits"},
				{ID: "plan", Label: "Plan mode"},
			},
			Current: func(s *Session) string {
				if s.PermMode == "" {
					return "bypassPermissions"
				}
				return s.PermMode
			},
			Apply: func(s *Session, v string) { s.PermMode = v },
		},
		{
			Key: "sandbox", Icon: "🏖", Label: "Sandbox", Agent: "codex",
			Choices: []Choice{
				{ID: "danger-full-access", Label: "Full access"},
				{ID: "workspace-write", Label: "Workspace write"},
				{ID: "read-only", Label: "Read only"},
			},
			Current: func(s *Session) string {
				if s.Sandbox == "" {
					return "danger-full-access"
				}
				return s.Sandbox
			},
			Apply: func(s *Session, v string) { s.Sandbox = v },
		},
		{
			Key: "approval", Icon: "✅", Label: "Approvals", Agent: "codex",
			Choices: []Choice{
				{ID: "never", Label: "Never ask"},
				{ID: "on-request", Label: "On request"},
				{ID: "on-failure", Label: "On failure"},
				{ID: "untrusted", Label: "Untrusted"},
			},
			Current: func(s *Session) string {
				if s.Approval == "" {
					return "never"
				}
				return s.Approval
			},
			Apply: func(s *Session, v string) { s.Approval = v },
		},
		{
			Key: "web", Icon: "🌐", Label: "Web search", Agent: "codex",
			Choices: []Choice{
				{ID: "", Label: "Default"},
				{ID: "on", Label: "On"},
				{ID: "off", Label: "Off"},
			},
			Current: func(s *Session) string { return s.WebSearch },
			Apply:   func(s *Session, v string) { s.WebSearch = v },
		},
		{
			Key: "net", Icon: "🔌", Label: "Network access", Agent: "codex",
			Choices: []Choice{
				{ID: "", Label: "Default"},
				{ID: "on", Label: "On"},
				{ID: "off", Label: "Off"},
			},
			Current: func(s *Session) string { return s.Network },
			Apply:   func(s *Session, v string) { s.Network = v },
		},
	}
}

func configOptionsFor(agent string) []configOption {
	var out []configOption
	for _, o := range configOptions() {
		if o.Agent == "" || o.Agent == agent {
			out = append(out, o)
		}
	}
	return out
}

func findConfigOption(agent, key string) *configOption {
	for _, o := range configOptionsFor(agent) {
		if o.Key == key {
			c := o
			return &c
		}
	}
	return nil
}

func choiceLabel(o *configOption, sess *Session) string {
	cur := o.Current(sess)
	for _, c := range o.Choices {
		if c.ID == cur {
			return c.Label
		}
	}
	if cur == "" {
		if o.Key == "fallback" {
			return "Off"
		}
		return "Default"
	}
	return cur
}

// showConfig is the Config button: everything /config would offer, as buttons.
func (gw *Gateway) showConfig(thread, editMsg int, sess *Session) {
	var rows [][]Button
	for _, o := range configOptionsFor(sess.Agent) {
		rows = append(rows, []Button{{
			Text:         o.Icon + " " + o.Label + ": " + choiceLabel(&o, sess),
			CallbackData: "cfg:" + o.Key,
		}})
	}
	rows = append(rows, []Button{
		{Text: "🧠 Model", CallbackData: "model"},
		{Text: "⚡ Effort", CallbackData: "effort"},
	})
	rows = append(rows, []Button{{Text: "‹ Back", CallbackData: "cfg:back"}})
	title := "<b>" + agentLabel(sess.Agent) + " config</b>\n" + gw.sessionHeader(sess)
	gw.notify(thread, editMsg, title, Rows(rows...))
}

func (gw *Gateway) showConfigChoice(thread, editMsg int, sess *Session, key string) {
	o := findConfigOption(sess.Agent, key)
	if o == nil {
		gw.showConfig(thread, editMsg, sess)
		return
	}
	if key == "fallback" {
		// The choices are the agent's own models, so they are fetched.
		guard("fallback menu", func() {
			models, err := gw.fetchModels(sess.Agent)
			if err != nil {
				models = gw.fallbackModels(sess.Agent)
			}
			choices := []Choice{{ID: "", Label: "Off"}}
			for _, m := range models {
				if m.ID == "" || m.ID == "default" {
					continue
				}
				choices = append(choices, Choice{ID: m.ID, Label: m.Label})
			}
			opt := *o
			opt.Choices = choices
			gw.renderConfigChoice(thread, editMsg, sess, &opt,
				"Retry on another model when a message is flagged.")
		})
		return
	}
	gw.renderConfigChoice(thread, editMsg, sess, o, "")
}

func (gw *Gateway) renderConfigChoice(thread, editMsg int, sess *Session, o *configOption, note string) {
	cur := o.Current(sess)
	var buttons []Button
	for _, c := range o.Choices {
		label := c.Label
		if c.ID == cur {
			label = "✓ " + label
		}
		id := c.ID
		if id == "" {
			id = "-"
		}
		buttons = append(buttons, Button{Text: truncate(label, 40), CallbackData: "cf:" + o.Key + ":" + id})
	}
	kb := Grid(2, buttons)
	kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{{Text: "‹ Config", CallbackData: "cfg"}})
	title := o.Icon + " <b>" + o.Label + "</b>"
	if note != "" {
		title += "\n<i>" + html.EscapeString(note) + "</i>"
	}
	gw.notify(thread, editMsg, title, kb)
}

// askEnd offers the two ways a topic can go away, both on buttons.
func (gw *Gateway) askEnd(thread int) {
	gw.reply(thread, "End this session?", Rows(
		[]Button{{Text: "🔒 Close the topic", CallbackData: "end:close"}},
		[]Button{{Text: "🗑 Delete the topic", CallbackData: "end:delete"}},
		[]Button{{Text: "Cancel", CallbackData: "dismiss"}},
	))
}

// busyHere reports whether a turn is running in this topic, and says so.
func (gw *Gateway) busyHere(thread int) bool {
	gw.mu.Lock()
	running := gw.running[thread]
	gw.mu.Unlock()
	if running {
		gw.reply(thread, "⏳ That would change the session while it is working. Stop it first.", Rows(
			[]Button{{Text: "⏹ Stop", CallbackData: "stop"}, {Text: "💀 Kill", CallbackData: "kill"}},
		))
	}
	return running
}

func (gw *Gateway) needSession(thread int, sess *Session, fn func(*Session)) {
	if sess == nil {
		gw.offerBind(thread, "There is no session in this topic.")
		return
	}
	fn(sess)
}

func splitCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	sp := strings.IndexAny(text, " \n")
	head, rest := text, ""
	if sp > 0 {
		head, rest = text[:sp], strings.TrimSpace(text[sp+1:])
	}
	head = strings.TrimPrefix(head, "/")
	if at := strings.Index(head, "@"); at > 0 {
		head = head[:at]
	}
	return strings.ToLower(head), rest
}

func resolvePath(base, p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

// ---------------------------------------------------------------- /new

// cmdNew accepts "/new", "/new codex", "/new /root/project" and
// "/new My Project" in any combination: a word that looks like a path is the
// folder, "claude"/"codex" pick the agent, and whatever is left becomes the
// name of the topic.
func (gw *Gateway) cmdNew(thread int, arg string) {
	agent, path, name := parseNewArgs(arg)
	if agent == "" {
		m := gw.reply(thread, "Which agent?", agentPicker("new:agent:"))
		if m != nil {
			gw.rememberMenu(m.MessageID, &pending{kind: "newagent", name: name, threadID: thread})
		}
		return
	}
	if path == "" {
		gw.showDirs(thread, gw.cfg.DefaultCwd, "newdir", agent, name, 0)
		return
	}
	dir := resolvePath(gw.cfg.DefaultCwd, path)
	if name == "" {
		gw.askTopicName(thread, 0, agent, dir)
		return
	}
	gw.startSession(thread, agent, dir, name, 0)
}

func parseNewArgs(arg string) (agent, path, name string) {
	var words []string
	for _, f := range strings.Fields(arg) {
		switch strings.ToLower(f) {
		case "claude", "claude-code", "cc":
			agent = "claude"
			continue
		case "codex", "cx":
			agent = "codex"
			continue
		case "antigravity", "agy", "gemini":
			agent = "antigravity"
			continue
		}
		if path == "" && looksLikePath(f) {
			path = f
			continue
		}
		words = append(words, f)
	}
	return agent, path, strings.TrimSpace(strings.Join(words, " "))
}

func looksLikePath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") ||
		strings.HasPrefix(s, "./") || strings.Contains(s, "/")
}

// startSession creates the topic and posts the pinned header. editMsg > 0 means
// the menu message that led here should be replaced instead of a new one sent.
func (gw *Gateway) startSession(thread int, agent, cwd, name string, editMsg int) {
	if !gw.cfg.PathAllowed(cwd) {
		gw.notify(thread, editMsg, "That folder is outside the allowed roots.")
		return
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		gw.notify(thread, editMsg, "No such folder: <code>"+html.EscapeString(cwd)+"</code>")
		return
	}
	// A topic that already has a session is rebound rather than duplicated.
	target := 0
	if thread != 0 && gw.store.Get(thread) == nil {
		target = thread
	}
	sess, err := gw.createSession(agent, cwd, name, target)
	if err != nil {
		gw.notify(thread, editMsg, "⚠️ "+html.EscapeString(err.Error()))
		return
	}
	link := TopicLink(gw.cfg.ChatID, sess.ThreadID)
	text := "✅ <b>" + agentLabel(agent) + "</b> is ready in <code>" + html.EscapeString(cwd) + "</code>"
	kb := Rows([]Button{{Text: "➡️ Open the topic", URL: link}})
	if sess.ThreadID == thread {
		text = gw.sessionHeader(sess) + "\n\nSend a message and it goes to the agent."
		kb = gw.sessionKeyboard(sess)
	}
	gw.notify(thread, editMsg, text, kb)

	if sess.ThreadID != thread {
		m := gw.reply(sess.ThreadID, gw.sessionHeader(sess)+"\n\nSend a message and it goes to the agent.", gw.sessionKeyboard(sess))
		if m != nil {
			_ = gw.tg.Pin(gw.ctx, gw.cfg.ChatID, m.MessageID)
		}
	}
}

func (gw *Gateway) notify(thread, editMsg int, text string, kb ...*Keyboard) {
	var k *Keyboard
	if len(kb) > 0 {
		k = kb[0]
	}
	if editMsg > 0 {
		if err := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, editMsg, text, k); err == nil {
			return
		}
	}
	gw.reply(thread, text, k)
}

// showSkills lists what this agent can be told to run by name: Claude's
// skills and commands, Codex's prompt files, Antigravity's skills. Long lists
// are paged, because a phone keyboard of sixty buttons helps nobody.
const skillsPerPage = 10

func (gw *Gateway) showSkills(thread, editMsg int, sess *Session, page int) {
	msgID := editMsg
	if msgID == 0 {
		if m := gw.reply(thread, "⏳ asking "+agentLabel(sess.Agent)+" for its skills…", nil); m != nil {
			msgID = m.MessageID
		}
	}
	guard("skill menu", func() {
		skills, err := gw.fetchSkills(sess.Agent)
		if err != nil {
			gw.notify(thread, msgID, "⚠️ "+html.EscapeString(err.Error()))
			return
		}
		if len(skills) == 0 {
			gw.notify(thread, msgID, skillsEmptyText(sess.Agent))
			return
		}
		// Skills the user installed come first; the rest follow.
		sort.SliceStable(skills, func(i, j int) bool {
			if skills[i].Skill != skills[j].Skill {
				return skills[i].Skill
			}
			return skills[i].Name < skills[j].Name
		})
		pages := (len(skills) + skillsPerPage - 1) / skillsPerPage
		if page < 0 {
			page = pages - 1
		}
		if page >= pages {
			page = 0
		}
		start := page * skillsPerPage
		end := start + skillsPerPage
		if end > len(skills) {
			end = len(skills)
		}
		var buttons []Button
		for i := start; i < end; i++ {
			label := skills[i].Name
			if skills[i].Skill {
				label = "🧩 " + label
			}
			buttons = append(buttons, Button{Text: truncate(label, 32), CallbackData: "sk:" + strconv.Itoa(i)})
		}
		kb := Grid(2, buttons)
		if pages > 1 {
			kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{
				{Text: "‹ Back", CallbackData: "skp:" + strconv.Itoa(page-1)},
				{Text: fmt.Sprintf("%d/%d ›", page+1, pages), CallbackData: "skp:" + strconv.Itoa(page+1)},
			})
		}
		var b strings.Builder
		fmt.Fprintf(&b, "<b>%s can run:</b>\n", agentLabel(sess.Agent))
		for i := start; i < end; i++ {
			b.WriteString("\n<b>/" + html.EscapeString(skills[i].Name) + "</b>")
			if skills[i].Description != "" {
				b.WriteString(" — " + html.EscapeString(truncate(skills[i].Description, 90)))
			}
		}
		gw.rememberMenu(msgID, &pending{kind: "skills", agent: sess.Agent, skills: skills, page: page, threadID: thread})
		gw.notify(thread, msgID, b.String(), kb)
	})
}

func skillsEmptyText(agent string) string {
	switch agent {
	case "codex":
		return "Codex has no prompt files yet.\nPut a <code>name.md</code> in <code>~/.codex/prompts/</code> and it shows up here."
	case "antigravity":
		return "Antigravity has no skills yet.\nAdd one as <code>~/.gemini/antigravity-cli/skills/&lt;name&gt;/SKILL.md</code>."
	}
	return "No skills or commands were reported.\nAdd one as <code>~/.claude/skills/&lt;name&gt;/SKILL.md</code>."
}

// runSkill sends the skill to the agent. Claude and Antigravity expand a
// leading slash themselves; Codex does not, so its prompt file is read and
// sent as the text it contains.
func (gw *Gateway) runSkill(thread int, sess *Session, sk SkillInfo, args string) {
	text := "/" + sk.Name
	if args != "" {
		text += " " + args
	}
	if sess.Agent == "codex" && sk.Body != "" {
		body := sk.Body
		if i := strings.Index(body, "\n---\n"); strings.HasPrefix(body, "---\n") && i > 0 {
			body = body[i+5:]
		}
		body = strings.ReplaceAll(body, "$ARGUMENTS", args)
		body = strings.ReplaceAll(body, "$1", args)
		text = strings.TrimSpace(body)
		if args != "" && !strings.Contains(sk.Body, "$ARGUMENTS") && !strings.Contains(sk.Body, "$1") {
			text += "\n\n" + args
		}
	}
	gw.submit(sess, text)
}

// askTopicName is the last step of creating a session: what the topic should
// be called. The agent goes in front of whatever you pick, so "VPN App"
// becomes "Claude • VPN App".
func (gw *Gateway) askTopicName(thread, editMsg int, agent, cwd string) {
	suggestion := filepath.Base(strings.TrimRight(cwd, "/"))
	kb := Rows(
		[]Button{{Text: "⌨️ Type a name", CallbackData: "newname:type"}},
		[]Button{{Text: "📁 " + truncate(suggestion, 30), CallbackData: "newname:auto"}},
	)
	title := "What should this topic be called?\n<i>" +
		html.EscapeString(topicTitle(agent, "your name here")) + "</i>"
	menu := &pending{kind: "newname", agent: agent, dirs: []string{cwd}, threadID: thread}
	if editMsg > 0 {
		gw.rememberMenu(editMsg, menu)
		if err := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, editMsg, title, kb); err == nil {
			return
		}
	}
	if m := gw.reply(thread, title, kb); m != nil {
		gw.rememberMenu(m.MessageID, menu)
	}
}

// ---------------------------------------------------------------- pickers

func (gw *Gateway) showDirs(thread int, dir, kind, agent, name string, editMsg int) {
	dirs := gw.subdirs(dir)
	recent := gw.store.RecentDirs(4)
	var buttons []Button
	list := []string{}
	add := func(p, label string) {
		list = append(list, p)
		buttons = append(buttons, Button{Text: label, CallbackData: kind + ":" + strconv.Itoa(len(list)-1)})
	}
	for _, r := range recent {
		if r != dir {
			add(r, "🕘 "+filepath.Base(r))
		}
	}
	for _, d := range dirs {
		add(d, "📁 "+filepath.Base(d))
	}
	kb := Grid(2, buttons)
	nav := []Button{}
	if parent := filepath.Dir(dir); parent != dir {
		list = append(list, parent)
		nav = append(nav, Button{Text: "⬆️ up", CallbackData: kind + ":" + strconv.Itoa(len(list)-1)})
	}
	nav = append(nav,
		Button{Text: "✅ use this folder", CallbackData: kind + ":here"},
		Button{Text: "⌨️ type a path", CallbackData: kind + ":type"},
	)
	kb.InlineKeyboard = append(kb.InlineKeyboard, nav)

	title := "Folder for the new session:\n<code>" + html.EscapeString(dir) + "</code>"
	if kind == "cd" {
		title = "Working folder:\n<code>" + html.EscapeString(dir) + "</code>"
	}
	menu := &pending{kind: kind, agent: agent, name: name, dirs: append(list, dir), threadID: thread}
	if editMsg > 0 {
		gw.rememberMenu(editMsg, menu)
		if err := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, editMsg, title, kb); err == nil {
			return
		}
		logf("folder menu: falling back to a new message")
	}
	m := gw.reply(thread, title, kb)
	if m == nil {
		return
	}
	gw.rememberMenu(m.MessageID, menu)
}

// showModels lists everything the agent itself reports, on buttons. Asking
// Claude means starting a CLI, which takes a moment, so a placeholder goes up
// first and is replaced in place.
func (gw *Gateway) showModels(thread int, sess *Session, editMsg int) {
	msgID := editMsg
	if msgID > 0 {
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "⏳ asking "+agentLabel(sess.Agent)+" for its models…", nil)
	} else if m := gw.reply(thread, "⏳ asking "+agentLabel(sess.Agent)+" for its models…", nil); m != nil {
		msgID = m.MessageID
	}
	guard("model menu", func() {
		models, err := gw.fetchModels(sess.Agent)
		if err != nil {
			logf("model list for %s: %v", sess.Agent, err)
			models = gw.fallbackModels(sess.Agent)
		}
		var buttons []Button
		for i, m := range models {
			label := m.Label
			if m.ID == sess.Model {
				label = "✓ " + label
			}
			buttons = append(buttons, Button{Text: truncate(label, 40), CallbackData: "mdl:" + strconv.Itoa(i)})
		}
		kb := Grid(2, buttons)
		kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{{Text: "⌨️ type a model name", CallbackData: "mdl:type"}})
		title := "Model for <b>" + agentLabel(sess.Agent) + "</b>"
		if err != nil {
			title += "\n<i>(could not reach the agent, showing the configured list)</i>"
		}
		if msgID > 0 {
			gw.rememberMenu(msgID, &pending{kind: "model", agent: sess.Agent, models: models, threadID: thread})
			if e := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, title, kb); e != nil {
				logf("model menu: %v", e)
			}
			return
		}
		if m := gw.reply(thread, title, kb); m != nil {
			gw.rememberMenu(m.MessageID, &pending{kind: "model", agent: sess.Agent, models: models, threadID: thread})
		}
	})
}

// showEfforts offers the levels the chosen model actually accepts.
func (gw *Gateway) showEfforts(thread int, sess *Session, editMsg int) {
	guard("effort menu", func() {
		models, err := gw.fetchModels(sess.Agent)
		if err != nil {
			gw.notify(thread, editMsg, "⚠️ "+html.EscapeString(err.Error()))
			return
		}
		idx := 0
		for i, m := range models {
			if m.ID == sess.Model {
				idx = i
				break
			}
		}
		if len(models[idx].Efforts) == 0 {
			gw.notify(thread, editMsg, "<b>"+html.EscapeString(models[idx].Label)+"</b> has no effort levels.")
			return
		}
		gw.presentEfforts(thread, editMsg, sess, models, idx)
	})
}

// presentEfforts is the second step of the picker: the levels for one model.
func (gw *Gateway) presentEfforts(thread, editMsg int, sess *Session, models []ModelInfo, idx int) {
	m := models[idx]
	var buttons []Button
	for i, e := range m.Efforts {
		label := strings.ToUpper(e[:1]) + e[1:]
		if e == m.DefaultEffort {
			label += " ·default"
		}
		if e == sess.Effort {
			label = "✓ " + label
		}
		buttons = append(buttons, Button{Text: label, CallbackData: "eff:" + strconv.Itoa(i)})
	}
	kb := Grid(3, buttons)
	kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{
		{Text: "— none —", CallbackData: "eff:none"},
		{Text: "‹ models", CallbackData: "model"},
	})
	title := "<b>" + html.EscapeString(m.Label) + "</b>"
	if m.Description != "" {
		title += "\n<i>" + html.EscapeString(truncate(m.Description, 200)) + "</i>"
	}
	title += "\n\nHow hard should it think?"
	msgID := editMsg
	if msgID > 0 {
		gw.rememberMenu(msgID, &pending{kind: "effort", agent: sess.Agent, models: models, modelIdx: idx, threadID: thread})
		if err := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, title, kb); err != nil {
			logf("effort menu: %v", err)
		}
		return
	}
	sent := gw.reply(thread, title, kb)
	if sent == nil {
		return
	}
	gw.rememberMenu(sent.MessageID, &pending{kind: "effort", agent: sess.Agent, models: models, modelIdx: idx, threadID: thread})
}

// agentPicker is the one list of agents, so adding one shows up everywhere.
func agentPicker(prefix string, current ...string) *Keyboard {
	cur := ""
	if len(current) > 0 {
		cur = current[0]
	}
	var buttons []Button
	for _, a := range []struct{ id, label string }{
		{"claude", "🟠 Claude Code"},
		{"codex", "🟢 Codex"},
		{"antigravity", "🟣 Antigravity"},
	} {
		label := a.label
		if a.id == cur {
			label = "✓ " + label
		}
		buttons = append(buttons, Button{Text: label, CallbackData: prefix + a.id})
	}
	return Grid(2, buttons)
}

func (gw *Gateway) showAgents(thread int, sess *Session, editMsg int) {
	gw.notify(thread, editMsg, "Which agent runs in this topic?\nSwitching starts a fresh conversation.",
		agentPicker("agt:", sess.Agent))
}

func (gw *Gateway) cmdSessions(thread int) {
	list := gw.store.List()
	if len(list) == 0 {
		gw.reply(thread, "No sessions yet.", Rows([]Button{{Text: "✨ New session", CallbackData: "new"}}))
		return
	}
	var b strings.Builder
	var buttons []Button
	b.WriteString("<b>Sessions</b>\n")
	const maxListed = 20
	extra := 0
	if len(list) > maxListed {
		extra = len(list) - maxListed
		list = list[:maxListed]
	}
	for _, s := range list {
		b.WriteString(fmt.Sprintf("\n• <b>%s</b> — <code>%s</code>", agentLabel(s.Agent), html.EscapeString(s.Cwd)))
		if s.Turns > 0 {
			b.WriteString(fmt.Sprintf(" · %d turns", s.Turns))
		}
		buttons = append(buttons, Button{
			Text: agentLabel(s.Agent) + " · " + filepath.Base(s.Cwd),
			URL:  TopicLink(gw.cfg.ChatID, s.ThreadID),
		})
	}
	if extra > 0 {
		fmt.Fprintf(&b, "\n\n<i>and %d older ones</i>", extra)
	}
	kb := Grid(1, buttons)
	kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{{Text: "✨ New session", CallbackData: "new"}})
	gw.reply(thread, b.String(), kb)
}

// cmdKill is the hard stop: it takes down the agent process for this topic
// and every command it started, which /stop deliberately does not do.
func (gw *Gateway) cmdKill(thread int) {
	if err := gw.bridge.Send(Command{Type: "kill", SID: sidOf(thread)}); err != nil {
		gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
		return
	}
	gw.mu.Lock()
	running := gw.running[thread]
	gw.mu.Unlock()
	if !running {
		gw.reply(thread, "💀 killed anything this session had running.", nil)
	}
}

func (gw *Gateway) cmdStop(thread int) {
	gw.mu.Lock()
	running := gw.running[thread]
	gw.mu.Unlock()
	if !running {
		gw.reply(thread, "Nothing is running here.", nil)
		return
	}
	_ = gw.bridge.Send(Command{Type: "interrupt", SID: sidOf(thread)})
}

func (gw *Gateway) setCwd(thread int, sess *Session, arg string) {
	p := resolvePath(sess.Cwd, arg)
	if !gw.cfg.PathAllowed(p) {
		gw.reply(thread, "That path is outside the allowed roots.", nil)
		return
	}
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		gw.reply(thread, "No such folder: <code>"+html.EscapeString(p)+"</code>", nil)
		return
	}
	ns := gw.store.Update(thread, func(s *Session) {
		s.Cwd = p
		// A title nobody chose follows the folder it is working in.
		if s.AutoName {
			s.Name = filepath.Base(strings.TrimRight(p, "/"))
			s.Title = topicTitle(s.Agent, s.Name)
		}
	})
	// The topic may have been ended while this menu was open.
	if ns == nil {
		return
	}
	if ns.AutoName {
		_ = gw.tg.EditTopic(gw.ctx, gw.cfg.ChatID, thread, ns.Title)
	}
	gw.reply(thread, "📁 now <code>"+html.EscapeString(p)+"</code>", gw.sessionKeyboard(ns))
}

// finishPathEntry handles the text the user sends after "type a path".
func (gw *Gateway) finishPathEntry(p *pending, text string, thread int) {
	switch p.kind {
	case "newdir":
		dir := resolvePath(gw.cfg.DefaultCwd, text)
		if p.name == "" {
			gw.askTopicName(p.threadID, 0, p.agent, dir)
			return
		}
		gw.startSession(p.threadID, p.agent, dir, p.name, 0)
	case "newname":
		if len(p.dirs) == 0 {
			return
		}
		gw.startSession(p.threadID, p.agent, p.dirs[0], text, 0)
	case "cd":
		sess := gw.store.Get(p.threadID)
		if sess == nil {
			gw.offerBind(p.threadID, "That session is gone.")
			return
		}
		gw.setCwd(p.threadID, sess, text)
	case "skillargs":
		sess := gw.store.Get(p.threadID)
		if sess == nil || len(p.skills) == 0 {
			return
		}
		if text == "-" {
			text = ""
		}
		gw.runSkill(p.threadID, sess, p.skills[0], text)
	case "model":
		sess := gw.store.Get(p.threadID)
		if sess == nil {
			return
		}
		ns := gw.store.Update(p.threadID, func(s *Session) { s.Model = text })
		// The topic may have been ended while this menu was open.
		if ns == nil {
			return
		}
		gw.reply(p.threadID, "🧠 model: <b>"+html.EscapeString(text)+"</b>", gw.sessionKeyboard(ns))
	}
}

// ---------------------------------------------------------------- callbacks

func (gw *Gateway) handleCallback(cq *TGCallbackQuery) {
	if cq.Message == nil || !gw.authorised(cq.From, cq.Message.Chat.ID) {
		_ = gw.tg.AnswerCallback(gw.ctx, cq.ID, "Not for you.", true)
		return
	}
	thread := cq.Message.MessageThreadID
	if !cq.Message.IsTopicMessage {
		thread = 0
	}
	msgID := cq.Message.MessageID
	data := cq.Data
	ack := func(text string) { _ = gw.tg.AnswerCallback(gw.ctx, cq.ID, text, false) }

	head, arg := data, ""
	if i := strings.Index(data, ":"); i >= 0 {
		head, arg = data[:i], data[i+1:]
	}
	sess := gw.store.Get(thread)

	switch head {
	case "dismiss":
		ack("")
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "<i>cancelled</i>", nil)

	case "help":
		ack("")
		gw.reply(thread, helpText, nil)

	case "list":
		ack("")
		gw.cmdSessions(thread)

	case "new":
		if arg == "" {
			ack("")
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "Which agent?", agentPicker("new:agent:"))
			return
		}
		if strings.HasPrefix(arg, "agent:") {
			agent := strings.TrimPrefix(arg, "agent:")
			name := ""
			if p := gw.menu(msgID); p != nil {
				name = p.name
			}
			ack(agentLabel(agent))
			gw.showDirs(thread, gw.cfg.DefaultCwd, "newdir", agent, name, msgID)
		}

	case "newdir", "cd":
		p := gw.menu(msgID)
		if p == nil {
			ack("That menu expired.")
			return
		}
		switch arg {
		case "here":
			ack("")
			dir := p.dirs[len(p.dirs)-1]
			if head == "newdir" {
				if p.name == "" {
					gw.askTopicName(p.threadID, msgID, p.agent, dir)
					return
				}
				gw.startSession(p.threadID, p.agent, dir, p.name, msgID)
			} else if sess != nil {
				gw.setCwd(thread, sess, dir)
				_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "📁 <code>"+html.EscapeString(dir)+"</code>", nil)
			}
		case "type":
			ack("Send me the path")
			gw.awaitPath(cq.From.ID, &pending{kind: head, agent: p.agent, name: p.name, threadID: p.threadID})
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "Send the folder path as a message.", nil)
		default:
			i, err := strconv.Atoi(arg)
			if err != nil || i < 0 || i >= len(p.dirs) {
				ack("")
				return
			}
			ack("")
			gw.showDirs(thread, p.dirs[i], head, p.agent, p.name, msgID)
		}

	case "skp":
		p := gw.menu(msgID)
		if p == nil || sess == nil {
			ack("That menu expired.")
			return
		}
		page, err := strconv.Atoi(arg)
		if err != nil {
			ack("")
			return
		}
		ack("")
		gw.showSkills(thread, msgID, sess, page)

	case "sk":
		p := gw.menu(msgID)
		if p == nil || sess == nil {
			ack("That menu expired.")
			return
		}
		i, err := strconv.Atoi(arg)
		if err != nil || i < 0 || i >= len(p.skills) {
			ack("")
			return
		}
		sk := p.skills[i]
		if sk.Hint != "" {
			ack("It takes arguments")
			gw.awaitPath(cq.From.ID, &pending{kind: "skillargs", agent: sess.Agent, skills: []SkillInfo{sk}, threadID: thread})
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID,
				"<b>/"+html.EscapeString(sk.Name)+"</b> takes <code>"+html.EscapeString(sk.Hint)+"</code>.\nSend them as a message, or send <code>-</code> to run it without.", nil)
			return
		}
		ack("Running /" + sk.Name)
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "▶️ <b>/"+html.EscapeString(sk.Name)+"</b>", nil)
		gw.runSkill(thread, sess, sk, "")

	case "newname":
		p := gw.menu(msgID)
		if p == nil || len(p.dirs) == 0 {
			ack("That menu expired.")
			return
		}
		if arg == "type" {
			ack("Send me the name")
			gw.awaitPath(cq.From.ID, &pending{kind: "newname", agent: p.agent, dirs: p.dirs, threadID: p.threadID})
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "Send the name for this topic as a message.", nil)
			return
		}
		ack("")
		gw.startSession(p.threadID, p.agent, p.dirs[0], "", msgID)

	case "bind":
		ack("")
		gw.startSession(thread, arg, gw.cfg.DefaultCwd, "", msgID)

	case "stop":
		if sess == nil {
			ack("No session.")
			return
		}
		gw.mu.Lock()
		running := gw.running[thread]
		gw.mu.Unlock()
		if !running {
			ack("Nothing is running.")
			return
		}
		ack("Stopping…")
		_ = gw.bridge.Send(Command{Type: "interrupt", SID: sidOf(thread)})

	case "kill":
		if sess == nil {
			ack("No session.")
			return
		}
		ack("Killing the processes…")
		gw.cmdKill(thread)

	case "skills":
		if sess == nil {
			ack("No session.")
			return
		}
		ack("")
		gw.showSkills(thread, 0, sess, 0)

	case "model":
		if sess == nil {
			ack("No session.")
			return
		}
		ack("")
		gw.showModels(thread, sess, msgID)

	case "mdl":
		if sess == nil {
			ack("No session.")
			return
		}
		if arg == "type" {
			ack("Send me the model name")
			gw.awaitPath(cq.From.ID, &pending{kind: "model", threadID: thread})
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "Send the model name as a message.", nil)
			return
		}
		p := gw.menu(msgID)
		if p == nil || len(p.models) == 0 {
			ack("That menu expired.")
			gw.showModels(thread, sess, 0)
			return
		}
		i, err := strconv.Atoi(arg)
		if err != nil || i < 0 || i >= len(p.models) {
			ack("")
			return
		}
		chosen := p.models[i]
		ns := gw.store.Update(thread, func(s *Session) {
			s.Model = chosen.ID
			// An effort the new model does not accept would be refused.
			if len(chosen.Efforts) == 0 {
				s.Effort = ""
			} else if s.Effort != "" && !containsStr(chosen.Efforts, s.Effort) {
				s.Effort = ""
			}
		})
		// The topic may have been ended while this menu was open.
		if ns == nil {
			return
		}
		ack(chosen.Label)
		if len(chosen.Efforts) > 0 {
			gw.presentEfforts(thread, msgID, ns, p.models, i)
			return
		}
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, gw.sessionHeader(ns), gw.sessionKeyboard(ns))

	case "effort":
		if sess == nil {
			ack("No session.")
			return
		}
		ack("")
		gw.showEfforts(thread, sess, msgID)

	case "eff":
		if sess == nil {
			ack("No session.")
			return
		}
		if arg == "none" {
			ns := gw.store.Update(thread, func(s *Session) { s.Effort = "" })
			// The topic may have been ended while this menu was open.
			if ns == nil {
				return
			}
			ack("Default effort")
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, gw.sessionHeader(ns), gw.sessionKeyboard(ns))
			return
		}
		p := gw.menu(msgID)
		if p == nil || p.modelIdx >= len(p.models) {
			ack("That menu expired.")
			gw.showEfforts(thread, sess, 0)
			return
		}
		efforts := p.models[p.modelIdx].Efforts
		i, err := strconv.Atoi(arg)
		if err != nil || i < 0 || i >= len(efforts) {
			ack("")
			return
		}
		e := efforts[i]
		ns := gw.store.Update(thread, func(s *Session) { s.Effort = e })
		// The topic may have been ended while this menu was open.
		if ns == nil {
			return
		}
		ack(e)
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, gw.sessionHeader(ns), gw.sessionKeyboard(ns))

	case "agent":
		if sess == nil {
			ack("No session.")
			return
		}
		ack("")
		gw.showAgents(thread, sess, 0)

	case "agt":
		if sess == nil {
			ack("No session.")
			return
		}
		if arg != "claude" && arg != "codex" && arg != "antigravity" {
			ack("")
			return
		}
		if gw.busyHere(thread) {
			ack("It is still working")
			return
		}
		ns := gw.store.Update(thread, func(s *Session) {
			if s.Agent != arg {
				s.Agent = arg
				s.Ref = ""
				s.Model = ""
			}
		})
		// The topic may have been ended while this menu was open.
		if ns == nil {
			return
		}
		ack(agentLabel(arg))
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, gw.sessionHeader(ns), gw.sessionKeyboard(ns))
		// The topic keeps the name you chose; only the agent in front changes.
		title := topicTitle(ns.Agent, ns.Name)
		if err := gw.tg.EditTopic(gw.ctx, gw.cfg.ChatID, thread, title); err == nil {
			gw.store.Update(thread, func(x *Session) { x.Title = title })
		}

	case "cfg":
		if sess == nil {
			ack("No session.")
			return
		}
		ack("")
		switch arg {
		case "", "back":
			if arg == "back" {
				_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, gw.sessionHeader(sess), gw.sessionKeyboard(sess))
				return
			}
			gw.showConfig(thread, msgID, sess)
		default:
			gw.showConfigChoice(thread, msgID, sess, arg)
		}

	case "cf":
		if sess == nil {
			ack("No session.")
			return
		}
		key, val, ok := strings.Cut(arg, ":")
		if !ok {
			ack("")
			return
		}
		if val == "-" {
			val = ""
		}
		o := findConfigOption(sess.Agent, key)
		if o == nil {
			ack("")
			return
		}
		ns := gw.store.Update(thread, func(s *Session) { o.Apply(s, val) })
		// The topic may have been ended while this menu was open.
		if ns == nil {
			return
		}
		// Nothing is sent to the agent here: every prompt carries the whole
		// configuration, so the change lands on the next turn without
		// disturbing one that may be running now.
		ack(o.Label + ": " + choiceLabel(o, ns))
		gw.showConfig(thread, msgID, ns)

	case "verbose":
		if sess == nil {
			ack("No session.")
			return
		}
		ns := gw.store.Update(thread, func(s *Session) { s.Verbose = !s.Verbose })
		// The topic may have been ended while this menu was open.
		if ns == nil {
			return
		}
		ack("Details " + onOff(ns.Verbose))
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, gw.sessionHeader(ns), gw.sessionKeyboard(ns))

	case "clear":
		if sess == nil {
			ack("No session.")
			return
		}
		if arg != "yes" {
			ack("")
			gw.reply(thread, "Start a fresh conversation in this topic?", Rows(
				[]Button{{Text: "🧹 Yes", CallbackData: "clear:yes"}, {Text: "Cancel", CallbackData: "dismiss"}},
			))
			return
		}
		if gw.busyHere(thread) {
			ack("It is still working")
			return
		}
		gw.store.Update(thread, func(s *Session) { s.Ref = "" })
		_ = gw.bridge.Send(Command{Type: "clear", SID: sidOf(thread)})
		ack("Cleared")
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "🧹 <i>fresh conversation; the folder and settings stayed</i>", nil)

	case "end":
		if sess == nil {
			ack("No session.")
			return
		}
		if arg != "close" && arg != "delete" {
			ack("")
			gw.askEnd(thread)
			return
		}
		ack("Ended")
		_ = gw.bridge.Send(Command{Type: "stop", SID: sidOf(thread)})
		gw.store.Delete(thread)
		if arg == "delete" {
			if err := gw.tg.DeleteTopic(gw.ctx, gw.cfg.ChatID, thread); err != nil {
				logf("delete topic %d: %v", thread, err)
				_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "🗑 <i>session ended; the topic could not be deleted: "+html.EscapeString(err.Error())+"</i>", nil)
				_ = gw.tg.CloseTopic(gw.ctx, gw.cfg.ChatID, thread)
			}
			return
		}
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "🗑 <i>session ended, topic closed</i>", nil)
		_ = gw.tg.CloseTopic(gw.ctx, gw.cfg.ChatID, thread)

	case "unhold":
		held, _ := gw.takeHeldFiles(thread)
		ack("Forgotten")
		if len(held) == 0 {
			_ = gw.tg.EditKeyboard(gw.ctx, gw.cfg.ChatID, msgID, nil)
			return
		}
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID,
			fmt.Sprintf("📎 %d file(s) are still saved in the folder; I will not bring them up again.", len(held)), nil)

	default:
		ack("")
	}
}

// startedAt is used by /status-style output.
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

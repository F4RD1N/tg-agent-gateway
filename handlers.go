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
/sessions — browse past conversations and take one up again
/end — close the topic, or delete it and keep the session

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

<b>Isolated sessions</b>
A sandbox session works in a folder of its own and sees nothing else of this server: no other projects, no memories, no settings. Pick <b>Isolated sandbox</b> instead of a folder when starting one, and say whether it is for you or for somebody else. A session made for somebody else answers only them.
/services — what that session keeps running (there is no systemd inside; use <code>svc</code>)

Send a file to a topic and it lands in that session's folder. Anything else starting with / goes to the agent, so Claude's own commands work.`

// adminCommands are the ones that reach past a single topic: the list of what
// is running, starting something new, and taking a topic away. A guest given a
// sandbox has none of them.
var adminCommands = map[string]bool{
	"new": true, "sessions": true, "list": true, "end": true, "resume": true, "attach": true,
}

func (gw *Gateway) handleCommand(m *TGMessage, thread int, text string) {
	cmd, arg := splitCommand(text)
	sess := gw.store.Get(thread)

	if adminCommands[cmd] && !gw.mayList(m.From.ID) {
		gw.adminOnly(thread)
		return
	}

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
			gw.reply(thread, "📁 <code>"+html.EscapeString(guestPath(s, s.Cwd))+"</code>", nil)
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
				p, err := hostPath(s, s.Cwd, arg)
				if err != nil {
					gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
					return
				}
				dir = p
			}
			gw.runShell(thread, s, dir, "ls -la --color=never")
		})
	case "get":
		gw.needSession(thread, sess, func(s *Session) {
			if arg == "" {
				gw.reply(thread, "Usage: <code>/get path/to/file</code>", nil)
				return
			}
			p, err := hostPath(s, s.Cwd, arg)
			if err != nil {
				gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
				return
			}
			if !gw.pathAllowed(s, p) {
				gw.reply(thread, "That path is outside the allowed roots.", nil)
				return
			}
			if err := gw.tg.SendDocument(gw.ctx, gw.cfg.ChatID, thread, p, "<code>"+html.EscapeString(guestPath(s, p))+"</code>"); err != nil {
				gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
			}
		})
	case "run":
		gw.needSession(thread, sess, func(s *Session) {
			if arg == "" {
				gw.reply(thread, "Usage: <code>/run ls -la</code>", nil)
				return
			}
			go gw.runShell(thread, s, s.Cwd, arg)
		})
	case "services", "svc":
		gw.needSession(thread, sess, func(s *Session) {
			if !s.Isolated {
				gw.reply(thread, "Services are part of isolated sessions, which have no systemd of their own. "+
					"This topic works on the machine directly, so use systemd here.", nil)
				return
			}
			cmdline := "svc list"
			if arg != "" {
				cmdline = "svc " + arg
			}
			go gw.runShell(thread, s, s.Cwd, cmdline)
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
	gw.reply(thread, "End this session?\n\n<i>Keeping the conversation means the topic goes "+
		"but the session stays: find it again under /sessions and resume it in a new topic.</i>", Rows(
		[]Button{{Text: "🔒 Close the topic", CallbackData: "end:close"}},
		[]Button{{Text: "📥 Delete topic, keep session", CallbackData: "end:keep"}},
		[]Button{{Text: "🗑 Delete both", CallbackData: "end:delete"}},
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
	available := gw.availableAgents()
	if len(available) == 0 {
		gw.reply(thread, noAgentsText(), nil)
		return
	}
	if agent != "" && !gw.agentInstalled(agent) {
		gw.reply(thread, "⚠️ "+agentLabel(agent)+" is not installed on this server.", gw.agentPicker("new:agent:"))
		return
	}
	if agent == "" && len(available) == 1 {
		// Nothing to choose between, so do not ask.
		agent = available[0]
	}
	if agent == "" {
		m := gw.reply(thread, "Which agent?", gw.agentPicker("new:agent:"))
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
func (gw *Gateway) askTopicName(thread, editMsg int, agent, cwd string, iso ...*pending) {
	suggestion := filepath.Base(strings.TrimRight(cwd, "/"))
	menu := &pending{kind: "newname", agent: agent, dirs: []string{cwd}, threadID: thread}
	if len(iso) > 0 && iso[0] != nil {
		menu.isolated = iso[0].isolated
		menu.owner = iso[0].owner
		suggestion = "Sandbox"
	}
	kb := Rows(
		[]Button{{Text: "⌨️ Type a name", CallbackData: "newname:type"}},
		[]Button{{Text: "📁 " + truncate(suggestion, 30), CallbackData: "newname:auto"}},
	)
	title := "What should this topic be called?\n<i>" +
		html.EscapeString(topicTitle(agent, "your name here")) + "</i>"
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

// ------------------------------------------------------- isolated sessions

// askWhoFor is the question that decides what an isolated session is for. A
// sandbox kept for yourself behaves like any other topic. One made for
// somebody else belongs to them alone: they are the only person the bot will
// answer in it, and the folder is all they can see of this machine.
func (gw *Gateway) askWhoFor(thread, editMsg int, agent string) {
	menu := &pending{kind: "isoowner", agent: agent, threadID: thread, isolated: true}
	kb := Rows(
		[]Button{{Text: "🙋 For me", CallbackData: "iso:mine"}},
		[]Button{{Text: "👤 For someone else", CallbackData: "iso:guest"}},
		[]Button{{Text: "Cancel", CallbackData: "dismiss"}},
	)
	text := "<b>Isolated session</b> — " + agentLabel(agent) + "\n\n" +
		"It gets a folder of its own and sees nothing else of this server: no other " +
		"projects, no memories, no settings, no other session. Commands, compilers " +
		"and the network all work in there.\n\nWho is it for?"
	if editMsg > 0 {
		gw.rememberMenu(editMsg, menu)
		if err := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, editMsg, text, kb); err == nil {
			return
		}
	}
	if m := gw.reply(thread, text, kb); m != nil {
		gw.rememberMenu(m.MessageID, menu)
	}
}

// askGuestID asks for the Telegram id of the person the sandbox is for.
func (gw *Gateway) askGuestID(thread, editMsg int, agent string, userID int64) {
	gw.awaitPath(userID, &pending{kind: "isoguest", agent: agent, threadID: thread, isolated: true})
	text := "Send me the <b>Telegram user id</b> of the person this session is for.\n\n" +
		"They will be the only one who can use that topic — you included. " +
		"Ask them to send <code>/id</code> here if they do not know theirs."
	gw.notify(thread, editMsg, text, Rows([]Button{{Text: "Cancel", CallbackData: "dismiss"}}))
}

// startIsolated creates the folder and the session behind it.
func (gw *Gateway) startIsolated(thread int, agent, name string, owner int64, editMsg int) {
	dir, err := gw.makeIsolatedDir(agent)
	if err != nil {
		gw.notify(thread, editMsg, "⚠️ "+html.EscapeString(err.Error()))
		return
	}
	sess, err := gw.createSessionIn(agent, dir, name, 0, true)
	if err != nil {
		gw.notify(thread, editMsg, "⚠️ "+html.EscapeString(err.Error()))
		return
	}
	if owner != 0 {
		sess = gw.store.Update(sess.ThreadID, func(s *Session) { s.OwnerID = owner })
		if sess == nil {
			return
		}
		// The session already went to the bridge without an owner; nothing
		// about the sandbox changes, but keep the two in step.
		_ = gw.bridge.Send(gw.command("start", sess, ""))
	}

	link := TopicLink(gw.cfg.ChatID, sess.ThreadID)
	text := "🔒 <b>Isolated " + agentLabel(agent) + "</b> is ready.\n" +
		"📁 <code>" + html.EscapeString(dir) + "</code>\n" +
		"<i>seen from inside as /workspace</i>"
	if owner != 0 {
		text += "\n👤 for <code>" + strconv.FormatInt(owner, 10) + "</code> only"
	}
	gw.notify(thread, editMsg, text, Rows([]Button{{Text: "➡️ Open the topic", URL: link}}))

	header := gw.sessionHeader(sess) + "\n\n" + isolatedWelcome(owner)
	if m := gw.reply(sess.ThreadID, header, gw.sessionKeyboard(sess)); m != nil {
		_ = gw.tg.Pin(gw.ctx, gw.cfg.ChatID, m.MessageID)
	}
}

func isolatedWelcome(owner int64) string {
	s := "This session is <b>isolated</b>. It works in <code>/workspace</code>, which is " +
		"the only folder that exists for it: the rest of this server — other projects, " +
		"memories, settings, other sessions — is not there at all. Shell commands, " +
		"package tools and the network all work normally.\n\n" +
		"There is no systemd in here. To keep something running — a website, an API, a " +
		"worker — use <code>svc</code>:\n" +
		"<code>svc start web npm run dev</code>\n" +
		"<code>svc list</code> · <code>svc log web</code> · <code>svc stop web</code>\n" +
		"A service started that way keeps running between messages and is brought back " +
		"if the session restarts. Ports it opens are reachable from outside.\n\n" +
		"Send a file back with <code>tg-send &lt;path&gt;</code> and it arrives in this topic."
	if owner != 0 {
		s += "\n\nThis topic is yours alone."
	}
	return s
}

// ---------------------------------------------------------------- pickers

func (gw *Gateway) showDirs(thread int, dir, kind, agent, name string, editMsg int) {
	// In an isolated topic the picker is the sandbox's own tree and nothing
	// above it: there is no rest of the machine to browse to.
	var iso *Session
	if kind == "cd" {
		if s := gw.store.Get(thread); s != nil && s.Isolated {
			iso = s
			if !within(sandboxRoot(s), dir) {
				dir = sandboxRoot(s)
			}
		}
	}
	dirs := gw.subdirs(dir)
	recent := gw.store.RecentDirs(4)
	if iso != nil {
		recent = nil
	}
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
	parent := filepath.Dir(dir)
	if iso != nil && !within(sandboxRoot(iso), parent) {
		parent = dir // the sandbox root has no "up"
	}
	if parent != dir {
		list = append(list, parent)
		nav = append(nav, Button{Text: "⬆️ up", CallbackData: kind + ":" + strconv.Itoa(len(list)-1)})
	}
	nav = append(nav,
		Button{Text: "✅ use this folder", CallbackData: kind + ":here"},
		Button{Text: "⌨️ type a path", CallbackData: kind + ":type"},
	)
	kb.InlineKeyboard = append(kb.InlineKeyboard, nav)
	// The other way to answer this question: do not pick a folder at all, and
	// have the session work in a sandbox of its own instead.
	if kind == "newdir" && sandboxAvailable() {
		kb.InlineKeyboard = append(kb.InlineKeyboard,
			[]Button{{Text: "🔒 Isolated sandbox", CallbackData: "iso:start"}})
	}

	title := "Folder for the new session:\n<code>" + html.EscapeString(dir) + "</code>"
	if kind == "cd" {
		title = "Working folder:\n<code>" + html.EscapeString(guestPath(iso, dir)) + "</code>"
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

var agentButtonLabel = map[string]string{
	"claude":      "🟠 Claude Code",
	"codex":       "🟢 Codex",
	"antigravity": "🟣 Antigravity",
}

// agentPicker offers the agents this machine actually has.
func (gw *Gateway) agentPicker(prefix string, current ...string) *Keyboard {
	cur := ""
	if len(current) > 0 {
		cur = current[0]
	}
	var buttons []Button
	for _, a := range gw.availableAgents() {
		label := agentButtonLabel[a]
		if a == cur {
			label = "✓ " + label
		}
		buttons = append(buttons, Button{Text: label, CallbackData: prefix + a})
	}
	return Grid(2, buttons)
}

// noAgentsText explains the one situation the bot cannot work around.
func noAgentsText() string {
	return "No agent is installed on this server.\n\nInstall one and it appears here:\n" +
		"· Claude Code — <code>claude</code>\n· Codex — <code>codex</code>\n· Antigravity — <code>agy</code>"
}

func (gw *Gateway) showAgents(thread int, sess *Session, editMsg int) {
	gw.notify(thread, editMsg, "Which agent runs in this topic?\nSwitching starts a fresh conversation.",
		gw.agentPicker("agt:", sess.Agent))
}

// cmdSessions opens the session browser: pick an agent, see its last
// conversations, open one to read what it was about and take it up again in a
// topic of its own.
func (gw *Gateway) cmdSessions(thread int, editMsg ...int) {
	edit := 0
	if len(editMsg) > 0 {
		edit = editMsg[0]
	}
	var rows [][]Button
	var pair []Button
	for _, a := range gw.availableAgents() {
		pair = append(pair, Button{Text: agentButtonLabel[a], CallbackData: "sessions:" + a})
		if len(pair) == 2 {
			rows = append(rows, pair)
			pair = nil
		}
	}
	if len(pair) > 0 {
		rows = append(rows, pair)
	}
	live := gw.store.List()
	if len(live) > 0 {
		rows = append(rows, []Button{{Text: fmt.Sprintf("🗂 Open topics (%d)", len(live)), CallbackData: "sessions:live"}})
	}
	rows = append(rows, []Button{{Text: "✨ New session", CallbackData: "new"}})
	text := "<b>Sessions</b>\nPick an agent to see its last conversations."
	if len(rows) == 1 {
		text = noAgentsText()
	}
	gw.notify(thread, edit, text, Rows(rows...))
}

// showLiveTopics lists the sessions that have a topic right now.
func (gw *Gateway) showLiveTopics(thread, editMsg int) {
	list := gw.store.List()
	if len(list) == 0 {
		gw.notify(thread, editMsg, "No session has a topic at the moment.", Rows(
			[]Button{{Text: "✨ New session", CallbackData: "new"}},
			[]Button{{Text: "⬅️ Back", CallbackData: "sessions"}},
		))
		return
	}
	var b strings.Builder
	var buttons []Button
	b.WriteString("<b>Open topics</b>\n")
	const maxListed = 20
	extra := 0
	if len(list) > maxListed {
		extra = len(list) - maxListed
		list = list[:maxListed]
	}
	for _, s := range list {
		lock := ""
		if s.Isolated {
			lock = "🔒 "
		}
		b.WriteString(fmt.Sprintf("\n• %s<b>%s</b> — <code>%s</code>", lock, agentLabel(s.Agent), html.EscapeString(s.Cwd)))
		if s.Turns > 0 {
			b.WriteString(fmt.Sprintf(" · %d turns", s.Turns))
		}
		if s.OwnerID != 0 {
			b.WriteString(fmt.Sprintf(" · for %d", s.OwnerID))
		}
		buttons = append(buttons, Button{
			Text: lock + agentLabel(s.Agent) + " · " + filepath.Base(s.Cwd),
			URL:  TopicLink(gw.cfg.ChatID, s.ThreadID),
		})
	}
	if extra > 0 {
		fmt.Fprintf(&b, "\n\n<i>and %d older ones</i>", extra)
	}
	kb := Grid(1, buttons)
	kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{{Text: "⬅️ Back", CallbackData: "sessions"}})
	gw.notify(thread, editMsg, b.String(), kb)
}

// pastPerAgent is how many of an agent's conversations the browser lists.
const pastPerAgent = 10

// showPastSessions lists what this agent still has on disk: the last ten
// conversations, whether or not they ever had a topic here.
func (gw *Gateway) showPastSessions(thread, editMsg int, agent string) {
	msgID := editMsg
	if msgID > 0 {
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "⏳ reading "+agentLabel(agent)+"'s history…", nil)
	} else if m := gw.reply(thread, "⏳ reading "+agentLabel(agent)+"'s history…", nil); m != nil {
		msgID = m.MessageID
	}
	guard("session browser", func() {
		past, err := gw.fetchHistory(agent)
		if err != nil {
			gw.notify(thread, msgID, "⚠️ "+html.EscapeString(err.Error()), Rows(
				[]Button{{Text: "⬅️ Back", CallbackData: "sessions"}}))
			return
		}
		past = gw.withKept(agent, past)
		if len(past) == 0 {
			gw.notify(thread, msgID, agentLabel(agent)+" has no conversations on this server yet.", Rows(
				[]Button{{Text: "✨ New session", CallbackData: "new"}},
				[]Button{{Text: "⬅️ Back", CallbackData: "sessions"}}))
			return
		}
		open := gw.openByRef()
		var buttons []Button
		for i, p := range past {
			mark := "💬"
			if p.Isolated {
				mark = "🔒"
			}
			if p.Kept != "" {
				mark = "📥"
			}
			if open[p.ID] != 0 {
				mark = "▶️"
			}
			label := p.Preview
			if p.Name != "" {
				label = p.Name
			}
			buttons = append(buttons, Button{Text: mark + " " + truncate(label, 34), CallbackData: "past:" + strconv.Itoa(i)})
		}
		kb := Grid(1, buttons)
		kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{{Text: "⬅️ Back", CallbackData: "sessions"}})
		text := "<b>" + agentLabel(agent) + "</b> — last " + strconv.Itoa(len(past)) + " conversations\n" +
			"<i>▶️ open in a topic · 🔒 isolated</i>"
		gw.rememberMenu(msgID, &pending{kind: "past", agent: agent, past: past, threadID: thread})
		if e := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, text, kb); e != nil {
			logf("session browser: %v", e)
		}
	})
}

// withKept folds the archive into the list: a session whose topic was deleted
// on purpose keeps the name it had, and one that never got as far as a
// conversation still appears, because it was deliberately kept.
func (gw *Gateway) withKept(agent string, past []PastSession) []PastSession {
	kept := map[string]*Session{}
	var orphans []*Session
	for _, s := range gw.store.Kept() {
		if s.Agent != agent {
			continue
		}
		if s.Ref == "" {
			orphans = append(orphans, s)
			continue
		}
		kept[s.Ref] = s
	}
	for i := range past {
		if s := kept[past[i].ID]; s != nil {
			past[i].Kept = KeptID(s)
			past[i].Name = s.Name
			if past[i].Cwd == "" {
				past[i].Cwd = s.Cwd
			}
		}
	}
	for _, s := range orphans {
		past = append(past, PastSession{
			ID: "", Preview: "(kept before it said anything)", Name: s.Name,
			Cwd: s.Cwd, When: s.LastUsed.Format(time.RFC3339),
			Isolated: s.Isolated, Kept: KeptID(s),
		})
	}
	return past
}

// openByRef maps a conversation id to the topic that is running it.
func (gw *Gateway) openByRef() map[string]int {
	out := map[string]int{}
	for _, s := range gw.store.List() {
		if s.Ref != "" {
			out[s.Ref] = s.ThreadID
		}
	}
	return out
}

// showPastSession is the summary of one conversation, with the button that
// takes it up again.
func (gw *Gateway) showPastSession(thread, editMsg int, agent string, p PastSession, idx int) {
	var b strings.Builder
	b.WriteString("<b>" + agentLabel(agent) + "</b>")
	if p.Name != "" {
		b.WriteString(" · " + html.EscapeString(p.Name))
	}
	if p.Isolated {
		b.WriteString(" · 🔒 isolated")
	}
	if p.Kept != "" {
		b.WriteString(" · 📥 kept")
	}
	if p.ID != "" {
		b.WriteString("\n<code>" + html.EscapeString(p.ID) + "</code>")
	}
	b.WriteString("\n\n")
	b.WriteString("<i>" + html.EscapeString(truncate(p.Preview, 400)) + "</i>\n")
	if p.Cwd != "" {
		b.WriteString("\n📁 <code>" + html.EscapeString(p.Cwd) + "</code>")
	}
	if p.Messages > 0 {
		fmt.Fprintf(&b, "\n💬 %d messages", p.Messages)
	}
	if when, err := time.Parse(time.RFC3339, p.When); err == nil {
		b.WriteString(" · " + since(when))
	}

	rows := [][]Button{}
	if topic := gw.openByRef()[p.ID]; topic != 0 {
		b.WriteString("\n\n<i>this conversation is open in a topic</i>")
		rows = append(rows, []Button{{Text: "➡️ Open the topic", URL: TopicLink(gw.cfg.ChatID, topic)}})
	}
	rows = append(rows,
		[]Button{{Text: "▶️ Resume in a new topic", CallbackData: "past:go:" + strconv.Itoa(idx)}},
		[]Button{{Text: "⬅️ Back", CallbackData: "sessions:" + agent}},
	)
	gw.notify(thread, editMsg, b.String(), Rows(rows...))
}

// resumePast gives a past conversation a topic of its own and points it at
// the conversation, so the next message carries on where it left off.
func (gw *Gateway) resumePast(thread, editMsg int, agent string, p PastSession) {
	name := truncate(strings.TrimSuffix(p.Preview, "…"), 28)
	if p.Name != "" {
		name = p.Name
	}
	if name == "" || strings.HasPrefix(name, "(") {
		name = "Resumed"
	}
	// A session that was kept when its topic went comes back as it was, with
	// its model, its settings and, if it had one, its owner.
	var restored *Session
	if p.Kept != "" {
		restored = gw.store.TakeKept(p.Kept)
	}
	var sess *Session
	var err error
	if p.Isolated {
		// Its folder is still there; the sandbox is rebuilt around it.
		dir := p.Cwd
		if dir == "" || !within(gw.isolatedRoot(), dir) {
			gw.notify(thread, editMsg, "That isolated session's folder is gone, so it cannot be resumed.")
			return
		}
		sess, err = gw.createSessionIn(agent, dir, name, 0, true)
	} else {
		dir := p.Cwd
		if dir == "" || !gw.cfg.PathAllowed(dir) {
			dir = gw.cfg.DefaultCwd
		}
		if st, e := os.Stat(dir); e != nil || !st.IsDir() {
			dir = gw.cfg.DefaultCwd
		}
		sess, err = gw.createSession(agent, dir, name, 0)
	}
	if err != nil {
		if restored != nil {
			// Put it back rather than losing it to a failed topic creation.
			gw.store.PutKept(p.Kept, restored)
		}
		gw.notify(thread, editMsg, "⚠️ "+html.EscapeString(err.Error()))
		return
	}
	ns := gw.store.Update(sess.ThreadID, func(s *Session) {
		if restored != nil {
			thread, title, cwd, root := s.ThreadID, s.Title, s.Cwd, s.Root
			*s = *restored
			s.ThreadID, s.Title, s.Cwd, s.Root = thread, title, cwd, root
			s.DetachedAt = time.Time{}
		}
		s.Ref = p.ID
		s.LastUsed = time.Now()
	})
	if ns == nil {
		return
	}
	_ = gw.bridge.Send(gw.command("start", ns, ""))

	gw.notify(thread, editMsg, "▶️ resumed <code>"+html.EscapeString(p.ID)+"</code> in a new topic.",
		Rows([]Button{{Text: "➡️ Open the topic", URL: TopicLink(gw.cfg.ChatID, ns.ThreadID)}}))
	header := gw.sessionHeader(ns) + "\n\n<i>continuing " + html.EscapeString(truncate(p.ID, 40)) +
		"</i>\nSend a message and it picks up where that conversation stopped."
	if m := gw.reply(ns.ThreadID, header, gw.sessionKeyboard(ns)); m != nil {
		_ = gw.tg.Pin(gw.ctx, gw.cfg.ChatID, m.MessageID)
	}
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
	p, err := hostPath(sess, sess.Cwd, arg)
	if err != nil {
		gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
		return
	}
	if !gw.pathAllowed(sess, p) {
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
	gw.reply(thread, "📁 now <code>"+html.EscapeString(guestPath(ns, p))+"</code>", gw.sessionKeyboard(ns))
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
		if p.isolated {
			gw.startIsolated(p.threadID, p.agent, text, p.owner, 0)
			return
		}
		if len(p.dirs) == 0 {
			return
		}
		gw.startSession(p.threadID, p.agent, p.dirs[0], text, 0)
	case "isoguest":
		id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(text, "@")), 10, 64)
		if err != nil || id <= 0 {
			gw.reply(p.threadID, "That is not a Telegram user id. It is a number — ask them to send <code>/id</code> here.", Rows(
				[]Button{{Text: "🔒 Try again", CallbackData: "iso:guest"}, {Text: "Cancel", CallbackData: "dismiss"}},
			))
			return
		}
		gw.askTopicName(p.threadID, 0, p.agent, "sandbox", &pending{isolated: true, owner: id})
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
	if cq.Message == nil {
		_ = gw.tg.AnswerCallback(gw.ctx, cq.ID, "Not for you.", true)
		return
	}
	thread := cq.Message.MessageThreadID
	if !cq.Message.IsTopicMessage {
		thread = 0
	}
	if !gw.authorised(cq.From, cq.Message.Chat.ID, thread) {
		_ = gw.tg.AnswerCallback(gw.ctx, cq.ID, "Not for you.", true)
		return
	}
	msgID := cq.Message.MessageID
	data := cq.Data
	ack := func(text string) { _ = gw.tg.AnswerCallback(gw.ctx, cq.ID, text, false) }

	head, arg := data, ""
	if i := strings.Index(data, ":"); i >= 0 {
		head, arg = data[:i], data[i+1:]
	}
	sess := gw.store.Get(thread)

	// The same rule as the commands: a guest may work in their topic, not run
	// the gateway from it.
	switch head {
	case "new", "list", "end", "newdir", "newagent", "newname", "sessions", "past", "iso":
		if !gw.mayList(cq.From.ID) {
			ack("Only an administrator can do that.")
			return
		}
	}

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

	case "sessions":
		ack("")
		switch {
		case arg == "":
			gw.cmdSessions(thread, msgID)
		case arg == "live":
			gw.showLiveTopics(thread, msgID)
		default:
			gw.showPastSessions(thread, msgID, arg)
		}

	case "past":
		p := gw.menu(msgID)
		if p == nil {
			ack("That menu expired.")
			return
		}
		resume := strings.HasPrefix(arg, "go:")
		i, err := strconv.Atoi(strings.TrimPrefix(arg, "go:"))
		if err != nil || i < 0 || i >= len(p.past) {
			ack("")
			return
		}
		if resume {
			ack("Resuming")
			gw.resumePast(thread, msgID, p.agent, p.past[i])
			return
		}
		ack("")
		// The list stays remembered: Back returns to it, and Resume needs it.
		gw.rememberMenu(msgID, p)
		gw.showPastSession(thread, msgID, p.agent, p.past[i], i)

	case "new":
		if arg == "" {
			ack("")
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "Which agent?", gw.agentPicker("new:agent:"))
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
				_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "📁 <code>"+html.EscapeString(guestPath(sess, dir))+"</code>", nil)
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

	case "iso":
		p := gw.menu(msgID)
		agent := ""
		if p != nil {
			agent = p.agent
		}
		if agent == "" {
			agent = gw.defaultAgent()
		}
		switch arg {
		case "start":
			ack("")
			gw.askWhoFor(thread, msgID, agent)
		case "mine":
			ack("")
			gw.askTopicName(thread, msgID, agent, "sandbox", &pending{isolated: true})
		case "guest":
			ack("")
			gw.askGuestID(thread, msgID, agent, cq.From.ID)
		default:
			ack("")
		}

	case "services":
		if sess == nil {
			ack("No session.")
			return
		}
		if !sess.Isolated {
			ack("Only isolated sessions run their own services.")
			return
		}
		ack("")
		go gw.runShell(thread, sess, sess.Cwd, "svc list")

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
		if p == nil || (len(p.dirs) == 0 && !p.isolated) {
			ack("That menu expired.")
			return
		}
		if arg == "type" {
			ack("Send me the name")
			gw.awaitPath(cq.From.ID, &pending{
				kind: "newname", agent: p.agent, dirs: p.dirs, threadID: p.threadID,
				isolated: p.isolated, owner: p.owner,
			})
			_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "Send the name for this topic as a message.", nil)
			return
		}
		ack("")
		if p.isolated {
			gw.startIsolated(p.threadID, p.agent, "", p.owner, msgID)
			return
		}
		gw.startSession(p.threadID, p.agent, p.dirs[0], "", msgID)

	case "bind":
		if !gw.agentInstalled(arg) {
			ack(agentLabel(arg) + " is not installed")
			return
		}
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
		if !gw.agentInstalled(arg) {
			ack(agentLabel(arg) + " is not installed")
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
		if arg != "close" && arg != "delete" && arg != "keep" {
			ack("")
			gw.askEnd(thread)
			return
		}
		if arg == "keep" {
			// The topic goes, the conversation stays: it moves to the archive,
			// where /sessions finds it and can give it a new topic. Startup
			// must not resurrect it, which is why it leaves the live map.
			ack("Kept")
			_ = gw.bridge.Send(Command{Type: "stop", SID: sidOf(thread)})
			kept := gw.store.Detach(thread)
			if kept != nil && kept.Ref != "" {
				logf("session %d detached, conversation %s kept", thread, kept.Ref)
			}
			if err := gw.tg.DeleteTopic(gw.ctx, gw.cfg.ChatID, thread); err != nil {
				logf("delete topic %d: %v", thread, err)
				_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID,
					"📥 <i>session kept; the topic could not be deleted: "+html.EscapeString(err.Error())+"</i>", nil)
				_ = gw.tg.CloseTopic(gw.ctx, gw.cfg.ChatID, thread)
			}
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

package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func logf(format string, args ...any) { log.Printf(format, args...) }

// pending remembers what a menu message is offering, so a button press can
// carry a short index instead of a long path.
type pending struct {
	kind     string // newagent | newdir | cd | model
	agent    string
	name     string // topic name the user asked for with "/new Name"
	dirs     []string
	models   []ModelInfo
	modelIdx int
	threadID int
	expires  time.Time
}

type modelCacheEntry struct {
	at     time.Time
	models []ModelInfo
}

type Gateway struct {
	cfg    *Config
	tg     *Telegram
	store  *Store
	bridge *Bridge
	ctx    context.Context
	me     *TGUser

	mu       sync.Mutex
	turns    map[int]*Turn
	running  map[int]bool
	menus    map[int]*pending
	awaitTP  map[int64]*pending // user id -> "send me a path" state
	modelsBy map[string]modelCacheEntry
	reqSeq   int64
}

func NewGateway(ctx context.Context, cfg *Config, store *Store) *Gateway {
	return &Gateway{
		cfg: cfg, tg: NewTelegram(cfg.APIBase, cfg.BotToken), store: store,
		bridge: NewBridge(cfg.BridgeCmd), ctx: ctx,
		turns: map[int]*Turn{}, running: map[int]bool{},
		menus: map[int]*pending{}, awaitTP: map[int64]*pending{},
		modelsBy: map[string]modelCacheEntry{},
	}
}

func sidOf(threadID int) string { return strconv.Itoa(threadID) }

// command builds a bridge command carrying everything the Config button can
// change, so a setting takes effect on the very next turn.
func (gw *Gateway) command(kind string, sess *Session, text string) Command {
	return Command{
		Type: kind, SID: sidOf(sess.ThreadID), Agent: sess.Agent, Cwd: sess.Cwd,
		Model: sess.Model, Effort: sess.Effort, Resume: sess.Ref, Text: text,
		PermMode: sess.PermMode, Thinking: sess.Thinking, MaxTurns: sess.MaxTurns,
		BudgetUSD: sess.BudgetUSD, UserSettings: sess.UserSettings, Fallback: sess.Fallback,
		Sandbox: sess.Sandbox, Approval: sess.Approval,
		WebSearch: sess.WebSearch, Network: sess.Network,
		TGChat:  strconv.FormatInt(gw.cfg.ChatID, 10),
		TGTopic: strconv.Itoa(sess.ThreadID),
		TGToken: gw.cfg.BotToken,
		TGTitle: sess.Title,
	}
}

// editInterval widens as more topics stream at once: Telegram counts every
// edit against the same group-wide flood limit, so three live sessions must
// each slow down or they take each other down with 429s.
func (gw *Gateway) editInterval() time.Duration {
	gw.mu.Lock()
	n := len(gw.running)
	gw.mu.Unlock()
	if n < 1 {
		n = 1
	}
	d := time.Duration(gw.cfg.EditIntervalMS) * time.Millisecond * time.Duration(n)
	if d > 12*time.Second {
		d = 12 * time.Second
	}
	return d
}

// ---------------------------------------------------------------- polling

func (gw *Gateway) Run() error {
	me, err := gw.tg.GetMe(gw.ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	gw.me = me
	chat, err := gw.tg.GetChat(gw.ctx, gw.cfg.ChatID)
	if err != nil {
		return fmt.Errorf("getChat: %w", err)
	}
	if !chat.IsForum {
		logf("WARNING: chat %d (%s) is not a forum; enable Topics in the group settings", chat.ID, chat.Title)
	}
	if err := gw.bridge.Start(); err != nil {
		return err
	}
	logf("gateway ready as @%s in %q (%d)", me.Username, chat.Title, chat.ID)
	if err := gw.tg.SetCommands(gw.ctx, gw.cfg.ChatID, botCommands()); err != nil {
		logf("could not publish the command menu: %v", err)
	}
	gw.reconcileTopics()

	offset := 0
	for {
		select {
		case <-gw.ctx.Done():
			return nil
		default:
		}
		ups, err := gw.tg.GetUpdates(gw.ctx, offset, 25)
		if err != nil {
			if gw.ctx.Err() != nil {
				return nil
			}
			logf("getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			gw.handleUpdate(u)
		}
	}
}

func (gw *Gateway) handleUpdate(u TGUpdate) {
	defer func() {
		if r := recover(); r != nil {
			logf("panic handling update %d: %v", u.UpdateID, r)
		}
	}()
	switch {
	case u.CallbackQuery != nil:
		gw.handleCallback(u.CallbackQuery)
	case u.Message != nil:
		gw.handleMessage(u.Message)
	}
}

func (gw *Gateway) authorised(user *TGUser, chatID int64) bool {
	if user == nil || !gw.cfg.UserAllowed(user.ID) {
		return false
	}
	return chatID == gw.cfg.ChatID
}

// ---------------------------------------------------------------- messages

func (gw *Gateway) handleMessage(m *TGMessage) {
	if m.From != nil && m.From.IsBot {
		return
	}
	if !gw.authorised(m.From, m.Chat.ID) {
		if m.From != nil && m.Text == "/id" {
			// The one thing an unknown chat may learn: its own ids, so the
			// operator can put them in the config.
			_, _ = gw.tg.Send(gw.ctx, m.Chat.ID, fmt.Sprintf("chat id: <code>%d</code>\nyour id: <code>%d</code>\ntopic: <code>%d</code>",
				m.Chat.ID, m.From.ID, m.MessageThreadID), SendOpts{ThreadID: m.MessageThreadID})
		}
		return
	}
	thread := m.MessageThreadID
	if !m.IsTopicMessage {
		thread = 0 // General
	}

	// A path we asked the user to type.
	if p := gw.takeAwaited(m.From.ID); p != nil && m.Text != "" && !strings.HasPrefix(m.Text, "/") {
		gw.finishPathEntry(p, strings.TrimSpace(m.Text), thread)
		return
	}

	if m.Document != nil || len(m.Photo) > 0 {
		gw.handleUpload(m, thread)
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		gw.handleCommand(m, thread, text)
		return
	}
	// General is the control centre, never a session: a session that ends up
	// there (an import, or a deleted topic) is given a topic at startup.
	if thread == 0 {
		gw.offerBind(0, "That was the General topic, which I keep for controls.")
		return
	}
	sess := gw.store.Get(thread)
	if sess == nil {
		gw.offerBind(thread, "There is no session in this topic yet.")
		return
	}
	gw.submit(sess, text)
}

func (gw *Gateway) offerBind(thread int, why string) {
	if thread == 0 {
		gw.reply(0, why+"\n\nStart one and I will make a topic for it.", Rows(
			[]Button{{Text: "✨ New session", CallbackData: "new"}},
			[]Button{{Text: "📋 Sessions", CallbackData: "list"}, {Text: "❓ Help", CallbackData: "help"}},
		))
		return
	}
	gw.reply(thread, why+"\n\nPick an agent to run in this topic:", Rows(
		[]Button{{Text: "🟠 Claude Code", CallbackData: "bind:claude"}, {Text: "🟢 Codex", CallbackData: "bind:codex"}},
	))
}

func (gw *Gateway) reply(thread int, text string, kb *Keyboard) *TGMessage {
	m, err := gw.tg.Send(gw.ctx, gw.cfg.ChatID, text, SendOpts{ThreadID: thread, Keyboard: kb})
	if err != nil {
		logf("reply failed: %v", err)
		return nil
	}
	return m
}

// ---------------------------------------------------------------- prompts

var errBusy = fmt.Errorf("busy")

func (gw *Gateway) submit(sess *Session, text string) {
	gw.mu.Lock()
	busy := gw.running[sess.ThreadID]
	gw.mu.Unlock()
	if busy {
		gw.reply(sess.ThreadID, "⏳ That session is still working. Stop it, kill it, or wait.", Rows(
			[]Button{{Text: "⏹ Stop", CallbackData: "stop"}, {Text: "💀 Kill", CallbackData: "kill"}},
		))
		return
	}
	if !gw.bridge.Alive() {
		gw.reply(sess.ThreadID, "The agent bridge is restarting. Try again in a moment.", nil)
		return
	}
	gw.mu.Lock()
	gw.running[sess.ThreadID] = true
	turn := gw.NewTurn(sess)
	gw.turns[sess.ThreadID] = turn
	gw.mu.Unlock()
	go gw.pump(sess, turn, text)
}

// sessionPreamble is prepended to the first message of a conversation so the
// agent knows where its files belong. Without it, both CLIs fall back to the
// private-chat delivery helpers their own notes tell them to use.
func sessionPreamble(sess *Session) string {
	return "[gateway] You are answering inside a Telegram topic. " +
		"To send the user a file, an archive or a build, run `tg-send <path>` " +
		"(optionally with --caption \"...\"); it delivers into this topic. " +
		"Do not use any other Telegram script, chat id or bot token. " +
		"Keep your replies short and readable: they are being read on a phone.\n\n"
}

func (gw *Gateway) pump(sess *Session, turn *Turn, text string) {
	sid := sidOf(sess.ThreadID)
	ch := gw.bridge.Subscribe(sid)
	defer func() {
		gw.bridge.Unsubscribe(sid, ch)
		gw.mu.Lock()
		delete(gw.running, sess.ThreadID)
		delete(gw.turns, sess.ThreadID)
		gw.mu.Unlock()
	}()

	gw.tg.TypingAction(gw.ctx, gw.cfg.ChatID, sess.ThreadID)
	typingBeat := 0
	if sess.Ref == "" && !strings.HasPrefix(text, "/") {
		text = sessionPreamble(sess) + text
	}
	err := gw.bridge.Send(gw.command("prompt", sess, text))
	if err != nil {
		turn.AddNote("⚠️ " + html.EscapeString(err.Error()))
		turn.Finish("")
		return
	}

	// Tick faster than the edit interval, or a flush that becomes due just
	// after a tick waits a whole extra period before it goes out.
	tick := time.Duration(gw.cfg.EditIntervalMS) * time.Millisecond / 3
	if tick < 300*time.Millisecond {
		tick = 300 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	idle := time.NewTimer(45 * time.Minute)
	defer idle.Stop()

	for {
		select {
		case <-gw.ctx.Done():
			turn.Finish("interrupted: the gateway is shutting down")
			return
		case <-ticker.C:
			turn.Flush(false)
			typingBeat++
			if typingBeat%3 == 0 {
				gw.tg.TypingAction(gw.ctx, gw.cfg.ChatID, sess.ThreadID)
			}
		case <-idle.C:
			turn.AddNote("⚠️ no response for 45 minutes, giving up")
			turn.Finish("")
			return
		case ev := <-ch:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(45 * time.Minute)
			switch ev.Type {
			case "started":
				if ev.Session != "" {
					sess = gw.store.Update(sess.ThreadID, func(s *Session) {
						s.Ref = ev.Session
						s.LastUsed = time.Now()
					})
					if sess == nil {
						return
					}
				}
			case "delta":
				turn.AddText(ev.Text)
				turn.Flush(false)
			case "text":
				turn.AddText(ev.Text)
				turn.Flush(false)
			case "thinking":
				if sess.Verbose || gw.cfg.ShowThinking {
					turn.AddNote("<blockquote expandable>💭 " + html.EscapeString(truncate(ev.Text, 600)) + "</blockquote>")
				}
			case "tool":
				turn.SetStep(ev.Status, ev.Name, ev.Detail)
				turn.Flush(false)
			case "file":
				turn.SetFileStep(ev.Kind, ev.Path)
				turn.Flush(false)
			case "todo":
				// The agent's own plan already reads as a checklist; only the
				// item it just started is worth a line of its own.
				for _, it := range ev.Items {
					if !it.Done {
						turn.SetStep("start", "todo-item", truncate(it.Text, 90))
						break
					}
				}
				turn.Flush(false)
			case "busy":
				turn.AddNote("<i>queued behind the running turn</i>")
			case "stopped":
				turn.AddNote("⏹ <i>stopped</i>")
			case "note":
				turn.AddNote("ℹ️ <i>" + html.EscapeString(ev.Message) + "</i>")
				turn.Flush(false)
			case "killed":
				turn.AddNote("💀 <i>" + html.EscapeString(ev.Message) + "</i>")
				turn.Flush(false)
			case "idle":
				// worker went quiet; nothing to show
			case "error":
				turn.AddNote("⚠️ " + html.EscapeString(truncate(ev.Message, 900)))
				turn.Flush(false)
			case "done":
				gw.store.Update(sess.ThreadID, func(s *Session) {
					if ev.Session != "" {
						s.Ref = ev.Session
					}
					s.Cost += ev.Cost
					s.Turns++
					s.LastUsed = time.Now()
				})
				// Release the topic before the closing edits: those are two
				// more round trips to Telegram, and a message sent in that
				// window would otherwise be refused as "still working".
				gw.mu.Lock()
				delete(gw.running, sess.ThreadID)
				gw.mu.Unlock()
				turn.Finish(gw.footer(sess, turn, ev))
				return
			}
		}
	}
}

func (gw *Gateway) footer(sess *Session, turn *Turn, ev Event) string {
	parts := []string{}
	if turn != nil && turn.steps > 0 {
		word := "steps"
		if turn.steps == 1 {
			word = "step"
		}
		parts = append(parts, fmt.Sprintf("%d %s", turn.steps, word))
	}
	if ev.DurationMS > 0 {
		parts = append(parts, fmtDuration(ev.DurationMS))
	}
	if ev.Tokens != nil && (ev.Tokens.Input > 0 || ev.Tokens.Output > 0) {
		parts = append(parts, fmt.Sprintf("%s in / %s out", fmtTokens(ev.Tokens.Input), fmtTokens(ev.Tokens.Output)))
	}
	if ev.Cost > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f", ev.Cost))
	}
	switch ev.Subtype {
	case "stopped":
		parts = append(parts, "stopped")
	case "error":
		parts = append(parts, "failed")
	case "bridge_down":
		parts = append(parts, "bridge restarted")
	}
	if len(parts) == 0 {
		return ""
	}
	return agentLabel(sess.Agent) + " · " + strings.Join(parts, " · ")
}

// ---------------------------------------------------------------- sessions

// shortAgent is the name a topic title carries: the label is long enough
// already once a project name is on the end of it.
func shortAgent(agent string) string {
	if agent == "codex" {
		return "Codex"
	}
	return "Claude"
}

// topicTitle is the one place a topic name is composed, so the agent always
// leads: "Claude • VPN App".
func topicTitle(agent, name string) string {
	name = bareName(strings.TrimSpace(name))
	if name == "" {
		return shortAgent(agent)
	}
	return truncate(shortAgent(agent)+" • "+name, 120)
}

// bareName strips an agent prefix off a title, so renaming or switching
// agents does not stack them up ("Claude • Codex • thing").
func bareName(title string) string {
	t := strings.TrimSpace(title)
	for _, sep := range []string{" • ", " · ", " - "} {
		for _, prefix := range []string{"Claude Code", "Claude", "Codex"} {
			if strings.HasPrefix(t, prefix+sep) {
				return strings.TrimSpace(t[len(prefix)+len(sep):])
			}
		}
	}
	if t == "Claude" || t == "Codex" || t == "Claude Code" {
		return ""
	}
	return t
}

func (gw *Gateway) createSession(agent, cwd, name string, thread int) (*Session, error) {
	name = strings.TrimSpace(name)
	auto := name == ""
	if auto {
		name = filepath.Base(strings.TrimRight(cwd, "/"))
	}
	title := topicTitle(agent, name)
	if thread == 0 {
		ft, err := gw.tg.CreateTopic(gw.ctx, gw.cfg.ChatID, title, topicColor(agent))
		if err != nil {
			return nil, fmt.Errorf("could not create a topic (is the bot an admin with Manage Topics?): %w", err)
		}
		thread = ft.MessageThreadID
	}
	sess := &Session{
		ThreadID: thread, Title: title, Name: name, AutoName: auto,
		Agent: agent, Cwd: cwd,
		Created: time.Now(), LastUsed: time.Now(),
	}
	gw.store.Put(sess)
	_ = gw.bridge.Send(Command{Type: "start", SID: sidOf(thread), Agent: agent, Cwd: cwd})
	return sess, nil
}

func (gw *Gateway) sessionHeader(sess *Session) string {
	model := sess.Model
	if model == "" {
		model = "default"
	}
	s := fmt.Sprintf("<b>%s</b>\n📁 <code>%s</code>\n🧠 %s", agentLabel(sess.Agent), html.EscapeString(sess.Cwd), html.EscapeString(model))
	if sess.Effort != "" {
		s += " · ⚡" + html.EscapeString(sess.Effort)
	}
	if extra := configSummary(sess); extra != "" {
		s += "\n⚙️ " + extra
	}
	if sess.Turns > 0 {
		s += fmt.Sprintf("\n💬 %d turns", sess.Turns)
		if sess.Cost > 0 {
			s += fmt.Sprintf(" · $%.4f", sess.Cost)
		}
		s += " · " + since(sess.LastUsed)
	}
	return s
}

func (gw *Gateway) sessionKeyboard(sess *Session) *Keyboard {
	return Rows(
		[]Button{{Text: "🧠 Model", CallbackData: "model"}, {Text: "⚡ Effort", CallbackData: "effort"}},
		[]Button{{Text: "📁 Folder", CallbackData: "cd"}, {Text: "🤖 Agent", CallbackData: "agent"}},
		[]Button{{Text: "⚙️ Config", CallbackData: "cfg"}, {Text: "🔎 Details: " + onOff(sess.Verbose), CallbackData: "verbose"}},
		[]Button{{Text: "🧹 New thread", CallbackData: "clear"}, {Text: "💀 Kill processes", CallbackData: "kill"}},
		[]Button{{Text: "🗑 End session", CallbackData: "end"}},
	)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---------------------------------------------------------------- uploads

func (gw *Gateway) handleUpload(m *TGMessage, thread int) {
	sess := gw.store.Get(thread)
	dir := gw.cfg.DefaultCwd
	if sess != nil {
		dir = sess.Cwd
	}
	var fileID, name string
	if m.Document != nil {
		fileID, name = m.Document.FileID, m.Document.FileName
	} else if len(m.Photo) > 0 {
		best := m.Photo[len(m.Photo)-1]
		fileID = best.FileID
		name = fmt.Sprintf("photo_%d.jpg", m.MessageID)
	}
	if fileID == "" {
		return
	}
	path, err := gw.tg.Download(gw.ctx, fileID, dir, name)
	if err != nil {
		gw.reply(thread, "⚠️ could not save that file: "+html.EscapeString(err.Error()), nil)
		return
	}
	caption := strings.TrimSpace(m.Caption)
	text := "📎 saved to <code>" + html.EscapeString(path) + "</code>"
	if sess != nil && caption != "" {
		gw.reply(thread, text, nil)
		gw.submit(sess, caption+"\n\n(the file is at "+path+")")
		return
	}
	kb := (*Keyboard)(nil)
	if sess != nil {
		kb = Rows([]Button{{Text: "👀 Ask the agent to look at it", CallbackData: "look"}})
	}
	gw.reply(thread, text, kb)
}

// ---------------------------------------------------------------- shell

func (gw *Gateway) runShell(thread int, cwd, cmdline string) {
	ctx, cancel := context.WithTimeout(gw.ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", cmdline)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	body := strings.TrimRight(string(out), "\n")
	status := "✓"
	if err != nil {
		status = "✗ " + err.Error()
	}
	if len([]rune(body)) > 3000 {
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("run-%d.txt", time.Now().Unix()))
		_ = os.WriteFile(tmp, out, 0o600)
		_ = gw.tg.SendDocument(gw.ctx, gw.cfg.ChatID, thread, tmp, "<code>"+html.EscapeString(truncate(cmdline, 200))+"</code> — "+status)
		_ = os.Remove(tmp)
		return
	}
	msg := "<code>$ " + html.EscapeString(truncate(cmdline, 300)) + "</code>\n"
	if body != "" {
		msg += "<pre>" + html.EscapeString(body) + "</pre>"
	}
	msg += "\n<i>" + html.EscapeString(status) + "</i>"
	gw.reply(thread, msg, nil)
}

// ---------------------------------------------------------------- helpers

func (gw *Gateway) subdirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

func (gw *Gateway) rememberMenu(msgID int, p *pending) {
	p.expires = time.Now().Add(30 * time.Minute)
	gw.mu.Lock()
	defer gw.mu.Unlock()
	for id, old := range gw.menus {
		if time.Now().After(old.expires) {
			delete(gw.menus, id)
		}
	}
	gw.menus[msgID] = p
}

func (gw *Gateway) menu(msgID int) *pending {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return gw.menus[msgID]
}

func (gw *Gateway) awaitPath(userID int64, p *pending) {
	p.expires = time.Now().Add(10 * time.Minute)
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.awaitTP[userID] = p
}

func (gw *Gateway) takeAwaited(userID int64) *pending {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	p := gw.awaitTP[userID]
	if p == nil {
		return nil
	}
	delete(gw.awaitTP, userID)
	if time.Now().After(p.expires) {
		return nil
	}
	return p
}

// ---------------------------------------------------------------- models

// fetchModels asks the agent itself what it can run, so the buttons always
// match the installed CLI instead of a list baked into this program.
func (gw *Gateway) fetchModels(agent string) ([]ModelInfo, error) {
	gw.mu.Lock()
	if c, ok := gw.modelsBy[agent]; ok && time.Since(c.at) < 10*time.Minute {
		gw.mu.Unlock()
		return c.models, nil
	}
	gw.reqSeq++
	sid := "models-" + agent + "-" + strconv.FormatInt(gw.reqSeq, 10)
	gw.mu.Unlock()

	ch := gw.bridge.Subscribe(sid)
	defer gw.bridge.Unsubscribe(sid, ch)
	if err := gw.bridge.Send(Command{Type: "models", SID: sid, Agent: agent}); err != nil {
		return nil, err
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev.Type {
			case "models":
				if len(ev.Models) == 0 {
					return nil, fmt.Errorf("the agent returned an empty model list")
				}
				gw.mu.Lock()
				gw.modelsBy[agent] = modelCacheEntry{at: time.Now(), models: ev.Models}
				gw.mu.Unlock()
				return ev.Models, nil
			case "error":
				return nil, fmt.Errorf("%s", ev.Message)
			}
		case <-deadline:
			return nil, fmt.Errorf("timed out waiting for the model list")
		case <-gw.ctx.Done():
			return nil, fmt.Errorf("shutting down")
		}
	}
}

// fallbackModels keeps the picker usable if the agent cannot be asked.
func (gw *Gateway) fallbackModels(agent string) []ModelInfo {
	var out []ModelInfo
	for _, c := range gw.cfg.Models(agent) {
		out = append(out, ModelInfo{ID: c.ID, Label: c.Label})
	}
	return out
}

// ---------------------------------------------------------------- startup

// reconcileTopics makes the group match the sessions the gateway remembers.
// Every session gets a topic: one that lost its topic (deleted while the
// gateway was down, or imported from an older gateway that ran in General)
// gets a fresh one, and the rest just get a short "still here" header.
func (gw *Gateway) reconcileTopics() {
	sessions := gw.store.List()
	if len(sessions) == 0 {
		return
	}
	logf("restoring %d session(s)", len(sessions))
	for _, sess := range sessions {
		_ = gw.bridge.Send(gw.command("start", sess, ""))
		header := gw.sessionHeader(sess) + "\n\n<i>restored after a restart</i>"
		if sess.ThreadID > 1 {
			m, err := gw.tg.Send(gw.ctx, gw.cfg.ChatID, header, SendOpts{
				ThreadID: sess.ThreadID, Keyboard: gw.sessionKeyboard(sess), Silent: true,
			})
			if err == nil {
				_ = m
				continue
			}
			if !isThreadMissing(err) {
				logf("restore %d: %v", sess.ThreadID, err)
				continue
			}
			logf("topic %d is gone, making a new one", sess.ThreadID)
		}
		gw.rehomeSession(sess, header)
	}
}

// rehomeSession creates a topic for a session that has none and moves its
// state (and the bridge's) to the new thread id.
func (gw *Gateway) rehomeSession(sess *Session, header string) {
	if sess.Name == "" {
		// A session from an older gateway has a title but no name; keep what
		// the topic was called rather than inventing one from the folder.
		if sess.Name = bareName(sess.Title); sess.Name == "" {
			sess.Name = filepath.Base(strings.TrimRight(sess.Cwd, "/"))
			sess.AutoName = true
		}
	}
	name := topicTitle(sess.Agent, sess.Name)
	ft, err := gw.tg.CreateTopic(gw.ctx, gw.cfg.ChatID, name, topicColor(sess.Agent))
	if err != nil {
		logf("could not create a topic for session %d: %v", sess.ThreadID, err)
		return
	}
	old := sess.ThreadID
	_ = gw.bridge.Send(Command{Type: "stop", SID: sidOf(old)})
	gw.store.Delete(old)
	sess.ThreadID = ft.MessageThreadID
	sess.Title = name
	gw.store.Put(sess)
	_ = gw.bridge.Send(gw.command("start", sess, ""))
	m, err := gw.tg.Send(gw.ctx, gw.cfg.ChatID, header, SendOpts{
		ThreadID: sess.ThreadID, Keyboard: gw.sessionKeyboard(sess), Silent: true,
	})
	if err == nil && m != nil {
		_ = gw.tg.Pin(gw.ctx, gw.cfg.ChatID, m.MessageID)
	}
	logf("session moved from topic %d to %d", old, sess.ThreadID)
}

func topicColor(agent string) int {
	if agent == "codex" {
		return 0x8EEE98
	}
	return 0x6FB9F0
}

// isThreadMissing reports whether Telegram refused because the topic is gone.
func isThreadMissing(err error) bool {
	e, ok := err.(*apiError)
	if !ok {
		return false
	}
	d := strings.ToLower(e.Desc)
	return strings.Contains(d, "thread not found") ||
		strings.Contains(d, "topic_id_invalid") ||
		strings.Contains(d, "topic deleted") ||
		strings.Contains(d, "message thread not found")
}

// configSummary lists only what has been changed from the defaults.
func configSummary(sess *Session) string {
	var parts []string
	if sess.Agent == "codex" {
		if sess.Sandbox != "" && sess.Sandbox != "danger-full-access" {
			parts = append(parts, "sandbox "+sess.Sandbox)
		}
		if sess.Approval != "" && sess.Approval != "never" {
			parts = append(parts, "approvals "+sess.Approval)
		}
		if sess.WebSearch != "" {
			parts = append(parts, "web "+sess.WebSearch)
		}
		if sess.Network != "" {
			parts = append(parts, "network "+sess.Network)
		}
	} else {
		if sess.PermMode != "" && sess.PermMode != "bypassPermissions" {
			parts = append(parts, permLabel(sess.PermMode))
		}
		if sess.Thinking > 0 {
			parts = append(parts, fmt.Sprintf("thinking %dk", sess.Thinking/1000))
		}
		if sess.MaxTurns > 0 {
			parts = append(parts, fmt.Sprintf("max %d turns", sess.MaxTurns))
		}
		if sess.BudgetUSD > 0 {
			parts = append(parts, fmt.Sprintf("budget $%.0f", sess.BudgetUSD))
		}
		if sess.UserSettings {
			parts = append(parts, "~/.claude settings")
		}
		if sess.Fallback != "" {
			parts = append(parts, "flagged → "+sess.Fallback)
		}
	}
	return strings.Join(parts, " · ")
}

func permLabel(mode string) string {
	switch mode {
	case "", "bypassPermissions":
		return "full access"
	case "acceptEdits":
		return "accept edits"
	case "plan":
		return "plan mode"
	case "default":
		return "ask first"
	case "dontAsk":
		return "don't ask"
	case "auto":
		return "auto"
	}
	return mode
}

// botCommands is what Telegram shows when you type "/" in the group.
func botCommands() []BotCommand {
	return []BotCommand{
		{"new", "start a session (agent and folder on buttons)"},
		{"sessions", "list the running sessions"},
		{"status", "what this session is, with its buttons"},
		{"model", "pick the model, then its effort level"},
		{"config", "permissions, thinking, limits, sandbox"},
		{"agent", "switch between Claude Code and Codex"},
		{"effort", "how hard the model should think"},
		{"compact", "compact the agent's context"},
		{"clear", "forget the conversation, keep the topic"},
		{"stop", "interrupt the current turn"},
		{"kill", "kill the agent and everything it started"},
		{"cd", "change the working folder"},
		{"pwd", "show the working folder"},
		{"ls", "list the working folder"},
		{"get", "send me a file from the folder"},
		{"run", "run a shell command in the folder"},
		{"verbose", "show tool output and thinking"},
		{"rename", "rename this topic"},
		{"end", "close or delete this topic"},
		{"help", "how this bot works"},
		{"id", "chat, user and topic ids"},
	}
}

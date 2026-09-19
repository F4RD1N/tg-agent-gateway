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

// guard runs fn on its own goroutine and keeps a panic in it from taking the
// gateway - and every other topic - down with it.
func guard(what string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logf("panic in %s: %v", what, r)
			}
		}()
		fn()
	}()
}

// pending remembers what a menu message is offering, so a button press can
// carry a short index instead of a long path.
type pending struct {
	kind     string // newagent | newdir | cd | model | owner | past
	agent    string
	name     string // topic name the user asked for with "/new Name"
	dirs     []string
	models   []ModelInfo
	skills   []SkillInfo
	past     []PastSession
	runs     []WorkflowRun
	page     int
	modelIdx int
	threadID int
	isolated bool     // this session is being created behind a sandbox
	owner    int64    // the one person the session belongs to, if it is for somebody else
	prompt   string   // a message waiting to be queued, applied or dropped
	images   []string // and whatever came attached to it
	form     *questionForm
	expires  time.Time
}

type modelCacheEntry struct {
	at     time.Time
	models []ModelInfo
}

type skillCacheEntry struct {
	at     time.Time
	skills []SkillInfo
}

type Gateway struct {
	cfg    *Config
	tg     *Telegram
	store  *Store
	bridge *Bridge
	ctx    context.Context
	me     *TGUser

	mu       sync.Mutex
	running  map[int]bool
	menus    map[int]*pending
	awaitTP  map[int64]*pending // user id -> "send me a path" state
	agentsAt time.Time
	agentsOK map[string]bool
	modelsBy map[string]modelCacheEntry
	skillsBy map[string]skillCacheEntry
	albums   map[string]*album
	waiting  map[int]*fileWait
	watching map[int]string // message id -> the workflow run it is redrawing
	turnMsg  map[int]int    // thread -> the message the running turn is writing
	queued   map[int][]pendingPrompt
	reqSeq   int64
}

// fileWait holds files that arrived without a caption. Nothing is sent to the
// agent until the next message says what to do with them.
type fileWait struct {
	paths   []string
	askedAt int // the message that asked, so it can be updated in place
	expires time.Time
}

// album collects the photos of one Telegram media group: they arrive as
// separate messages, usually with the caption on only one of them, so they are
// held briefly and handed to the agent together.
type album struct {
	paths   []string
	caption string
	thread  int
	timer   *time.Timer
}

func NewGateway(ctx context.Context, cfg *Config, store *Store) *Gateway {
	return &Gateway{
		cfg: cfg, tg: NewTelegram(cfg.APIBase, cfg.BotToken), store: store,
		bridge: NewBridge(cfg.BridgeCmd), ctx: ctx,
		running: map[int]bool{},
		menus:   map[int]*pending{}, awaitTP: map[int64]*pending{},
		agentsOK: map[string]bool{},
		modelsBy: map[string]modelCacheEntry{},
		skillsBy: map[string]skillCacheEntry{},
		albums:   map[string]*album{},
		waiting:  map[int]*fileWait{},
		watching: map[int]string{},
		turnMsg:  map[int]int{},
		queued:   map[int][]pendingPrompt{},
	}
}

func sidOf(threadID int) string { return strconv.Itoa(threadID) }

// command builds a bridge command carrying everything the Config button can
// change, so a setting takes effect on the very next turn.
func (gw *Gateway) command(kind string, sess *Session, text string) Command {
	c := Command{
		Type: kind, SID: sidOf(sess.ThreadID), Agent: sess.Agent, Cwd: sess.Cwd,
		Model: sess.Model, Effort: sess.Effort, Resume: sess.Ref, Text: text,
		PermMode: sess.PermMode, Thinking: sess.Thinking, MaxTurns: sess.MaxTurns,
		BudgetUSD: sess.BudgetUSD, NoUserSettings: sess.NoUserSettings, Fallback: sess.Fallback,
		Sandbox: sess.Sandbox, Approval: sess.Approval, Fast: sess.Fast,
		WebSearch: sess.WebSearch, Network: sess.Network,
		TGChat:  strconv.FormatInt(gw.cfg.ChatID, 10),
		TGTopic: strconv.Itoa(sess.ThreadID),
		TGToken: gw.cfg.BotToken,
		TGTitle: sess.Title,
	}
	if sess.Isolated {
		// The agent works in /workspace and knows nothing of the path this
		// machine keeps the folder at. The bot token stays out here: whoever
		// works in an isolated topic would otherwise be able to drive the
		// whole bot. Files come back through the session's outbox instead.
		c.SandboxDir = sandboxRoot(sess)
		c.Cwd = guestPath(sess, sess.Cwd)
		c.TGToken, c.TGChat = "", ""
	}
	return c
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
	switch available := gw.availableAgents(); len(available) {
	case 0:
		logf("WARNING: none of claude, codex or agy is on PATH; install one, or sessions cannot start")
	default:
		logf("agents available: %s", strings.Join(available, ", "))
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

// authorised decides whether this person may act here at all.
//
// Two kinds of person use the bot. An admin runs it and may work in any
// topic. A guest was given one isolated session and exists only in that
// topic: they are not in the allowed list, their id is on the session, and
// everywhere else the bot does not answer them.
func (gw *Gateway) authorised(user *TGUser, chatID int64, thread int) bool {
	if user == nil || chatID != gw.cfg.ChatID {
		return false
	}
	if gw.cfg.IsAdmin(user.ID) {
		return true
	}
	sess := gw.store.Get(thread)
	if sess != nil && sess.OwnerID != 0 {
		// Administrators manage every topic; other users need to own it.
		return user.ID == sess.OwnerID
	}
	return gw.cfg.UserAllowed(user.ID)
}

// mayList reports whether this person may see or start sessions. Guests may
// not: they get the one topic they were given.
func (gw *Gateway) mayList(userID int64) bool { return gw.cfg.IsAdmin(userID) }

// adminOnly answers a guest who reached for a control that is not theirs.
func (gw *Gateway) adminOnly(thread int) {
	gw.reply(thread, "Only an administrator of this gateway can do that.", nil)
}

// ---------------------------------------------------------------- messages

func (gw *Gateway) handleMessage(m *TGMessage) {
	if m.From != nil && m.From.IsBot {
		return
	}
	messageThread := m.MessageThreadID
	if !m.IsTopicMessage {
		messageThread = 0
	}
	if !gw.authorised(m.From, m.Chat.ID, messageThread) {
		if m.From != nil && m.Text == "/id" {
			// The one thing an unknown chat may learn: its own ids, so the
			// operator can put them in the config.
			_, _ = gw.tg.Send(gw.ctx, m.Chat.ID, fmt.Sprintf("chat id: <code>%d</code>\nyour id: <code>%d</code>\ntopic: <code>%d</code>",
				m.Chat.ID, m.From.ID, m.MessageThreadID), SendOpts{ThreadID: m.MessageThreadID})
		}
		return
	}
	thread := messageThread // 0 is General

	// A path we asked the user to type.
	if p := gw.takeAwaited(m.From.ID); p != nil && m.Text != "" && !strings.HasPrefix(m.Text, "/") {
		gw.finishPathEntry(p, strings.TrimSpace(m.Text), thread)
		return
	}

	if fileID, _, _ := mediaOf(m); fileID != "" {
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
	// A file arrived earlier with nothing to do: this message is what to do.
	if held, ask := gw.takeHeldFiles(thread); len(held) > 0 {
		if ask > 0 {
			_ = gw.tg.EditKeyboard(gw.ctx, gw.cfg.ChatID, ask, nil)
		}
		gw.submitWithFiles(sess, text, held)
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

// submitWithFiles sends a prompt that has files attached to it. Images are
// named for the agent and, where the agent supports it, handed over as image
// input rather than a path.
func (gw *Gateway) submitWithFiles(sess *Session, caption string, paths []string) {
	var images, others []string
	for _, p := range paths {
		// An isolated agent knows the file by the path it can see, which is
		// under /workspace, not the one this machine keeps it at.
		p = guestPath(sess, p)
		if isImage(p) {
			images = append(images, p)
		} else {
			others = append(others, p)
		}
	}
	var b strings.Builder
	if caption != "" {
		b.WriteString(caption)
	} else if len(images) > 0 {
		b.WriteString("Look at this and tell me what you see.")
	}
	// Images are handed to the agent as images; anything else is named, with
	// the kind spelled out so it knows what it is dealing with.
	if len(others) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if len(others) == 1 {
			b.WriteString("Attached file: " + others[0])
		} else {
			b.WriteString("Attached files:\n" + strings.Join(others, "\n"))
		}
	}
	gw.submitPrompt(sess, strings.TrimSpace(b.String()), images)
}

func isImage(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".heic", ".heif":
		return true
	}
	return false
}

func (gw *Gateway) submit(sess *Session, text string) {
	gw.submitPrompt(sess, text, nil)
}

func (gw *Gateway) submitPrompt(sess *Session, text string, images []string) {
	gw.submitPromptOrQueue(sess, text, images, false)
}

func (gw *Gateway) submitPromptOrQueue(sess *Session, text string, images []string, answer bool) {
	// Check and claim under one lock: two uploads landing together would
	// otherwise both pass the check and start a turn each.
	gw.mu.Lock()
	busy := gw.running[sess.ThreadID]
	if busy && answer {
		gw.queued[sess.ThreadID] = append(gw.queued[sess.ThreadID], pendingPrompt{text: text, images: images, at: time.Now()})
		gw.mu.Unlock()
		gw.reply(sess.ThreadID, "Answer queued for when the current turn finishes.", nil)
		return
	}
	if !busy && gw.bridge.Alive() {
		gw.running[sess.ThreadID] = true
	}
	gw.mu.Unlock()
	if busy {
		// Ask what to do with it rather than dropping it: queue it behind the
		// turn in flight, stop that turn and run this instead, or forget it.
		gw.offerQueue(sess, text, images)
		return
	}
	if !gw.bridge.Alive() {
		gw.reply(sess.ThreadID, "The agent bridge is restarting. Try again in a moment.", nil)
		return
	}
	if !answer {
		gw.clearAgentQuestions(sess.ThreadID)
	}
	// The Turn belongs to its pump goroutine and is never shared: everything
	// else asks gw.running whether a topic is busy.
	turn := gw.NewTurn(sess)
	guard("turn", func() { gw.pump(sess, turn, text, images) })
}

// sessionPreamble is prepended to the first message of a conversation so the
// agent knows where its files belong. Without it, both CLIs fall back to the
// private-chat delivery helpers their own notes tell them to use.
func sessionPreamble(sess *Session) string {
	s := "[gateway] You are answering inside a Telegram topic. " +
		"To send the user a file, an archive or a build, run `tg-send <path>` " +
		"(optionally with --caption \"...\"); it delivers into this topic. " +
		"Do not use any other Telegram script, chat id or bot token. " +
		"Keep your replies short and readable: they are being read on a phone."
	if sess != nil && sess.Isolated {
		s += " This session is isolated: /workspace is yours and is the only " +
			"folder that exists, the rest of the machine is not visible, and " +
			"`tg-send` hands the file to the gateway to post."
	}
	return s + "\n\n"
}

func (gw *Gateway) pump(sess *Session, turn *Turn, text string, images []string) {
	sid := sidOf(sess.ThreadID)
	ch := gw.bridge.Subscribe(sid)
	defer func() {
		gw.bridge.Unsubscribe(sid, ch)
		gw.mu.Lock()
		delete(gw.running, sess.ThreadID)
		delete(gw.turnMsg, sess.ThreadID)
		gw.mu.Unlock()
		// Whatever the turn left for delivery goes out now, however it ended.
		gw.drainOutbox(sess)
		// And whatever was written while it was working goes in now.
		guard("queue", func() { gw.drainQueue(sess) })
	}()

	gw.tg.TypingAction(gw.ctx, gw.cfg.ChatID, sess.ThreadID)
	typingBeat := 0
	if sess.Ref == "" && !strings.HasPrefix(text, "/") {
		text = sessionPreamble(sess) + text
	}
	cmd := gw.command("prompt", sess, text)
	cmd.Images = images
	err := gw.bridge.Send(cmd)
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
				// An isolated session delivers by leaving files in its
				// outbox, so a build that finished mid-turn arrives about
				// when the agent says it made one.
				gw.drainOutbox(sess)
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
				if gw.offerAgentQuestions(sess, ev.Text) {
					turn.seal()
					continue
				}
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
				// The agent narrates its own plan in words; the structured
				// checklist would only repeat it.
			case "busy":
				turn.AddNote("<i>queued behind the running turn</i>")
			case "stopped":
				turn.AddNote("⏹ <i>stopped</i>")
			case "note":
				turn.AddNote("ℹ️ <i>" + html.EscapeString(ev.Message) + "</i>")
				turn.Flush(false)
			case "killed":
				// The process is gone, so the turn is over. Waiting for a
				// paired "done" would leave the topic busy for 45 minutes if
				// one never came.
				turn.AddNote("💀 <i>" + html.EscapeString(ev.Message) + "</i>")
				gw.mu.Lock()
				delete(gw.running, sess.ThreadID)
				gw.mu.Unlock()
				turn.Finish(gw.footer(sess, turn, ev))
				return
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
	switch agent {
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
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
		for _, prefix := range []string{"Claude Code", "Claude", "Codex", "Antigravity"} {
			if strings.HasPrefix(t, prefix+sep) {
				return strings.TrimSpace(t[len(prefix)+len(sep):])
			}
		}
	}
	if t == "Claude" || t == "Codex" || t == "Claude Code" || t == "Antigravity" {
		return ""
	}
	return t
}

func (gw *Gateway) createSession(agent, cwd, name string, thread int) (*Session, error) {
	return gw.createSessionIn(agent, cwd, name, thread, false)
}

// createSessionIn makes a session, isolated or not. An isolated one gets a
// folder of its own under the isolated root and is never asked where to work.
func (gw *Gateway) createSessionIn(agent, cwd, name string, thread int, isolated bool) (*Session, error) {
	name = strings.TrimSpace(name)
	auto := name == ""
	if auto {
		name = filepath.Base(strings.TrimRight(cwd, "/"))
		if isolated {
			name = "Sandbox " + name
		}
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
	if isolated {
		sess.Isolated = true
		sess.Root = cwd
	}
	gw.store.Put(sess)
	_ = gw.bridge.Send(gw.command("start", sess, ""))
	return sess, nil
}

func (gw *Gateway) sessionHeader(sess *Session) string {
	model := sess.Model
	if model == "" {
		model = "default"
	}
	name := agentLabel(sess.Agent)
	if sess.Isolated {
		name = "🔒 " + name + " <i>(isolated)</i>"
	}
	s := fmt.Sprintf("<b>%s</b>\n📁 <code>%s</code>\n🧠 %s", name, html.EscapeString(guestPath(sess, sess.Cwd)), html.EscapeString(model))
	if sess.OwnerID != 0 {
		s += fmt.Sprintf("\n👤 <code>%d</code> · administrators also have access", sess.OwnerID)
	}
	if sess.Effort != "" {
		s += " · ⚡" + html.EscapeString(sess.Effort)
	}
	if speed := fastLabel(sess); speed != "" {
		s += " · " + speed
	}
	if !gw.agentInstalled(sess.Agent) {
		s += "\n⚠️ <i>" + agentLabel(sess.Agent) + " is not installed on this server</i>"
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
	rows := [][]Button{
		{{Text: "🧠 Model", CallbackData: "model"}, {Text: "⚡ Effort", CallbackData: "effort"}},
		{{Text: "📁 Folder", CallbackData: "cd"}, {Text: "🤖 Agent", CallbackData: "agent"}},
		{{Text: "🧩 Skills", CallbackData: "skills"}, {Text: "⚙️ Config", CallbackData: "cfg"}},
		{{Text: "🔎 Details: " + onOff(sess.Verbose), CallbackData: "verbose"}, {Text: "🔓 Mode", CallbackData: "cfg:perm"}},
	}
	// The row above the destructive one carries whatever this session has that
	// the others do not: services inside a sandbox, workflows under Claude.
	var extra []Button
	if sess.Isolated {
		// There is no systemd inside a sandbox, so what is running there is
		// worth a button of its own.
		extra = append(extra, Button{Text: "🛠 Services", CallbackData: "services"})
	}
	if sess.Agent == "claude" {
		extra = append(extra, Button{Text: "🧵 Workflows", CallbackData: "wf:reload"})
	}
	if sess.Agent == "codex" {
		extra = append(extra, fastButton(sess))
	}
	extra = append(extra, Button{Text: "🧹 New thread", CallbackData: "clear"})
	for len(extra) >= 2 {
		rows = append(rows, extra[:2])
		extra = extra[2:]
	}
	last := []Button{{Text: "💀 Kill processes", CallbackData: "kill"}, {Text: "🗑 End session", CallbackData: "end"}}
	if len(extra) == 1 {
		rows = append(rows, []Button{extra[0], last[0]})
		rows = append(rows, []Button{last[1]})
		return Rows(rows...)
	}
	rows = append(rows, last)
	return Rows(rows...)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---------------------------------------------------------------- uploads

// mediaOf pulls the one attachment out of a message, whatever kind it is.
func mediaOf(m *TGMessage) (fileID, name, kind string) {
	switch {
	case m.Document != nil:
		return m.Document.FileID, m.Document.FileName, "file"
	case len(m.Photo) > 0:
		// The last size is the largest one Telegram kept.
		return m.Photo[len(m.Photo)-1].FileID, "", "photo"
	case m.Video != nil:
		return m.Video.FileID, m.Video.FileName, "video"
	case m.Animation != nil:
		return m.Animation.FileID, m.Animation.FileName, "animation"
	case m.Audio != nil:
		return m.Audio.FileID, m.Audio.FileName, "audio"
	case m.Voice != nil:
		return m.Voice.FileID, "", "voice message"
	case m.VideoNote != nil:
		return m.VideoNote.FileID, "", "video note"
	case m.Sticker != nil:
		return m.Sticker.FileID, "", "sticker"
	}
	return "", "", ""
}

// defaultName invents a filename for the attachments Telegram sends without
// one, using the mime type so the extension is right.
func defaultName(m *TGMessage, kind string) string {
	mime := ""
	for _, f := range []*TGFile{m.Video, m.Audio, m.Voice, m.VideoNote, m.Animation, m.Sticker, m.Document} {
		if f != nil && f.MimeType != "" {
			mime = f.MimeType
			break
		}
	}
	ext := map[string]string{
		"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
		"video/mp4": ".mp4", "video/quicktime": ".mov", "video/webm": ".webm",
		"audio/mpeg": ".mp3", "audio/ogg": ".ogg", "audio/mp4": ".m4a", "audio/wav": ".wav",
		"application/pdf": ".pdf", "application/zip": ".zip", "text/plain": ".txt",
	}[mime]
	if ext == "" {
		switch kind {
		case "photo":
			ext = ".jpg"
		case "voice message":
			ext = ".ogg"
		case "video", "video note", "animation":
			ext = ".mp4"
		case "sticker":
			ext = ".webp"
		default:
			ext = ".bin"
		}
	}
	base := strings.ReplaceAll(kind, " ", "_")
	return fmt.Sprintf("%s_%d%s", base, m.MessageID, ext)
}

func (gw *Gateway) handleUpload(m *TGMessage, thread int) {
	sess := gw.store.Get(thread)
	dir := gw.cfg.DefaultCwd
	if sess != nil {
		dir = sess.Cwd
	}
	fileID, name, kind := mediaOf(m)
	if fileID == "" {
		return
	}
	if name == "" {
		name = defaultName(m, kind)
	}
	path, err := gw.tg.Download(gw.ctx, fileID, dir, name)
	if err != nil {
		gw.reply(thread, "⚠️ could not save that "+kind+": "+html.EscapeString(friendlyDownloadError(err)), nil)
		return
	}
	caption := strings.TrimSpace(m.Caption)

	// An album arrives as several messages, and usually only one of them
	// carries the caption, so they are collected before anything is decided.
	if m.MediaGroupID != "" && sess != nil {
		gw.collectAlbum(m.MediaGroupID, thread, path, caption)
		return
	}
	if sess == nil {
		gw.reply(thread, "📎 saved to <code>"+html.EscapeString(path)+"</code>", nil)
		return
	}
	if caption != "" {
		gw.reply(thread, "📎 <code>"+html.EscapeString(path)+"</code>", nil)
		gw.submitWithFiles(sess, caption, []string{path})
		return
	}
	// No caption: hold the file and ask. Nothing runs until the next message
	// says what to do with it.
	gw.holdFile(thread, path)
}

// friendlyDownloadError says what to do about the one failure that is common.
func friendlyDownloadError(err error) string {
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "too big") {
		return "Telegram only lets a bot fetch files up to 20 MB. Put it somewhere on the server and tell the agent the path instead."
	}
	return msg
}

// holdFile keeps an uncaptioned file until the next message explains it.
func (gw *Gateway) holdFile(thread int, paths ...string) {
	gw.mu.Lock()
	w := gw.waiting[thread]
	if w == nil || time.Now().After(w.expires) {
		w = &fileWait{}
		gw.waiting[thread] = w
	}
	w.paths = append(w.paths, paths...)
	w.expires = time.Now().Add(2 * time.Hour)
	ask := w.askedAt
	held := append([]string(nil), w.paths...)
	gw.mu.Unlock()

	var b strings.Builder
	if len(held) == 1 {
		b.WriteString("📎 <code>" + html.EscapeString(filepath.Base(held[0])) + "</code> is saved.\n\n")
	} else {
		fmt.Fprintf(&b, "📎 %d files are saved:\n", len(held))
		for _, p := range held {
			b.WriteString("· <code>" + html.EscapeString(filepath.Base(p)) + "</code>\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("<b>What should I do with it?</b>\nSend me the instruction and I will pass it to the agent along with the file.")
	kb := Rows([]Button{{Text: "✖️ Forget it", CallbackData: "unhold"}})

	if ask > 0 {
		if err := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, ask, b.String(), kb); err == nil {
			return
		}
	}
	if msg := gw.reply(thread, b.String(), kb); msg != nil {
		gw.mu.Lock()
		if w := gw.waiting[thread]; w != nil {
			w.askedAt = msg.MessageID
		}
		gw.mu.Unlock()
	}
}

// takeHeldFiles hands over the files waiting in a topic, if any.
func (gw *Gateway) takeHeldFiles(thread int) ([]string, int) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	w := gw.waiting[thread]
	if w == nil {
		return nil, 0
	}
	delete(gw.waiting, thread)
	if time.Now().After(w.expires) {
		return nil, w.askedAt
	}
	return w.paths, w.askedAt
}

func (gw *Gateway) collectAlbum(groupID string, thread int, path, caption string) {
	gw.mu.Lock()
	a := gw.albums[groupID]
	if a == nil {
		a = &album{thread: thread}
		gw.albums[groupID] = a
	}
	a.paths = append(a.paths, path)
	if caption != "" {
		a.caption = caption
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timer = time.AfterFunc(1500*time.Millisecond, func() {
		defer func() {
			if r := recover(); r != nil {
				logf("panic flushing an album: %v", r)
			}
		}()
		gw.flushAlbum(groupID)
	})
	gw.mu.Unlock()
}

func (gw *Gateway) flushAlbum(groupID string) {
	gw.mu.Lock()
	a := gw.albums[groupID]
	delete(gw.albums, groupID)
	gw.mu.Unlock()
	if a == nil || len(a.paths) == 0 {
		return
	}
	sess := gw.store.Get(a.thread)
	if sess == nil {
		return
	}
	if a.caption == "" {
		// No caption on any of them: hold them all and ask once.
		gw.holdFile(a.thread, a.paths...)
		return
	}
	gw.reply(a.thread, fmt.Sprintf("📎 saved %d files to <code>%s</code>", len(a.paths), html.EscapeString(sess.Cwd)), nil)
	gw.submitWithFiles(sess, a.caption, a.paths)
}

// ---------------------------------------------------------------- shell

func (gw *Gateway) runShell(thread int, sess *Session, cwd, cmdline string) {
	ctx, cancel := context.WithTimeout(gw.ctx, 2*time.Minute)
	defer cancel()
	name, argv, dir, err := sandboxCommand(sess, cwd, cmdline)
	if err != nil {
		gw.reply(thread, "⚠️ "+html.EscapeString(err.Error()), nil)
		return
	}
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = dir
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
	p := gw.menus[msgID]
	if p != nil && time.Now().After(p.expires) {
		delete(gw.menus, msgID)
		return nil
	}
	return p
}

// takeMenu is menu(), for a menu that may only be answered once. The offer on
// a message held back mid-turn is one of those: tapping Add twice must not
// queue it twice.
func (gw *Gateway) takeMenu(msgID int) *pending {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	p := gw.menus[msgID]
	delete(gw.menus, msgID)
	if p != nil && time.Now().After(p.expires) {
		return nil
	}
	return p
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

// ---------------------------------------------------------------- agents
//
// Only the agents actually installed on this machine are offered. Someone who
// has Claude Code but not Codex should never be shown a Codex button that
// fails on its first message.

// allAgents is the order they are offered in.
var allAgents = []string{"claude", "codex", "antigravity"}

func agentBinary(agent string) string {
	switch agent {
	case "codex":
		return "codex"
	case "antigravity":
		return "agy"
	}
	return "claude"
}

// agentInstalled reports whether an agent's CLI can be found, remembering the
// answer for a minute so a picker does not shell out on every tap.
func (gw *Gateway) agentInstalled(agent string) bool {
	gw.mu.Lock()
	fresh := time.Since(gw.agentsAt) < time.Minute
	ok, seen := gw.agentsOK[agent]
	gw.mu.Unlock()
	if fresh && seen {
		return ok
	}
	found := map[string]bool{}
	for _, a := range allAgents {
		found[a] = lookAgent(agentBinary(a))
	}
	gw.mu.Lock()
	gw.agentsOK = found
	gw.agentsAt = time.Now()
	gw.mu.Unlock()
	return found[agent]
}

// lookAgent checks PATH, then asks a login shell, because these CLIs install
// into ~/.local/bin, which a service PATH does not always carry.
func lookAgent(bin string) bool {
	if _, err := exec.LookPath(bin); err == nil {
		return true
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-lc", "command -v "+bin)
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// availableAgents lists what can actually be run here.
func (gw *Gateway) availableAgents() []string {
	var out []string
	for _, a := range allAgents {
		if gw.agentInstalled(a) {
			out = append(out, a)
		}
	}
	return out
}

// defaultAgent is what to use when there is nothing to choose between.
func (gw *Gateway) defaultAgent() string {
	if list := gw.availableAgents(); len(list) > 0 {
		return list[0]
	}
	return "claude"
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
				return append([]ModelInfo(nil), ev.Models...), nil
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

// fetchSkills asks the agent what it can be told to run by name.
func (gw *Gateway) fetchSkills(agent string) ([]SkillInfo, error) {
	gw.mu.Lock()
	if c, ok := gw.skillsBy[agent]; ok && time.Since(c.at) < 10*time.Minute {
		// A copy: the caller sorts it, and the cache is shared between topics.
		out := append([]SkillInfo(nil), c.skills...)
		gw.mu.Unlock()
		return out, nil
	}
	gw.reqSeq++
	sid := "skills-" + agent + "-" + strconv.FormatInt(gw.reqSeq, 10)
	gw.mu.Unlock()

	ch := gw.bridge.Subscribe(sid)
	defer gw.bridge.Unsubscribe(sid, ch)
	if err := gw.bridge.Send(Command{Type: "skills", SID: sid, Agent: agent}); err != nil {
		return nil, err
	}
	deadline := time.After(90 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev.Type {
			case "skills":
				gw.mu.Lock()
				gw.skillsBy[agent] = skillCacheEntry{at: time.Now(), skills: ev.Skills}
				gw.mu.Unlock()
				return append([]SkillInfo(nil), ev.Skills...), nil
			case "error":
				return nil, fmt.Errorf("%s", ev.Message)
			}
		case <-deadline:
			return nil, fmt.Errorf("timed out waiting for the skill list")
		case <-gw.ctx.Done():
			return nil, fmt.Errorf("shutting down")
		}
	}
}

// fetchHistory asks for the conversations this agent still has on disk. It is
// read fresh every time: somebody browsing their sessions has just been
// working in one of them.
func (gw *Gateway) fetchHistory(agent string) ([]PastSession, error) {
	gw.mu.Lock()
	gw.reqSeq++
	sid := "history-" + agent + "-" + strconv.FormatInt(gw.reqSeq, 10)
	gw.mu.Unlock()

	ch := gw.bridge.Subscribe(sid)
	defer gw.bridge.Unsubscribe(sid, ch)
	cmd := Command{Type: "history", SID: sid, Agent: agent, Limit: pastPerAgent}
	if root := gw.isolatedRoot(); root != "" {
		cmd.Roots = []string{root}
	}
	if err := gw.bridge.Send(cmd); err != nil {
		return nil, err
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev.Type {
			case "history":
				return append([]PastSession(nil), ev.Sessions...), nil
			case "error":
				return nil, fmt.Errorf("%s", ev.Message)
			}
		case <-deadline:
			return nil, fmt.Errorf("timed out reading the history")
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
	switch agent {
	case "codex":
		return 0x8EEE98 // green
	case "antigravity":
		return 0xCB86DB // purple
	}
	return 0x6FB9F0 // blue
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
		if sess.NoUserSettings {
			parts = append(parts, "no ~/.claude settings")
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
		{"sessions", "browse past conversations and resume one"},
		{"status", "what this session is, with its buttons"},
		{"model", "pick the model, then its effort level"},
		{"config", "permissions, thinking, limits, sandbox"},
		{"mode", "accept edits, plan, auto, manual…"},
		{"skills", "run a skill, prompt or command"},
		{"workflows", "recent workflow runs, and the live one"},
		{"fast", "Codex's priority tier: same model, twice the speed"},
		{"queue", "messages waiting for the current turn to end"},
		{"agent", "switch between Claude Code and Codex"},
		{"effort", "how hard the model should think"},
		{"compact", "compact the agent's context"},
		{"clear", "forget the conversation, keep the topic"},
		{"session", "show this topic's conversation id"},
		{"resume", "continue another conversation here"},
		{"stop", "interrupt the current turn"},
		{"kill", "kill the agent and everything it started"},
		{"cd", "change the working folder"},
		{"pwd", "show the working folder"},
		{"ls", "list the working folder"},
		{"get", "send me a file from the folder"},
		{"run", "run a shell command in the folder"},
		{"verbose", "show tool output and thinking"},
		{"rename", "rename this topic"},
		{"end", "close the topic, or delete it and keep the session"},
		{"services", "what an isolated session keeps running"},
		{"help", "how this bot works"},
		{"id", "chat, user and topic ids"},
	}
}

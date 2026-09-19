package main

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var conversationUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// cmdLoadSession finds the conversation before asking for its topic name.
// A UUID is always scoped to the requested agent.
func (gw *Gateway) cmdLoadSession(userID int64, thread int, command, arg string) {
	if !conversationUUID.MatchString(arg) {
		gw.reply(thread, "Usage: <code>/"+command+" &lt;uuid&gt;</code>\nSend one complete conversation UUID.", nil)
		return
	}
	agent := command
	if agent == "agy" {
		agent = "antigravity"
	}
	if !gw.agentInstalled(agent) {
		gw.reply(thread, agentLabel(agent)+" is not installed on this server.", nil)
		return
	}
	id := strings.ToLower(arg)
	p := &pending{kind: "loadlookup", agent: agent, threadID: thread, name: newSessionID(),
		expires: time.Now().Add(10 * time.Minute)}
	gw.mu.Lock()
	gw.awaitTP[userID] = p
	gw.mu.Unlock()
	gw.reply(thread, "Looking for that "+agentLabel(agent)+" session…", nil)
	guard("load session by UUID", func() {
		past, err := gw.findConversation(agent, id)
		gw.mu.Lock()
		if gw.awaitTP[userID] != p || time.Now().After(p.expires) {
			gw.mu.Unlock()
			return
		}
		if err != nil {
			delete(gw.awaitTP, userID)
			gw.mu.Unlock()
			gw.reply(thread, html.EscapeString(err.Error()), nil)
			return
		}
		ready := *p
		ready.kind, ready.past = "loadname", []PastSession{past}
		gw.awaitTP[userID] = &ready
		gw.mu.Unlock()
		gw.reply(thread, "Found <b>"+agentLabel(agent)+"</b> session <code>"+html.EscapeString(past.ID)+
			"</code>.\nWhat should the topic be called? Send the name here, or /cancel.",
			Rows([]Button{{Text: "Cancel", CallbackData: "loadcancel:" + p.name}}))
	})
}

func pastFromSession(s *Session) PastSession {
	return PastSession{ID: s.Ref, Cwd: s.Cwd, Name: s.Name, Isolated: s.Isolated}
}

func (gw *Gateway) findConversation(agent, id string) (PastSession, error) {
	for _, s := range gw.store.List() {
		if s.Agent == agent && strings.EqualFold(s.Ref, id) {
			return pastFromSession(s), nil
		}
	}
	for _, s := range gw.store.Kept() {
		if s.Agent == agent && strings.EqualFold(s.Ref, id) {
			p := pastFromSession(s)
			p.Kept = KeptID(s)
			return p, nil
		}
	}
	past, err := gw.fetchHistoryID(agent, id)
	if err != nil {
		return PastSession{}, err
	}
	for _, p := range past {
		if strings.EqualFold(p.ID, id) {
			return p, nil
		}
	}
	return PastSession{}, fmt.Errorf("No %s session with UUID %s was found on this server. Check the UUID and agent.", agentLabel(agent), id)
}

// Naming replies belong to the requesting user and topic. They must never
// become an agent prompt, or consume ordinary text from another topic.
func (gw *Gateway) handleSessionLoadInput(m *TGMessage, thread int) bool {
	text := strings.TrimSpace(m.Text)
	gw.mu.Lock()
	p := gw.awaitTP[m.From.ID]
	if p == nil || (p.kind != "loadname" && p.kind != "loadlookup") || p.threadID != thread {
		gw.mu.Unlock()
		return false
	}
	if time.Now().After(p.expires) {
		delete(gw.awaitTP, m.From.ID)
		gw.mu.Unlock()
		if strings.HasPrefix(text, "/") {
			return false
		}
		gw.reply(thread, "The session naming request expired. Send the UUID command again.", nil)
		return true
	}
	cmd, _ := splitCommand(text)
	if (strings.HasPrefix(text, "/") && cmd == "cancel") || !gw.mayList(m.From.ID) {
		delete(gw.awaitTP, m.From.ID)
		gw.mu.Unlock()
		gw.reply(thread, "Session loading cancelled.", nil)
		return true
	}
	if text == "" || strings.HasPrefix(text, "/") {
		gw.mu.Unlock()
		return false
	}
	if p.kind == "loadlookup" {
		gw.mu.Unlock()
		gw.reply(thread, "Still looking up that session. I will ask for its topic name when it is found.", nil)
		return true
	}
	if utf8.RuneCountInString(text) > 100 || strings.ContainsAny(text, "\r\n") {
		gw.mu.Unlock()
		gw.reply(thread, "Send a topic name on one line, up to 100 characters, or /cancel.", nil)
		return true
	}
	delete(gw.awaitTP, m.From.ID)
	gw.mu.Unlock()
	guard("open named session", func() { gw.finishSessionLoad(p, text) })
	return true
}

func (gw *Gateway) cancelSessionLoad(cq *TGCallbackQuery, thread int, token string) {
	gw.mu.Lock()
	p := gw.awaitTP[cq.From.ID]
	ok := p != nil && (p.kind == "loadlookup" || p.kind == "loadname") && p.threadID == thread && p.name == token
	if ok {
		delete(gw.awaitTP, cq.From.ID)
	}
	gw.mu.Unlock()
	if !ok {
		_ = gw.tg.AnswerCallback(gw.ctx, cq.ID, "That request has expired.", false)
		return
	}
	_ = gw.tg.AnswerCallback(gw.ctx, cq.ID, "Cancelled", false)
	_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, cq.Message.MessageID, "Session loading cancelled.", nil)
}

func (gw *Gateway) finishSessionLoad(p *pending, name string) {
	gw.resumeMu.Lock()
	defer gw.resumeMu.Unlock()
	if !gw.agentInstalled(p.agent) {
		gw.reply(p.threadID, agentLabel(p.agent)+" is not installed on this server.", nil)
		return
	}
	// Look again: it may have been opened, detached or renamed while the
	// person was choosing the name. Preserve its current owner and settings.
	for _, s := range gw.store.List() {
		if s.Agent != p.agent || !strings.EqualFold(s.Ref, p.past[0].ID) {
			continue
		}
		title := topicTitle(s.Agent, name)
		if err := gw.tg.EditTopic(gw.ctx, gw.cfg.ChatID, s.ThreadID, title); err != nil {
			gw.reply(p.threadID, "Could not rename the existing topic: "+html.EscapeString(err.Error()), nil)
			return
		}
		gw.store.Update(s.ThreadID, func(x *Session) { x.Name, x.Title, x.AutoName = name, title, false })
		gw.reply(p.threadID, "Session ready in <b>"+html.EscapeString(title)+"</b>.",
			Rows([]Button{{Text: "➡️ Open the topic", URL: TopicLink(gw.cfg.ChatID, s.ThreadID)}}))
		return
	}
	past, err := gw.findConversation(p.agent, p.past[0].ID)
	if err != nil {
		gw.reply(p.threadID, html.EscapeString(err.Error()), nil)
		return
	}
	past.Name = name
	gw.resumePast(p.threadID, 0, p.agent, past)
}

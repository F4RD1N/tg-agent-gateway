package main

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// codex exec currently flattens request_user_input_async into an agent_message
// containing a question and a bullet list. Parse only that narrow shape, never
// tool output, quotes, code, or ordinary lists without a question.
type agentQuestion struct {
	title   string
	options []string
}

type questionForm struct {
	questions    []agentQuestion
	answers      []string
	step         int
	conversation string
}

var questionOption = regexp.MustCompile(`^(?:[-*•]|[0-9]+[.)])\s+(.+)$`)

func parseAgentQuestions(text string) []agentQuestion {
	if strings.Contains(text, "```") || len([]rune(text)) > 8000 {
		return nil
	}
	var questions []agentQuestion
	var title []string
	var options []string
	finish := func() bool {
		q := strings.TrimSpace(strings.Join(title, "\n"))
		if !strings.ContainsAny(q, "?？؟") || len([]rune(q)) > 1800 || len(options) < 2 || len(options) > 6 {
			return false
		}
		questions = append(questions, agentQuestion{title: q, options: options})
		title, options = nil, nil
		return len(questions) <= 3
	}
	for _, raw := range strings.Split(strings.TrimSpace(text), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ">") || strings.HasPrefix(raw, "    ") || strings.HasPrefix(raw, "\t") {
			return nil
		}
		if match := questionOption.FindStringSubmatch(line); match != nil {
			label := strings.TrimSpace(match[1])
			if len(title) == 0 || len([]rune(label)) > 300 || strings.HasPrefix(label, "[ ]") || strings.HasPrefix(label, "[x]") {
				return nil
			}
			for _, option := range options {
				if option == label {
					return nil
				}
			}
			options = append(options, label)
			continue
		}
		if len(options) > 0 && !finish() {
			return nil
		}
		title = append(title, line)
	}
	if len(options) == 0 || !finish() {
		return nil
	}
	return questions
}

func questionView(form *questionForm) (string, *Keyboard) {
	q := form.questions[form.step]
	var body strings.Builder
	if len(form.questions) > 1 {
		fmt.Fprintf(&body, "<b>Question %d of %d</b>\n\n", form.step+1, len(form.questions))
	}
	body.WriteString(html.EscapeString(q.title))
	body.WriteString("\n")
	var rows [][]Button
	for i, option := range q.options {
		fmt.Fprintf(&body, "\n%d. %s", i+1, html.EscapeString(option))
		rows = append(rows, []Button{{Text: truncate(option, 60),
			CallbackData: fmt.Sprintf("answer:%d:%d", form.step, i)}})
	}
	body.WriteString("\n\n<i>Tap an answer, or type your own reply.</i>")
	return body.String(), Rows(rows...)
}

func (gw *Gateway) offerAgentQuestions(sess *Session, text string) bool {
	if sess.Agent != "codex" {
		return false
	}
	questions := parseAgentQuestions(text)
	if len(questions) == 0 {
		return false
	}
	form := &questionForm{questions: questions, conversation: sess.Ref}
	body, keyboard := questionView(form)
	m := gw.reply(sess.ThreadID, body, keyboard)
	if m == nil {
		return false // Preserve the original text if Telegram rejected the menu.
	}
	gw.rememberMenu(m.MessageID, &pending{kind: "answer", agent: sess.Agent,
		threadID: sess.ThreadID, form: form})
	return true
}

// Answer callbacks are bound to a topic, conversation, message and question
// index. A repeated tap from a previous step cannot answer the following one.
func (gw *Gateway) answerAgentQuestion(cq *TGCallbackQuery, thread int, arg string, sess *Session) {
	ack := func(text string) { _ = gw.tg.AnswerCallback(gw.ctx, cq.ID, text, false) }
	parts := strings.Split(arg, ":")
	if len(parts) != 2 || sess == nil {
		ack("That question is no longer available.")
		return
	}
	step, errStep := strconv.Atoi(parts[0])
	index, errIndex := strconv.Atoi(parts[1])
	msgID := cq.Message.MessageID
	gw.mu.Lock()
	p := gw.menus[msgID]
	if errStep != nil || errIndex != nil || p == nil || p.kind != "answer" || p.form == nil ||
		p.threadID != thread || p.agent != sess.Agent || p.form.conversation != sess.Ref || time.Now().After(p.expires) ||
		step != p.form.step || index < 0 || index >= len(p.form.questions[step].options) {
		gw.mu.Unlock()
		ack("That question expired or was already answered.")
		return
	}
	// Do not consume the final answer while the bridge cannot receive it.
	if step+1 == len(p.form.questions) && !gw.bridge.Alive() {
		gw.mu.Unlock()
		ack("The agent is reconnecting. Please tap again shortly.")
		return
	}
	form := p.form
	form.answers = append(form.answers, form.questions[step].options[index])
	form.step++
	if form.step < len(form.questions) {
		body, keyboard := questionView(form)
		gw.mu.Unlock()
		ack("Selected")
		if err := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, body, keyboard); err != nil {
			// Keep the visible buttons usable when advancing the form fails.
			gw.mu.Lock()
			form.step--
			form.answers = form.answers[:len(form.answers)-1]
			gw.mu.Unlock()
			gw.reply(thread, "Could not show the next question. Please tap your answer again, or type a reply.", nil)
		}
		return
	}
	delete(gw.menus, msgID)
	gw.mu.Unlock()

	var answer strings.Builder
	for i, q := range form.questions {
		if i > 0 {
			answer.WriteString("\n\n")
		}
		fmt.Fprintf(&answer, "%s\nAnswer: %s", q.title, form.answers[i])
	}
	ack("Answer selected")
	// This always becomes agent input, even when an option starts with a slash.
	// It must never be interpreted as a gateway command.
	_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID,
		"<b>Selected answer</b>\n\n"+html.EscapeString(answer.String()), nil)
	gw.submitPromptOrQueue(sess, answer.String(), nil, true)
}

// A typed follow-up supersedes old choices. Keep expired keyboards inert even
// if Telegram cannot remove them; their pending state is removed first.
func (gw *Gateway) clearAgentQuestions(thread int) {
	var messages []int
	gw.mu.Lock()
	for id, p := range gw.menus {
		if p.kind == "answer" && p.threadID == thread {
			delete(gw.menus, id)
			messages = append(messages, id)
		}
	}
	gw.mu.Unlock()
	for _, id := range messages {
		_ = gw.tg.EditKeyboard(gw.ctx, gw.cfg.ChatID, id, nil)
	}
}

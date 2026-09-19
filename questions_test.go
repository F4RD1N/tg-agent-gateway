package main

import (
	"strings"
	"testing"
	"time"
)

const restartQuestion = "The fix is ready. Restart the gateway now? This will interrupt sessions and may lose queued messages.\n- Restart now\n- Keep sessions running; activate later"

func TestParseAgentQuestions(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		count      int
	}{
		{"reported-restart-question", restartQuestion, 1},
		{"real-Codex-probe", "Which test color do you prefer?\n- Blue\n- Green", 1},
		{"two-questions", "Color?\n- Blue\n- Green\n\nSize?\n• Small\n• Large", 2},
		{"numbered", "Choose a size?\n1. Small\n2. Large", 1},
		{"unicode", "کدام رنگ؟\n- آبی\n- سبز", 1},
		{"ordinary-list", "Completed changes:\n- Restart now\n- Keep running", 0},
		{"quoted-question", "> Restart now?\n> - Yes\n> - No", 0},
		{"fenced-example", "```\nRestart now?\n- Yes\n- No\n```", 0},
		{"indented-code", "    Restart now?\n    - Yes\n    - No", 0},
		{"task-list", "What is done?\n- [x] Tests\n- [ ] Build", 0},
		{"trailing-prose", "Color?\n- Blue\n- Green\nThese are available colors.", 0},
		{"duplicate-options", "Color?\n- Blue\n- Blue", 0},
		{"only-one-option", "Color?\n- Blue", 0},
		{"too-many-options", "Number?\n- 1\n- 2\n- 3\n- 4\n- 5\n- 6\n- 7", 0},
		{"too-many-questions", strings.Repeat("Color?\n- Blue\n- Green\n\n", 4), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(parseAgentQuestions(tc.text)); got != tc.count {
				t.Fatalf("got %d questions, want %d", got, tc.count)
			}
		})
	}
}

func waitQuestionTurns(t *testing.T, gw *Gateway, thread int, turns int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		busy := gw.running[thread]
		gw.mu.Unlock()
		if s := gw.store.Get(thread); s != nil && s.Turns == turns && !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("topic %d did not finish %d turns", thread, turns)
}

func TestCodexQuestionButtonSendsAnswerOnce(t *testing.T) {
	gw, f, work := newTestGateway(t)
	const thread = 91
	gw.store.Put(&Session{ThreadID: thread, Agent: "codex", Cwd: work, Ref: "question-test"})
	gw.handleUpdate(msg(thread, "ASK-CHOICES"))
	menu := f.waitFor(t, "sendMessage", "Which test color", 3*time.Second)
	if !has(buttons(menu), "answer:0:0") || !has(buttons(menu), "answer:0:1") {
		t.Fatalf("question needs glass buttons: %v", buttons(menu))
	}
	waitQuestionTurns(t, gw, thread, 1)
	for _, call := range f.since(0) {
		if call.Method == "editMessageReplyMarkup" && targetID(&call) == menu.ID {
			t.Fatal("finishing the turn must leave the question buttons available")
		}
	}
	gw.handleUpdate(press(thread, menu.ID, "answer:0:1"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Received input:", 3*time.Second)
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Answer: Green", 3*time.Second)
	gw.handleUpdate(press(thread, menu.ID, "answer:0:1"))
	waitQuestionTurns(t, gw, thread, 2)
	if gw.queueLen(thread) != 0 || gw.menu(menu.ID) != nil {
		t.Fatal("an answered menu must not enqueue a second answer")
	}
}

func TestQuestionAnswerQueuesWithoutInterrupting(t *testing.T) {
	gw, f, work := newTestGateway(t)
	const thread = 92
	gw.store.Put(&Session{ThreadID: thread, Agent: "codex", Cwd: work, Ref: "question-test"})
	gw.handleUpdate(msg(thread, "SLOW please"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "thinking about it", 3*time.Second)
	label := "/end " + strings.Repeat("🟢長い選択肢", 14)
	if !gw.offerAgentQuestions(gw.store.Get(thread), "Which input should be sent?\n- Short\n- "+label) {
		t.Fatal("long Unicode options should render with a short button and full text")
	}
	menu := f.waitFor(t, "sendMessage", "Which input should be sent", 3*time.Second)
	gw.handleUpdate(press(thread, menu.ID, "answer:0:1"))
	gw.handleUpdate(press(thread, menu.ID, "answer:0:1"))
	gw.mu.Lock()
	busy := gw.running[thread]
	gw.mu.Unlock()
	if !busy || gw.queueLen(thread) != 1 {
		t.Fatal("click must queue one answer and leave the running turn alone")
	}
	for _, call := range f.since(0) {
		if has(buttons(&call), "q:add") || call.Method == "closeForumTopic" || call.Method == "deleteForumTopic" {
			t.Fatal("an answer must not become another queue menu or a gateway command")
		}
	}
	gw.handleUpdate(msg(thread, "/stop"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Received input:", 3*time.Second)
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Answer: "+label, 3*time.Second)
	waitQuestionTurns(t, gw, thread, 2)
}

func TestQuestionFormCollectsAllAnswers(t *testing.T) {
	gw, f, work := newTestGateway(t)
	const thread = 93
	s := &Session{ThreadID: thread, Agent: "codex", Cwd: work, Ref: "question-test"}
	gw.store.Put(s)
	gw.offerAgentQuestions(s, "Color?\n- Blue\n- Green\n\nSize?\n- Small\n- Large")
	menu := f.waitFor(t, "sendMessage", "Question 1 of 2", 3*time.Second)
	gw.handleUpdate(press(thread, menu.ID, "answer:0:1"))
	f.waitFor(t, "editMessageText", "Question 2 of 2", 3*time.Second)
	gw.handleUpdate(press(thread, menu.ID, "answer:0:1")) // stale double tap
	if gw.store.Get(thread).Turns != 0 || gw.menu(menu.ID).form.step != 1 {
		t.Fatal("a repeated tap must not answer the next question")
	}
	gw.handleUpdate(press(thread, menu.ID, "answer:1:0"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Received input:", 3*time.Second)
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Color?\nAnswer: Green\n\nSize?\nAnswer: Small", 3*time.Second)
	waitQuestionTurns(t, gw, thread, 1)
}

func TestQuestionCallbacksRespectSessionAndUser(t *testing.T) {
	for _, variant := range []string{"stranger", "other-topic", "other-chat", "other-conversation", "expired", "bad-index", "bad-step"} {
		t.Run(variant, func(t *testing.T) {
			gw, f, work := newTestGateway(t)
			gw.cfg.AdminUserIDs = []int64{42}
			s := &Session{ThreadID: 94, Agent: "codex", Cwd: work, Ref: "original", OwnerID: 777}
			gw.store.Put(s)
			gw.store.Put(&Session{ThreadID: 95, Agent: "codex", Cwd: work, Ref: "original"})
			gw.offerAgentQuestions(s, restartQuestion)
			menu := f.waitFor(t, "sendMessage", "Restart the gateway", 3*time.Second)
			click := press(94, menu.ID, "answer:0:0")
			switch variant {
			case "stranger":
				click.CallbackQuery.From.ID = 999
			case "other-topic":
				click.CallbackQuery.Message.MessageThreadID = 95
			case "other-chat":
				click.CallbackQuery.Message.Chat.ID = -9876
			case "other-conversation":
				gw.store.Update(94, func(s *Session) { s.Ref = "new" })
			case "expired":
				gw.mu.Lock()
				gw.menus[menu.ID].expires = time.Now().Add(-time.Minute)
				gw.mu.Unlock()
			case "bad-index":
				click.CallbackQuery.Data = "answer:0:99"
			case "bad-step":
				click.CallbackQuery.Data = "answer:-1:0"
			}
			gw.handleUpdate(click)
			gw.mu.Lock()
			busy := len(gw.running)
			gw.mu.Unlock()
			if busy != 0 || gw.queueLen(94)+gw.queueLen(95) != 0 {
				t.Fatal("invalid callback submitted an answer")
			}
		})
	}
}

func TestTypedAnswerExpiresQuestionButtons(t *testing.T) {
	gw, f, work := newTestGateway(t)
	s := &Session{ThreadID: 96, Agent: "codex", Cwd: work, Ref: "original", OwnerID: 777}
	gw.store.Put(s)
	gw.offerAgentQuestions(s, restartQuestion)
	menu := f.waitFor(t, "sendMessage", "Restart the gateway", 3*time.Second)
	u := msg(96, "Answer: I prefer something else")
	u.Message.From.ID = 777
	gw.handleUpdate(u)
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Received input:", 3*time.Second)
	gw.handleUpdate(press(96, menu.ID, "answer:0:0"))
	if gw.menu(menu.ID) != nil || gw.queueLen(96) != 0 {
		t.Fatal("typing an answer must invalidate the old buttons")
	}
	waitQuestionTurns(t, gw, 96, 1)
}

func TestOwnerAndAdminCanAnswerGuestQuestion(t *testing.T) {
	for _, user := range []int64{777, 42} {
		gw, f, work := newTestGateway(t)
		gw.cfg.AdminUserIDs = []int64{42}
		s := &Session{ThreadID: 97, Agent: "codex", Cwd: work, Ref: "original", OwnerID: 777}
		gw.store.Put(s)
		gw.offerAgentQuestions(s, "Save <draft>?\n- Keep <draft>\n- Discard")
		menu := f.waitFor(t, "sendMessage", "Save &lt;draft&gt;?", 3*time.Second)
		click := press(97, menu.ID, "answer:0:0")
		click.CallbackQuery.From.ID = user
		gw.handleUpdate(click)
		f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Received input:", 3*time.Second)
		f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Answer: Keep &lt;draft&gt;", 3*time.Second)
		waitQuestionTurns(t, gw, 97, 1)
	}
}

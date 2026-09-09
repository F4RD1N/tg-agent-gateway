package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- a fake Bot API ----------

type sentCall struct {
	Method string
	Params map[string]any
}

type fakeTG struct {
	srv *httptest.Server

	mu     sync.Mutex
	calls  []sentCall
	nextID int
}

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{nextID: 1000}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]
		var params map[string]any
		if body, _ := io.ReadAll(r.Body); len(body) > 0 {
			_ = json.Unmarshal(body, &params)
		}
		f.mu.Lock()
		f.calls = append(f.calls, sentCall{Method: method, Params: params})
		f.nextID++
		id := f.nextID
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "getMe":
			io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":"testbot"}}`)
		case "getChat":
			io.WriteString(w, `{"ok":true,"result":{"id":-100123,"type":"supergroup","title":"Test","is_forum":true}}`)
		case "sendMessage":
			thread := 0
			if v, ok := params["message_thread_id"].(float64); ok {
				thread = int(v)
			}
			resp := map[string]any{"ok": true, "result": map[string]any{
				"message_id": id, "message_thread_id": thread, "chat": map[string]any{"id": -100123},
			}}
			_ = json.NewEncoder(w).Encode(resp)
		case "getFile":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{
				"file_path": "photos/upload.jpg", "file_size": 11,
			}})
		case "createForumTopic":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{
				"message_thread_id": 555, "name": params["name"],
			}})
		default:
			io.WriteString(w, `{"ok":true,"result":true}`)
		}
	})
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "PRETEND-JPEG")
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTG) since(n int) []sentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > len(f.calls) {
		return nil
	}
	out := make([]sentCall, len(f.calls)-n)
	copy(out, f.calls[n:])
	return out
}

func (f *fakeTG) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// find returns the first call of that method containing sub in its text.
func (f *fakeTG) find(method, sub string) *sentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.calls {
		c := f.calls[i]
		if c.Method != method {
			continue
		}
		if sub == "" {
			return &c
		}
		if txt, _ := c.Params["text"].(string); strings.Contains(txt, sub) {
			return &c
		}
	}
	return nil
}

// findAny looks across several methods, since streamed text may land in the
// first sendMessage or in any later edit depending on timing.
func (f *fakeTG) findAny(methods []string, sub string) *sentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.calls {
		c := f.calls[i]
		ok := false
		for _, m := range methods {
			if c.Method == m {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		if txt, _ := c.Params["text"].(string); strings.Contains(txt, sub) {
			return &c
		}
	}
	return nil
}

func (f *fakeTG) waitForAny(t *testing.T, methods []string, sub string, d time.Duration) *sentCall {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c := f.findAny(methods, sub); c != nil {
			return c
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no %v containing %q within %s", methods, sub, d)
	return nil
}

func (f *fakeTG) waitFor(t *testing.T, method, sub string, d time.Duration) *sentCall {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c := f.find(method, sub); c != nil {
			return c
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("no %s containing %q within %s", method, sub, d)
	return nil
}

// buttons pulls the callback_data values out of a recorded call.
func buttons(c *sentCall) []string {
	var out []string
	rm, ok := c.Params["reply_markup"].(map[string]any)
	if !ok {
		return nil
	}
	rows, _ := rm["inline_keyboard"].([]any)
	for _, row := range rows {
		for _, b := range row.([]any) {
			m := b.(map[string]any)
			if d, ok := m["callback_data"].(string); ok {
				out = append(out, d)
			} else if u, ok := m["url"].(string); ok {
				out = append(out, "url:"+u)
			}
		}
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---------- harness ----------

func newTestGateway(t *testing.T) (*Gateway, *fakeTG, string) {
	t.Helper()
	f := newFakeTG(t)
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(work, "project"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.BotToken = "TESTTOKEN"
	cfg.ChatID = -100123
	cfg.AllowedUserIDs = []int64{42}
	cfg.DefaultCwd = work
	cfg.WorkspaceRoots = []string{work}
	cfg.StatePath = filepath.Join(dir, "state.json")
	cfg.APIBase = f.srv.URL
	cfg.EditIntervalMS = 400
	cfg.BridgeCmd = []string{"node", "testdata/fakebridge.mjs"}

	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gw := NewGateway(ctx, cfg, store)
	if err := gw.bridge.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !gw.bridge.Alive() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !gw.bridge.Alive() {
		t.Fatal("fake bridge did not start")
	}
	return gw, f, work
}

func msg(thread int, text string) TGUpdate {
	m := &TGMessage{
		MessageID: 1, From: &TGUser{ID: 42, FirstName: "u"},
		Chat: TGChat{ID: -100123, Type: "supergroup", IsForum: true},
		Text: text, MessageThreadID: thread, IsTopicMessage: thread > 0,
	}
	return TGUpdate{Message: m}
}

func press(thread, msgID int, data string) TGUpdate {
	return TGUpdate{CallbackQuery: &TGCallbackQuery{
		ID: "cb1", From: &TGUser{ID: 42},
		Data: data,
		Message: &TGMessage{
			MessageID: msgID, MessageThreadID: thread, IsTopicMessage: thread > 0,
			Chat: TGChat{ID: -100123, IsForum: true},
		},
	}}
}

// ---------- tests ----------

func TestNewSessionFlowIsAllButtons(t *testing.T) {
	gw, f, work := newTestGateway(t)

	gw.handleUpdate(msg(0, "/new"))
	c := f.waitFor(t, "sendMessage", "Which agent?", 3*time.Second)
	b := buttons(c)
	if !has(b, "new:agent:claude") || !has(b, "new:agent:codex") {
		t.Fatalf("agent picker should offer both agents as buttons, got %v", b)
	}
	menuID := 1001

	gw.handleUpdate(press(0, menuID, "new:agent:claude"))
	edit := f.waitFor(t, "editMessageText", "Folder", 3*time.Second)
	db := buttons(edit)
	if !has(db, "newdir:here") || !has(db, "newdir:type") {
		t.Fatalf("folder picker needs 'use this folder' and 'type a path' buttons, got %v", db)
	}
	if !has(db, "newdir:0") {
		t.Fatalf("folder picker should list sub-folders as buttons, got %v", db)
	}

	// "use this folder" now asks what the topic should be called.
	gw.handleUpdate(press(0, menuID, "newdir:here"))
	nameAsk := f.waitFor(t, "editMessageText", "What should this topic be called?", 3*time.Second)
	nb := buttons(nameAsk)
	if !has(nb, "newname:type") || !has(nb, "newname:auto") {
		t.Fatalf("the name step needs both buttons, got %v", nb)
	}
	gw.handleUpdate(press(0, menuID, "newname:auto"))
	f.waitFor(t, "createForumTopic", "", 3*time.Second)
	var sess *Session
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sess = gw.store.Get(555); sess != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sess == nil {
		t.Fatal("no session was stored for the new topic")
	}
	if sess.Agent != "claude" || sess.Cwd != work {
		t.Fatalf("session has wrong agent/cwd: %+v", sess)
	}
	// The topic gets a header message whose keyboard carries the settings.
	hdr := f.waitFor(t, "sendMessage", "Claude Code", 3*time.Second)
	hb := buttons(hdr)
	for _, want := range []string{"model", "cd", "agent", "clear", "verbose", "end"} {
		if !has(hb, want) {
			t.Errorf("session keyboard is missing the %q button (got %v)", want, hb)
		}
	}
}

func TestPromptStreamsAndFinishes(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 7, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(7, "say hello"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Hello world", 10*time.Second)
	sent := f.waitFor(t, "sendMessage", "Hello", 10*time.Second)
	if !has(buttons(sent), "stop") {
		t.Errorf("a running turn must offer a Stop button, got %v", buttons(sent))
	}
	// The turn is sealed with a footer, and the paragraph is ticked off.
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "1.2s", 10*time.Second)
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "1 step", 10*time.Second)
	last := f.findAny([]string{"editMessageText"}, "1.2s")
	txt, _ := last.Params["text"].(string)
	if !strings.Contains(txt, markDone+" Hello world") {
		t.Errorf("the finished paragraph should be ticked: %q", truncate(txt, 200))
	}
	// The finished message must not carry the command, its output or the tool.
	for _, leak := range []string{"echo hi", "Bash", "Running"} {
		if strings.Contains(txt, leak) {
			t.Errorf("the message leaks %q: %s", leak, truncate(txt, 200))
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(7); s != nil && s.Turns == 1 && s.Ref == "sess-7" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	s := gw.store.Get(7)
	t.Fatalf("session was not updated after the turn: %+v", s)
}

func TestStopButtonInterrupts(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 8, Agent: "codex", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(8, "SLOW please"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "thinking about it", 10*time.Second)

	gw.handleUpdate(press(8, 1002, "stop"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "stopped", 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		running := gw.running[8]
		gw.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the turn was still marked running after Stop")
}

// The picker shows the agent's own list, and choosing a model then offers
// that model's effort levels as a second screen of buttons.
// Every keyboard this bot sends must be at most two buttons wide: three
// across clips the labels on a phone.
func TestNoKeyboardIsWiderThanTwo(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 61, Agent: "codex", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	for _, cmd := range []string{"/help", "/status", "/config", "/cd", "/end", "/sessions", "/new"} {
		gw.handleUpdate(msg(61, cmd))
	}
	gw.handleUpdate(msg(61, "/model"))
	time.Sleep(1500 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := 0
	for _, c := range f.calls {
		rm, ok := c.Params["reply_markup"].(map[string]any)
		if !ok {
			continue
		}
		rows, _ := rm["inline_keyboard"].([]any)
		for _, row := range rows {
			seen++
			if n := len(row.([]any)); n > 2 {
				t.Errorf("%s sent a row of %d buttons", c.Method, n)
			}
		}
	}
	if seen < 8 {
		t.Fatalf("expected to inspect several keyboards, saw %d rows", seen)
	}
}

// A short message would squeeze the buttons, so it is padded out.
func TestShortMessagesArePaddedForButtons(t *testing.T) {
	gw, f, _ := newTestGateway(t)
	gw.handleUpdate(msg(0, "/new"))
	c := f.waitFor(t, "sendMessage", "Which agent?", 3*time.Second)
	txt, _ := c.Params["text"].(string)
	widest := 0
	for _, line := range strings.Split(txt, "\n") {
		if n := len([]rune(line)); n > widest {
			widest = n
		}
	}
	if widest < bubbleWidth {
		t.Fatalf("short button message was not widened: widest line %d runes", widest)
	}
	if padForButtons("a line that is definitely wider than the minimum bubble width") != "a line that is definitely wider than the minimum bubble width" {
		t.Error("a wide message should not be padded")
	}
}

// Kill is the hard stop, and it is offered next to Stop while a turn runs.
func TestKillButton(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 62, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(62, "SLOW please"))
	c := f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "thinking about it", 10*time.Second)
	b := buttons(c)
	if !has(b, "stop") || !has(b, "kill") {
		t.Fatalf("a running turn must offer Stop and Kill, got %v", b)
	}
	gw.handleUpdate(press(62, 1002, "kill"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "killed", 10*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		running := gw.running[62]
		gw.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the turn was still running after Kill")
}

func TestModelThenEffortPicker(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 9, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(9, "/model"))
	c := f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Model for", 10*time.Second)
	b := buttons(c)
	// three models from the fake agent, plus the "type a name" escape hatch
	if len(b) != 4 {
		t.Fatalf("expected a button per model, got %v", b)
	}
	menuID := 1001

	// Sonnet has effort levels, so the next screen must offer them.
	gw.handleUpdate(press(9, menuID, "mdl:1"))
	e := f.waitFor(t, "editMessageText", "How hard should it think?", 5*time.Second)
	eb := buttons(e)
	if !has(eb, "eff:0") || !has(eb, "eff:1") || !has(eb, "eff:none") {
		t.Fatalf("effort screen should list the model's levels, got %v", eb)
	}
	if s := gw.store.Get(9); s == nil || s.Model != "sonnet" {
		t.Fatalf("model was not applied: %+v", s)
	}
	gw.handleUpdate(press(9, menuID, "eff:1"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(9); s != nil && s.Effort == "high" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("effort was not applied: %+v", gw.store.Get(9))
}

// A model without effort levels must not leave a stale effort behind.
func TestPickingAModelWithoutEffortClearsIt(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 10, Agent: "claude", Cwd: work, Model: "sonnet", Effort: "high", Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(10, "/model"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Model for", 10*time.Second)
	gw.handleUpdate(press(10, 1001, "mdl:2")) // haiku, no efforts
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(10); s != nil && s.Model == "haiku" && s.Effort == "" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("effort should have been cleared: %+v", gw.store.Get(10))
}

// Codex and Claude must get their own lists.
func TestModelListIsPerAgent(t *testing.T) {
	gw, _, _ := newTestGateway(t)
	claude, err := gw.fetchModels("claude")
	if err != nil {
		t.Fatal(err)
	}
	codex, err := gw.fetchModels("codex")
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ms []ModelInfo) string {
		var out []string
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}
	if ids(claude) == ids(codex) {
		t.Fatalf("both agents returned the same list: %s", ids(claude))
	}
	if len(codex) < 2 || len(codex[1].Efforts) == 0 {
		t.Fatalf("codex models should carry effort levels, got %+v", codex)
	}
}

func TestGeneralNeverBecomesASession(t *testing.T) {
	gw, f, work := newTestGateway(t)
	// An imported session sitting in General must not swallow General's text.
	gw.store.Put(&Session{ThreadID: 0, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(0, "hello there"))
	c := f.waitFor(t, "sendMessage", "General", 3*time.Second)
	if !has(buttons(c), "new") {
		t.Fatalf("General should offer the controls, got %v", buttons(c))
	}
}

func TestStoreKeepsGeneralBoundSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Put(&Session{ThreadID: 0, Agent: "claude", Cwd: "/root", Ref: "abc"})
	st2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Get(0); got == nil || got.Ref != "abc" {
		t.Fatalf("a session bound to General must survive a reload, got %+v", got)
	}
}

// The Config button must expose the agent's own options, and applying one
// must reach the session state.
func TestConfigMenu(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 51, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(51, "/config"))
	c := f.waitFor(t, "sendMessage", "Claude Code config", 3*time.Second)
	b := buttons(c)
	for _, want := range []string{"cfg:perm", "cfg:think", "cfg:turns", "cfg:budget", "cfg:back"} {
		if !has(b, want) {
			t.Errorf("config menu is missing %q (got %v)", want, b)
		}
	}
	if has(b, "cfg:sandbox") {
		t.Error("codex-only options must not show for a Claude session")
	}

	gw.handleUpdate(press(51, 1001, "cfg:perm"))
	e := f.waitFor(t, "editMessageText", "Mode", 3*time.Second)
	if !has(buttons(e), "cf:perm:plan") {
		t.Fatalf("permission choices should be buttons, got %v", buttons(e))
	}
	gw.handleUpdate(press(51, 1001, "cf:perm:plan"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(51); s != nil && s.PermMode == "plan" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("permission mode was not applied: %+v", gw.store.Get(51))
}

// "Switch models when a message is flagged" offers the agent's own models,
// and picking one stores it as the fallback model.
func TestFlaggedFallbackOption(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 55, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(55, "/config"))
	c := f.waitFor(t, "sendMessage", "Claude Code config", 3*time.Second)
	if !has(buttons(c), "cfg:fallback") {
		t.Fatalf("config should offer the flagged-message fallback, got %v", buttons(c))
	}
	gw.handleUpdate(press(55, 1001, "cfg:fallback"))
	e := f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Switch model when flagged", 10*time.Second)
	b := buttons(e)
	if !has(b, "cf:fallback:-") || !has(b, "cf:fallback:sonnet") {
		t.Fatalf("fallback choices should be Off plus the agent's models, got %v", b)
	}
	gw.handleUpdate(press(55, 1001, "cf:fallback:sonnet"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(55); s != nil && s.Fallback == "sonnet" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("fallback model was not stored: %+v", gw.store.Get(55))
}

// Model buttons must not carry Claude's "(recommended)" noise.
func TestModelLabelsAreClean(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 56, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(56, "/model"))
	c := f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Model for", 10*time.Second)
	rm, _ := c.Params["reply_markup"].(map[string]any)
	rows, _ := rm["inline_keyboard"].([]any)
	for _, row := range rows {
		for _, b := range row.([]any) {
			text, _ := b.(map[string]any)["text"].(string)
			if strings.Contains(strings.ToLower(text), "recommend") {
				t.Errorf("model button still says %q", text)
			}
		}
	}
}

// /mode puts the permission modes straight on buttons, in the words the CLI
// uses for them.
func TestModeCommandShowsButtons(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 57, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(57, "/mode"))
	c := f.waitFor(t, "sendMessage", "Mode", 3*time.Second)
	b := buttons(c)
	for _, want := range []string{"cf:perm:acceptEdits", "cf:perm:plan", "cf:perm:dontAsk", "cf:perm:auto", "cf:perm:default"} {
		if !has(b, want) {
			t.Errorf("/mode is missing %q (got %v)", want, b)
		}
	}
	gw.handleUpdate(press(57, 1001, "cf:perm:acceptEdits"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(57); s != nil && s.PermMode == "acceptEdits" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("mode was not applied: %+v", gw.store.Get(57))
}

// Antigravity is a first-class agent: it can be chosen, named and configured.
func TestAntigravityIsAnAgent(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.handleUpdate(msg(0, "/new"))
	c := f.waitFor(t, "sendMessage", "Which agent?", 3*time.Second)
	if !has(buttons(c), "new:agent:antigravity") {
		t.Fatalf("the agent picker should offer Antigravity, got %v", buttons(c))
	}
	if got := topicTitle("antigravity", "VPN App"); got != "Antigravity • VPN App" {
		t.Errorf("topicTitle = %q", got)
	}
	if agentLabel("antigravity") != "Antigravity" {
		t.Errorf("agentLabel = %q", agentLabel("antigravity"))
	}
	gw.store.Put(&Session{ThreadID: 58, Agent: "antigravity", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(58, "/config"))
	cfgMsg := f.waitFor(t, "sendMessage", "Antigravity config", 3*time.Second)
	cb := buttons(cfgMsg)
	if !has(cb, "cfg:perm") {
		t.Errorf("antigravity config should offer the mode, got %v", cb)
	}
	if has(cb, "cfg:sandbox") || has(cb, "cfg:budget") {
		t.Errorf("antigravity config should not carry another agent's options: %v", cb)
	}
}

// Skills are listed on buttons and run by name.
func TestSkillsMenu(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 59, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(59, "/skills"))
	c := f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "claude-skill-0", 10*time.Second)
	b := buttons(c)
	if !has(b, "sk:0") {
		t.Fatalf("skills should be buttons, got %v", b)
	}
	if !has(b, "skp:1") {
		t.Errorf("a long list should paginate, got %v", b)
	}
	gw.handleUpdate(press(59, 1001, "sk:0"))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(59); s != nil && s.Turns == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the skill never ran")
}

func TestConfigMenuIsAgentSpecific(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 52, Agent: "codex", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(52, "/config"))
	c := f.waitFor(t, "sendMessage", "Codex config", 3*time.Second)
	b := buttons(c)
	for _, want := range []string{"cfg:sandbox", "cfg:approval", "cfg:web", "cfg:net"} {
		if !has(b, want) {
			t.Errorf("codex config is missing %q (got %v)", want, b)
		}
	}
	if has(b, "cfg:perm") {
		t.Error("claude-only options must not show for a Codex session")
	}
	gw.handleUpdate(press(52, 1001, "cf:sandbox:read-only"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(52); s != nil && s.Sandbox == "read-only" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("sandbox was not applied: %+v", gw.store.Get(52))
}

func TestUnknownTopicOffersAgentButtons(t *testing.T) {
	gw, f, _ := newTestGateway(t)
	gw.handleUpdate(msg(77, "hello?"))
	c := f.waitFor(t, "sendMessage", "Pick an agent", 3*time.Second)
	b := buttons(c)
	if !has(b, "bind:claude") || !has(b, "bind:codex") {
		t.Fatalf("an unbound topic should offer both agents, got %v", b)
	}
}

func TestStrangersAreIgnored(t *testing.T) {
	gw, f, _ := newTestGateway(t)
	before := f.count()
	u := msg(0, "/new")
	u.Message.From.ID = 999
	gw.handleUpdate(u)
	time.Sleep(200 * time.Millisecond)
	if got := f.since(before); len(got) != 0 {
		t.Fatalf("an unknown user must get no reply, got %+v", got)
	}
}

func TestEndSessionAsksThenCloses(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 11, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(11, "/end"))
	c := f.waitFor(t, "sendMessage", "End this session", 3*time.Second)
	b := buttons(c)
	if !has(b, "end:close") || !has(b, "end:delete") {
		t.Fatalf("ending must offer close and delete as buttons, got %v", b)
	}
	gw.handleUpdate(press(11, 1002, "end:close"))
	f.waitFor(t, "closeForumTopic", "", 3*time.Second)
	if gw.store.Get(11) != nil {
		t.Fatal("session should be gone after end")
	}
}

func TestDeleteTopicOption(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 14, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(14, "/end"))
	f.waitFor(t, "sendMessage", "End this session", 3*time.Second)
	gw.handleUpdate(press(14, 1002, "end:delete"))
	f.waitFor(t, "deleteForumTopic", "", 3*time.Second)
	if gw.store.Get(14) != nil {
		t.Fatal("session should be gone after delete")
	}
}

func TestNewWithATopicName(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.handleUpdate(msg(0, "/new My Project"))
	f.waitFor(t, "sendMessage", "Which agent?", 3*time.Second)
	gw.handleUpdate(press(0, 1001, "new:agent:codex"))
	f.waitFor(t, "editMessageText", "Folder", 3*time.Second)
	gw.handleUpdate(press(0, 1001, "newdir:here"))
	c := f.waitFor(t, "createForumTopic", "", 3*time.Second)
	if name, _ := c.Params["name"].(string); name != "Codex • My Project" {
		t.Fatalf("the topic should be named agent then name, got %q", name)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(555); s != nil {
			if s.Title != "Codex • My Project" || s.Name != "My Project" || s.Agent != "codex" || s.Cwd != work {
				t.Fatalf("session does not match the request: %+v", s)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session was not created")
}

// The pacing must widen as more topics stream, or concurrent sessions trip
// Telegram's group-wide flood limit together.
func TestEditIntervalWidensWithLoad(t *testing.T) {
	gw, _, _ := newTestGateway(t)
	base := time.Duration(gw.cfg.EditIntervalMS) * time.Millisecond
	if got := gw.editInterval(); got != base {
		t.Errorf("idle interval = %s, want %s", got, base)
	}
	gw.mu.Lock()
	gw.running[1], gw.running[2], gw.running[3] = true, true, true
	gw.mu.Unlock()
	if got := gw.editInterval(); got != 3*base {
		t.Errorf("interval with three live turns = %s, want %s", got, 3*base)
	}
	gw.mu.Lock()
	for i := 4; i < 40; i++ {
		gw.running[i] = true
	}
	gw.mu.Unlock()
	if got := gw.editInterval(); got != 12*time.Second {
		t.Errorf("interval should cap at 12s, got %s", got)
	}
}

// A turn that is slow to finish must not tear down the next turn's event
// subscription: that used to leave the follow-up message waiting forever.
func TestSubscriptionSurvivesAnOverlappingTurn(t *testing.T) {
	b := NewBridge([]string{"node", "testdata/fakebridge.mjs"})
	first := b.Subscribe("7")
	second := b.Subscribe("7") // the next turn takes over
	b.Unsubscribe("7", first)  // the old turn finishes late
	b.dispatch(Event{Type: "done", SID: "7"})
	select {
	case ev := <-second:
		if ev.Type != "done" {
			t.Fatalf("got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the live subscription lost its event")
	}
}

// A topic is always "<Agent> • <what you called it>", and the name survives a
// rename or a switch of agent.
func TestTopicTitles(t *testing.T) {
	cases := []struct{ agent, name, want string }{
		{"claude", "VPN App", "Claude • VPN App"},
		{"codex", "VPN App", "Codex • VPN App"},
		{"claude", "", "Claude"},
		{"claude", "Claude • VPN App", "Claude • VPN App"}, // no stacking
		{"codex", "Claude • VPN App", "Codex • VPN App"},   // switching agent
		{"claude", "Claude Code · agentchat", "Claude • agentchat"},
	}
	for _, c := range cases {
		if got := topicTitle(c.agent, c.name); got != c.want {
			t.Errorf("topicTitle(%q, %q) = %q, want %q", c.agent, c.name, got, c.want)
		}
	}
}

func TestSwitchingAgentKeepsTheName(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 81, Agent: "claude", Name: "VPN App", Title: "Claude • VPN App",
		Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(press(81, 1001, "agt:codex"))
	c := f.waitFor(t, "editForumTopic", "", 3*time.Second)
	if name, _ := c.Params["name"].(string); name != "Codex • VPN App" {
		t.Fatalf("switching agent should keep the name, got %q", name)
	}
	if s := gw.store.Get(81); s == nil || s.Title != "Codex • VPN App" || s.Name != "VPN App" {
		t.Fatalf("stored title is wrong: %+v", gw.store.Get(81))
	}
}

func TestRenameKeepsTheAgentPrefix(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 82, Agent: "codex", Name: "old", Title: "Codex • old",
		Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.handleUpdate(msg(82, "/rename VPN App"))
	c := f.waitFor(t, "editForumTopic", "", 3*time.Second)
	if name, _ := c.Params["name"].(string); name != "Codex • VPN App" {
		t.Fatalf("rename should keep the agent in front, got %q", name)
	}
	if s := gw.store.Get(82); s == nil || s.Name != "VPN App" || s.AutoName {
		t.Fatalf("rename should store the bare name: %+v", gw.store.Get(82))
	}
}

// Creating a session asks for a name, and a typed name lands on the topic
// behind the agent.
func TestSessionCreationAsksForAName(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.handleUpdate(msg(0, "/new claude "+work))
	c := f.waitFor(t, "sendMessage", "What should this topic be called?", 3*time.Second)
	if !has(buttons(c), "newname:type") {
		t.Fatalf("expected the name question, got %v", buttons(c))
	}
	gw.handleUpdate(press(0, 1001, "newname:type"))
	f.waitFor(t, "editMessageText", "Send the name", 3*time.Second)
	gw.handleUpdate(msg(0, "VPN App"))
	created := f.waitFor(t, "createForumTopic", "", 5*time.Second)
	if name, _ := created.Params["name"].(string); name != "Claude • VPN App" {
		t.Fatalf("topic name = %q, want %q", name, "Claude • VPN App")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(555); s != nil && s.Name == "VPN App" && !s.AutoName {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("session name was not stored: %+v", gw.store.Get(555))
}

func TestParseNewArgs(t *testing.T) {
	cases := []struct{ in, agent, path, name string }{
		{"", "", "", ""},
		{"codex", "codex", "", ""},
		{"/root/x", "", "/root/x", ""},
		{"My Project", "", "", "My Project"},
		{"codex /root/x My Project", "codex", "/root/x", "My Project"},
		{"claude Soren VPN", "claude", "", "Soren VPN"},
	}
	for _, c := range cases {
		a, p, n := parseNewArgs(c.in)
		if a != c.agent || p != c.path || n != c.name {
			t.Errorf("parseNewArgs(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, a, p, n, c.agent, c.path, c.name)
		}
	}
}

// A session whose topic is gone (or that came from a gateway which ran in
// General) must get a topic of its own when the gateway starts.
func TestReconcileGivesEverySessionATopic(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 0, Agent: "claude", Cwd: work, Title: "Imported", Ref: "old-session", Created: time.Now(), LastUsed: time.Now()})
	gw.reconcileTopics()
	c := f.waitFor(t, "createForumTopic", "", 3*time.Second)
	if name, _ := c.Params["name"].(string); name != "Claude • Imported" {
		t.Errorf("a recreated topic should keep its name under the agent, got %q", name)
	}
	if gw.store.Get(0) != nil {
		t.Error("the old thread id should not stay in the store")
	}
	s := gw.store.Get(555)
	if s == nil {
		t.Fatal("session was not moved to the new topic")
	}
	if s.Ref != "old-session" {
		t.Errorf("resume id must survive the move, got %q", s.Ref)
	}
}

// Two topics must be able to work at the same time.
func TestConcurrentTopics(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 21, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})
	gw.store.Put(&Session{ThreadID: 22, Agent: "codex", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(21, "SLOW one"))
	gw.handleUpdate(msg(22, "say hello"))

	// The second topic finishes while the first is still running.
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Hello world", 10*time.Second)
	gw.mu.Lock()
	stillRunning := gw.running[21]
	gw.mu.Unlock()
	if !stillRunning {
		t.Fatal("the slow topic should still be running while the other finished")
	}
	gw.handleUpdate(press(21, 1003, "stop"))
	f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "stopped", 10*time.Second)
}

func TestCdBrowsesWithButtons(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 12, Agent: "claude", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	gw.handleUpdate(msg(12, "/cd"))
	c := f.waitFor(t, "sendMessage", "Working folder", 3*time.Second)
	if !has(buttons(c), "cd:here") {
		t.Fatalf("cd picker needs buttons, got %v", buttons(c))
	}
	// pick the first sub-folder, then confirm it
	gw.handleUpdate(press(12, 1001, "cd:0"))
	f.waitFor(t, "editMessageText", "project", 3*time.Second)
	gw.handleUpdate(press(12, 1001, "cd:here"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(12); s != nil && s.Cwd == filepath.Join(work, "project") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("cwd not changed: %+v", gw.store.Get(12))
}

// A photo with a caption is saved into the session's folder and the caption
// becomes the prompt, with the image handed to the agent.
func TestPhotoWithCaptionReachesTheAgent(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 13, Agent: "codex", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	u := msg(13, "")
	u.Message.Photo = []TGPhotoSize{{FileID: "small", Width: 90}, {FileID: "big", Width: 1280}}
	u.Message.Caption = "make the button in this screenshot green"
	gw.handleUpdate(u)

	f.waitFor(t, "sendMessage", "photo_1.jpg", 5*time.Second)
	saved := filepath.Join(work, "photo_1.jpg")
	if _, err := os.Stat(saved); err != nil {
		t.Fatalf("the photo was not saved: %v", err)
	}
	// The turn runs, which means the caption went to the agent.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(13); s != nil && s.Turns == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the caption never reached the agent")
}

// Several photos sent as one album become one message to the agent.
func TestPhotoAlbumIsSentOnce(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.store.Put(&Session{ThreadID: 15, Agent: "codex", Cwd: work, Created: time.Now(), LastUsed: time.Now()})

	for i, caption := range []string{"compare these two", ""} {
		u := msg(15, "")
		u.Message.MessageID = 40 + i
		u.Message.Photo = []TGPhotoSize{{FileID: fmt.Sprintf("f%d", i), Width: 1280}}
		u.Message.Caption = caption
		u.Message.MediaGroupID = "album-1"
		gw.handleUpdate(u)
	}
	f.waitFor(t, "sendMessage", "saved 2 files", 6*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := gw.store.Get(15); s != nil && s.Turns == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("an album should produce exactly one turn, got %+v", gw.store.Get(15))
}

// Images go to the agent as images; other files are only named.
func TestImageDetection(t *testing.T) {
	for _, p := range []string{"/a/b.png", "/a/b.JPG", "/x/y.jpeg", "/x/y.webp", "/x/y.HEIC"} {
		if !isImage(p) {
			t.Errorf("%s should count as an image", p)
		}
	}
	for _, p := range []string{"/a/b.zip", "/a/b.txt", "/a/b.apk", "/a/b"} {
		if isImage(p) {
			t.Errorf("%s should not count as an image", p)
		}
	}
}
func TestMarkdownToHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a <b> & c", "a &lt;b&gt; &amp; c"},
		{"**bold**", "<b>bold</b>"},
		{"`code`", "<code>code</code>"},
		{"```go\nfmt.Println(1 < 2)\n```", "<pre><code class=\"language-go\">fmt.Println(1 &lt; 2)</code></pre>"},
		{"# Title", "<b>Title</b>"},
		{"[x](https://e.com)", `<a href="https://e.com">x</a>`},
		{"- item", "• item"},
	}
	for _, c := range cases {
		if got := mdToHTML(c.in); got != c.want {
			t.Errorf("mdToHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A tag inside a code fence must not be interpreted.
	got := mdToHTML("```\n<script>alert(1)</script>\n```")
	if strings.Contains(got, "<script>") {
		t.Errorf("code fence leaked a tag: %q", got)
	}
}

func TestSplitHTMLKeepsTagsBalanced(t *testing.T) {
	long := "<pre>" + strings.Repeat("line of code\n", 500) + "</pre>"
	head, tail := splitHTML(long, 300)
	if len([]rune(head)) > 340 {
		t.Fatalf("head too long: %d", len([]rune(head)))
	}
	if strings.Count(head, "<pre>") != strings.Count(head, "</pre>") {
		t.Errorf("unbalanced pre in head: %q", truncate(head, 120))
	}
	if !strings.HasPrefix(tail, "<pre>") {
		t.Errorf("tail should reopen the code block, got %q", truncate(tail, 80))
	}
}

func TestCommandParsing(t *testing.T) {
	cases := []struct{ in, cmd, arg string }{
		{"/new", "new", ""},
		{"/new claude /root/x", "new", "claude /root/x"},
		{"/model@testbot opus", "model", "opus"},
		{"/RUN ls -la", "run", "ls -la"},
	}
	for _, c := range cases {
		cmd, arg := splitCommand(c.in)
		if cmd != c.cmd || arg != c.arg {
			t.Errorf("splitCommand(%q) = (%q,%q), want (%q,%q)", c.in, cmd, arg, c.cmd, c.arg)
		}
	}
}

func TestPathAllowed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkspaceRoots = []string{"/root"}
	if !cfg.PathAllowed("/root/x/y") {
		t.Error("path under the root should be allowed")
	}
	if cfg.PathAllowed("/etc/passwd") {
		t.Error("path outside the roots must be refused")
	}
}

// ---------- step phrasing ----------

// The whole point: a Telegram reader sees "Installing dependencies", never
// /bin/bash -lc "npm install --no-fund 2>&1 | tail -20".
func TestDescribeStep(t *testing.T) {
	cases := []struct {
		tool, detail, want string
	}{
		{"Bash", `/bin/bash -lc "npm install --no-fund"`, "Installing dependencies"},
		{"Bash", `/bin/bash -lc 'pnpm add react'`, "Installing dependencies"},
		{"Bash", `/bin/bash -lc "go test ./... 2>&1 | tail -40"`, "Running tests"},
		{"Bash", `bash -lc "npm run build"`, "Building"},
		{"Bash", `/bin/bash -lc 'git commit -m "wip"'`, "Committing changes"},
		{"Bash", `/bin/bash -lc "git push origin main"`, "Syncing with git"},
		{"Bash", `/bin/bash -lc "ls -ld /root/calculator 2>/dev/null; cat /root/x"`, "Looking around"},
		{"Bash", `/bin/bash -lc "rg --files -g 'AGENTS.md' /root"`, "Searching the project"},
		{"Bash", `/bin/bash -lc "cat /root/.config/codex-delivery/send.py"`, "Reading files"},
		{"Bash", `/bin/bash -lc "mkdir -p /root/calculator/js"`, "Setting up files"},
		{"Bash", `/bin/bash -lc "curl -s http://127.0.0.1:8088/health"`, "Checking the network"},
		{"Bash", `/bin/bash -lc "node --version && npm --version"`, "Running a script"},
		{"Bash", `/bin/bash -lc "systemctl restart nginx"`, "Managing services"},
		{"Bash", `/bin/bash -lc "zip -r out.zip src"`, "Packaging files"},
		{"Bash", `/bin/bash -lc "python3 -m pytest -q"`, "Running tests"},
		{"Bash", `/bin/bash -lc "ss -ltnp"`, "Checking processes"},
		{"Bash", `/bin/bash -lc "gofmt -w ."`, "Tidying the code"},
		{"Read", "/root/calculator/js/app.js", "Reading js/app.js"},
		{"Write", "/root/calculator/index.html", "Writing calculator/index.html"},
		{"Edit", "/root/tg-agent-gateway/render.go", "Editing tg-agent-gateway/render.go"},
		{"Grep", "func main", "Searching the project"},
		{"WebFetch", "https://example.com/docs/page?x=1", "Reading example.com"},
		{"WebSearch", "golang telegram", "Searching the web"},
		{"Task", "review the diff", "Asking a subagent"},
		{"TodoWrite", "4 items", "Planning the work"},
	}
	for _, c := range cases {
		icon, phrase := describeStep(c.tool, c.detail)
		if phrase != c.want {
			t.Errorf("describeStep(%s, %q) = %q, want %q", c.tool, truncate(c.detail, 40), phrase, c.want)
		}
		if icon == "" {
			t.Errorf("describeStep(%s) has no icon", c.tool)
		}
	}
}

func TestCleanCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{`/bin/bash -lc "echo hi"`, "echo hi"},
		{`/bin/bash -lc 'ls -la'`, "ls -la"},
		{"bash -c \"go   test  ./...\"", "go test ./..."},
		{`sh -c 'printf "a b"'`, `printf "a b"`},
		{"plain command", "plain command"},
	}
	for _, c := range cases {
		if got := cleanCommand(c.in); got != c.want {
			t.Errorf("cleanCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := firstCommand("FOO=1 npm install && npm test"); got != "npm install" {
		t.Errorf("firstCommand = %q", got)
	}
}

// The checklist is the agent's own narration: what it said it would do,
// ticked off as it moves on, with the newest paragraph marked as current.
func TestNarrationChecklist(t *testing.T) {
	gw, _, work := newTestGateway(t)
	sess := &Session{ThreadID: 71, Agent: "codex", Cwd: work}
	turn := gw.NewTurn(sess)

	turn.AddText("I'll set the project up and wire the pieces together.")
	turn.SetStep("start", "Bash", `/bin/bash -lc "mkdir -p site"`)
	turn.SetStep("ok", "Bash", "")
	turn.AddText("The layout is in place. Adding the parts it depends on.")
	turn.SetStep("start", "Bash", `/bin/bash -lc "npm install"`)
	turn.SetStep("ok", "Bash", "added 120 packages")
	turn.AddText("Everything is built. I'm checking it works now.")

	body := turn.render()
	if strings.Count(body, markDone) != 2 {
		t.Errorf("finished paragraphs should be ticked: %q", body)
	}
	if strings.Count(body, markRunning) != 1 || !strings.Contains(body, markRunning+" Everything is built") {
		t.Errorf("the newest paragraph should be the current one: %q", body)
	}
	// No command, no tool name, no log.
	for _, leak := range []string{"npm install", "mkdir", "Bash", "added 120"} {
		if strings.Contains(body, leak) {
			t.Errorf("the message leaks %q: %s", leak, body)
		}
	}
	// Finishing marks everything done.
	turn.Finish("")
	if strings.Contains(turn.render(), markRunning) {
		t.Errorf("a finished turn has nothing in progress: %q", turn.render())
	}
	if strings.Count(turn.render(), markDone) != 3 {
		t.Errorf("every paragraph should be ticked at the end: %q", turn.render())
	}
}

// A plain answer is a plain answer: no checklist for a question that needed
// no work.
func TestSingleAnswerHasNoMarks(t *testing.T) {
	gw, _, work := newTestGateway(t)
	turn := gw.NewTurn(&Session{ThreadID: 73, Agent: "claude", Cwd: work})
	turn.AddText("It is at /etc/nginx/nginx.conf.")
	body := turn.render()
	if strings.Contains(body, markDone) || strings.Contains(body, markRunning) {
		t.Fatalf("a one-paragraph answer needs no marks: %q", body)
	}
}

// Details on: the tool list is available, folded away.
func TestDetailsShowTheToolList(t *testing.T) {
	gw, _, work := newTestGateway(t)
	turn := gw.NewTurn(&Session{ThreadID: 74, Agent: "claude", Cwd: work, Verbose: true})
	turn.AddText("Setting things up.")
	turn.SetStep("start", "Bash", `/bin/bash -lc "npm install"`)
	turn.SetStep("ok", "Bash", "")
	body := turn.render()
	if !strings.Contains(body, "blockquote expandable") || !strings.Contains(body, "Installing dependencies") {
		t.Fatalf("Details should list the steps behind a fold: %q", body)
	}
}

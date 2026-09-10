package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// text is the message body of a recorded call.
func text(c *sentCall) string {
	s, _ := c.Params["text"].(string)
	return s
}

// ---------- keeping a session when its topic goes ----------

func TestDeleteTopicKeepsTheSession(t *testing.T) {
	gw, f, work := newTestGateway(t)

	sess, err := gw.createSession("claude", work, "Kept One", 0)
	if err != nil {
		t.Fatal(err)
	}
	gw.store.Update(sess.ThreadID, func(s *Session) { s.Ref = "conv-abc" })
	thread := sess.ThreadID

	gw.handleUpdate(press(thread, 1, "end"))
	c := f.waitFor(t, "sendMessage", "End this session?", 3*time.Second)
	if b := buttons(c); !has(b, "end:keep") {
		t.Fatalf("ending a session should offer to keep it, got %v", b)
	}

	gw.handleUpdate(press(thread, 1, "end:keep"))
	f.waitFor(t, "deleteForumTopic", "", 3*time.Second)

	if gw.store.Get(thread) != nil {
		t.Fatal("the topic should be gone from the live sessions")
	}
	kept := gw.store.Kept()
	if len(kept) != 1 || kept[0].Ref != "conv-abc" || kept[0].Name != "Kept One" {
		t.Fatalf("the session should be kept with its conversation, got %+v", kept)
	}

	// Startup must not give a kept session a topic again: it was deleted on
	// purpose, which is the whole difference from a topic that went missing.
	before := f.count()
	gw.reconcileTopics()
	for _, call := range f.since(before) {
		if call.Method == "createForumTopic" {
			t.Fatal("a kept session must not be given a topic at startup")
		}
	}
}

func TestKeptSessionSurvivesARestart(t *testing.T) {
	gw, _, work := newTestGateway(t)
	sess, err := gw.createSession("codex", work, "Later", 0)
	if err != nil {
		t.Fatal(err)
	}
	gw.store.Update(sess.ThreadID, func(s *Session) { s.Ref = "codex-conv" })
	gw.store.Detach(sess.ThreadID)

	reopened, err := OpenStore(gw.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	kept := reopened.Kept()
	if len(kept) != 1 || kept[0].Ref != "codex-conv" {
		t.Fatalf("the archive should be on disk, got %+v", kept)
	}
	if len(reopened.List()) != 0 {
		t.Fatal("a kept session is not a live topic")
	}
}

// ---------- the session browser ----------

func TestSessionBrowserResumesAConversation(t *testing.T) {
	gw, f, _ := newTestGateway(t)

	gw.handleUpdate(msg(0, "/sessions"))
	c := f.waitFor(t, "sendMessage", "Pick an agent", 3*time.Second)
	if b := buttons(c); !has(b, "sessions:claude") {
		t.Fatalf("the browser should offer each agent, got %v", b)
	}

	gw.handleUpdate(press(0, 1001, "sessions:claude"))
	list := f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "last 10 conversations", 5*time.Second)
	if b := buttons(list); !has(b, "past:0") {
		t.Fatalf("the browser should list the conversations, got %v", b)
	}

	gw.handleUpdate(press(0, 1001, "past:0"))
	summary := f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "claude-conv-0", 3*time.Second)
	b := buttons(summary)
	if !has(b, "past:go:0") {
		t.Fatalf("the summary should offer to resume, got %v", b)
	}
	if !strings.Contains(text(summary), "something claude was asked to do") {
		t.Fatalf("the summary should say what the conversation was about: %s", text(summary))
	}

	before := f.count()
	gw.handleUpdate(press(0, 1001, "past:go:0"))
	f.waitFor(t, "createForumTopic", "", 3*time.Second)
	_ = before

	var found *Session
	for _, s := range gw.store.List() {
		if s.Ref == "claude-conv-0" {
			found = s
		}
	}
	if found == nil {
		t.Fatal("resuming should leave a session pointed at that conversation")
	}
}

// ---------- isolated sessions ----------

func requireSandbox(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed")
	}
	if sandboxRunPath() == "" {
		t.Skip("the sandbox helper is not where the tests can find it")
	}
}

func TestIsolatedSessionIsMadeForOnePerson(t *testing.T) {
	requireSandbox(t)
	gw, f, _ := newTestGateway(t)
	gw.cfg.IsolatedRoot = filepath.Join(t.TempDir(), "isolated")

	// The folder question offers a sandbox instead of a folder.
	gw.handleUpdate(msg(0, "/new claude"))
	dirs := f.waitFor(t, "sendMessage", "Folder for the new session", 3*time.Second)
	if b := buttons(dirs); !has(b, "iso:start") {
		t.Fatalf("the folder picker should offer an isolated session, got %v", b)
	}

	gw.handleUpdate(press(0, 1001, "iso:start"))
	who := f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "Who is it for?", 3*time.Second)
	if b := buttons(who); !has(b, "iso:guest") || !has(b, "iso:mine") {
		t.Fatalf("it should ask whether the sandbox is for somebody else, got %v", b)
	}

	gw.handleUpdate(press(0, 1001, "iso:guest"))
	f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "Telegram user id", 3*time.Second)

	gw.handleUpdate(msg(0, "777"))
	ask := f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "What should this topic be called", 3*time.Second)

	gw.handleUpdate(press(0, ask.ID, "newname:type"))
	f.waitForAny(t, []string{"editMessageText", "sendMessage"}, "Send the name for this topic", 3*time.Second)

	gw.handleUpdate(msg(0, "Guest Work"))
	f.waitFor(t, "createForumTopic", "", 3*time.Second)

	var iso *Session
	for _, s := range gw.store.List() {
		if s.Isolated {
			iso = s
		}
	}
	if iso == nil {
		t.Fatal("an isolated session should have been created")
	}
	if iso.OwnerID != 777 {
		t.Fatalf("the session should belong to the guest, got owner %d", iso.OwnerID)
	}
	if !strings.HasPrefix(iso.Cwd, filepath.Join(gw.cfg.IsolatedRoot, "Claude")) {
		t.Fatalf("an isolated session works under the isolated root, got %s", iso.Cwd)
	}
	if st, err := os.Stat(iso.Cwd); err != nil || !st.IsDir() {
		t.Fatalf("the sandbox folder should exist: %v", err)
	}
	if st, err := os.Stat(filepath.Join(iso.Cwd, "outbox")); err != nil || !st.IsDir() {
		t.Fatalf("the sandbox should have an outbox to deliver through: %v", err)
	}
}

func TestOwnedSessionAnswersOnlyItsOwner(t *testing.T) {
	gw, _, work := newTestGateway(t)
	sess, err := gw.createSession("claude", work, "Guest", 0)
	if err != nil {
		t.Fatal(err)
	}
	gw.store.Update(sess.ThreadID, func(s *Session) { s.OwnerID = 777 })
	thread := sess.ThreadID

	owner := &TGUser{ID: 777}
	admin := &TGUser{ID: 42}

	if !gw.authorised(owner, gw.cfg.ChatID, thread) {
		t.Fatal("the person the session was made for must be able to use it")
	}
	if gw.authorised(admin, gw.cfg.ChatID, thread) {
		t.Fatal("an owned session is that person's alone, admins included")
	}
	// Everywhere else, the guest is nobody.
	if gw.authorised(owner, gw.cfg.ChatID, 0) {
		t.Fatal("a guest must not be answered in General")
	}
	if !gw.authorised(admin, gw.cfg.ChatID, 0) {
		t.Fatal("an admin still runs the gateway")
	}
	if gw.mayList(777) {
		t.Fatal("a guest must not be able to browse the sessions")
	}
	if !gw.mayList(42) {
		t.Fatal("an admin browses the sessions")
	}
}

func TestGuestsCannotRunTheGateway(t *testing.T) {
	gw, f, work := newTestGateway(t)
	gw.cfg.AdminUserIDs = []int64{42}
	sess, err := gw.createSession("claude", work, "Guest", 0)
	if err != nil {
		t.Fatal(err)
	}
	gw.store.Update(sess.ThreadID, func(s *Session) { s.OwnerID = 777 })

	guest := TGUpdate{Message: &TGMessage{
		MessageID: 9, From: &TGUser{ID: 777},
		Chat: TGChat{ID: -100123, Type: "supergroup", IsForum: true},
		Text: "/sessions", MessageThreadID: sess.ThreadID, IsTopicMessage: true,
	}}
	before := f.count()
	gw.handleUpdate(guest)
	f.waitFor(t, "sendMessage", "Only an administrator", 3*time.Second)
	for _, call := range f.since(before) {
		if call.Method == "createForumTopic" {
			t.Fatal("a guest must not be able to start sessions")
		}
	}
}

// ---------- the boundary itself ----------

func TestIsolatedPathsCannotLeaveTheirFolder(t *testing.T) {
	root := t.TempDir()
	sess := &Session{Isolated: true, Cwd: root, Root: root}

	if got := guestPath(sess, filepath.Join(root, "src", "main.go")); got != "/workspace/src/main.go" {
		t.Fatalf("the agent should see its folder as /workspace, got %s", got)
	}
	if got := guestPath(sess, root); got != guestWork {
		t.Fatalf("the folder itself is /workspace, got %s", got)
	}

	inside, err := hostPath(sess, root, "/workspace/notes.txt")
	if err != nil || inside != filepath.Join(root, "notes.txt") {
		t.Fatalf("a path inside should map back to the folder, got %q %v", inside, err)
	}
	for _, bad := range []string{"/etc/passwd", "../..", "/root/.claude/.credentials.json", root + "/../elsewhere"} {
		if p, err := hostPath(sess, root, bad); err == nil {
			t.Fatalf("%q should not be reachable from an isolated session, got %s", bad, p)
		}
	}

	// A symlink planted inside must not become a way out.
	link := filepath.Join(root, "escape")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skip("symlinks are not available here")
	}
	if p, err := hostPath(sess, root, "escape/passwd"); err == nil {
		t.Fatalf("a symlink out of the folder should be refused, got %s", p)
	}
}

func TestIsolatedSessionsKeepTheTokenOutside(t *testing.T) {
	gw, _, work := newTestGateway(t)
	plain, err := gw.createSession("claude", work, "Normal", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c := gw.command("prompt", plain, "hello"); c.TGToken == "" {
		t.Fatal("an ordinary session delivers with tg-send and needs the token")
	}

	iso := &Session{ThreadID: 4242, Agent: "claude", Isolated: true, Cwd: work, Root: work}
	c := gw.command("prompt", iso, "hello")
	if c.TGToken != "" || c.TGChat != "" {
		t.Fatal("an isolated session must never be handed the bot token")
	}
	if c.SandboxDir != work {
		t.Fatalf("the sandbox should be built around the session folder, got %q", c.SandboxDir)
	}
	if c.Cwd != guestWork {
		t.Fatalf("the agent works in /workspace, got %q", c.Cwd)
	}
}

func TestOutboxDeliversIntoTheTopic(t *testing.T) {
	gw, f, _ := newTestGateway(t)
	dir := t.TempDir()
	sess := &Session{ThreadID: 4242, Agent: "claude", Isolated: true, Cwd: dir, Root: dir}

	out := filepath.Join(dir, "outbox")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "build.zip"), []byte("PRETEND-ZIP"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "build.zip.caption"), []byte("the finished build"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Files are left alone until they stop being written to.
	old := time.Now().Add(-5 * time.Second)
	_ = os.Chtimes(filepath.Join(out, "build.zip"), old, old)

	gw.drainOutbox(sess)
	if c := f.find("sendDocument", ""); c == nil {
		t.Fatal("what the sandbox left in its outbox should reach the topic")
	}
	if _, err := os.Stat(filepath.Join(out, "build.zip")); !os.IsNotExist(err) {
		t.Fatal("a delivered file should not be sent a second time")
	}
	if _, err := os.Stat(filepath.Join(out, ".sent", "build.zip")); err != nil {
		t.Fatalf("a delivered file should be kept aside, not thrown away: %v", err)
	}
}

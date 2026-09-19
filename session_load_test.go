package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const loadUUID = "abcdefab-1234-5678-90ab-000000000001"

func enableLoadAgents(gw *Gateway) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.agentsAt = time.Now()
	gw.agentsOK = map[string]bool{"claude": true, "codex": true, "antigravity": true}
}

func awaitLoadName(t *testing.T, gw *Gateway, f *fakeTG, thread int, command string) *sentCall {
	t.Helper()
	gw.handleUpdate(msg(thread, command))
	return f.waitFor(t, "sendMessage", "What should the topic be called?", 3*time.Second)
}

func TestUUIDLoadAsksNameThenResumesEachAgent(t *testing.T) {
	for _, command := range []string{"claude", "codex", "agy"} {
		for _, thread := range []int{0, 111} {
			t.Run(command+"/"+itoa(thread), func(t *testing.T) {
				gw, f, work := newTestGateway(t)
				enableLoadAgents(gw)
				gw.store.Put(&Session{ThreadID: 111, Agent: "codex", Ref: "other-conversation", Cwd: work, Model: "old-model"})
				gw.running[111] = true
				awaitLoadName(t, gw, f, thread, "/"+command+"@testbot "+strings.ToUpper(loadUUID))
				if f.find("createForumTopic", "") != nil {
					t.Fatal("created a topic before receiving the name")
				}
				gw.handleUpdate(msg(thread, "My Loaded Project"))
				f.waitFor(t, "sendMessage", "resumed", 3*time.Second)
				s := gw.store.Get(555)
				agent := command
				if agent == "agy" {
					agent = "antigravity"
				}
				if s == nil || s.Agent != agent || s.Ref != loadUUID || s.Name != "My Loaded Project" || s.Cwd != work || s.Model != "" {
					t.Fatalf("wrong resumed session: %+v", s)
				}
				if old := gw.store.Get(111); old.Ref != "other-conversation" || old.Model != "old-model" {
					t.Fatalf("changed the calling topic: %+v", old)
				}
				gw.handleUpdate(msg(555, "VERIFY-RESUME"))
				call := f.waitForAny(t, []string{"sendMessage", "editMessageText"}, "Resume received:", 3*time.Second)
				text, _ := call.Params["text"].(string)
				if !strings.Contains(text, loadUUID) || !strings.Contains(text, agent) || !strings.Contains(text, work) {
					t.Fatalf("wrong bridge resume routing: %s", text)
				}
			})
		}
	}
}

func TestUUIDLoadReusesBusyTopicAndScopesAgent(t *testing.T) {
	gw, f, work := newTestGateway(t)
	enableLoadAgents(gw)
	for _, s := range []*Session{
		{ThreadID: 111, Agent: "claude", Ref: loadUUID, Cwd: work, Name: "Wrong agent"},
		{ThreadID: 222, Agent: "codex", Ref: loadUUID, Cwd: work, Name: "Before", Model: "keep-model", OwnerID: 77, Isolated: true, Root: work},
	} {
		gw.store.Put(s)
	}
	gw.running[222] = true
	awaitLoadName(t, gw, f, 0, "/codex "+loadUUID)
	gw.handleUpdate(msg(0, "Chosen Name"))
	c := f.waitFor(t, "sendMessage", "Session ready", 3*time.Second)
	if !has(buttons(c), "url:"+TopicLink(gw.cfg.ChatID, 222)) || f.find("createForumTopic", "") != nil {
		t.Fatal("did not reuse the existing topic")
	}
	s := gw.store.Get(222)
	if s.Name != "Chosen Name" || s.Model != "keep-model" || s.OwnerID != 77 || !s.Isolated || s.Root != work || s.Ref != loadUUID {
		t.Fatalf("existing settings were lost: %+v", s)
	}
	if gw.store.Get(111).Name != "Wrong agent" {
		t.Fatal("renamed the wrong agent's topic")
	}
}

func TestUUIDLoadKeptSandboxKeepsOwnerSettingsAndRoot(t *testing.T) {
	gw, f, work := newTestGateway(t)
	enableLoadAgents(gw)
	gw.cfg.IsolatedRoot = filepath.Join(work, "isolated")
	root := filepath.Join(gw.cfg.IsolatedRoot, "Codex", "saved")
	cwd := filepath.Join(root, "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	gw.store.Put(&Session{ThreadID: 222, Agent: "codex", Ref: loadUUID, Cwd: cwd, Root: root,
		Name: "Before", AutoName: true, Isolated: true, OwnerID: 77, Model: "saved-model", Effort: "high", Fast: "on"})
	gw.store.Detach(222)
	awaitLoadName(t, gw, f, 0, "/codex "+loadUUID)
	gw.handleUpdate(msg(0, "Restored Name"))
	f.waitFor(t, "sendMessage", "resumed", 3*time.Second)
	s := gw.store.Get(555)
	if s == nil || s.Name != "Restored Name" || s.AutoName || s.Root != root || s.Cwd != cwd || !s.Isolated || s.OwnerID != 77 || s.Model != "saved-model" || s.Effort != "high" || s.Fast != "on" {
		t.Fatalf("archive restoration lost metadata: %+v", s)
	}
	if len(gw.store.Kept()) != 0 {
		t.Fatal("restored archive was not consumed")
	}
	c := gw.command("prompt", s, "hello")
	if c.Resume != loadUUID || c.SandboxDir != root || c.Cwd != "/workspace/project" || c.TGToken != "" {
		t.Fatal("sandbox resume command has wrong history, root or credentials")
	}
}

func TestUUIDLoadMissingSandboxPreservesArchive(t *testing.T) {
	gw, f, work := newTestGateway(t)
	enableLoadAgents(gw)
	gw.cfg.IsolatedRoot = filepath.Join(work, "isolated")
	root := filepath.Join(gw.cfg.IsolatedRoot, "missing")
	gw.store.Put(&Session{ThreadID: 222, Agent: "codex", Ref: loadUUID, Cwd: root, Root: root, Isolated: true, OwnerID: 77})
	gw.store.Detach(222)
	awaitLoadName(t, gw, f, 0, "/codex "+loadUUID)
	gw.handleUpdate(msg(0, "Missing folder"))
	f.waitFor(t, "sendMessage", "folder is gone", 3*time.Second)
	if len(gw.store.Kept()) != 1 || f.find("createForumTopic", "") != nil {
		t.Fatal("missing sandbox lost its archive or created a topic")
	}
}

func TestUUIDLoadRejectsInvalidMissingAndUnavailable(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"", "Usage:"}, {"not-a-uuid", "Usage:"}, {loadUUID + " extra", "Usage:"},
		{"ffffffff-ffff-ffff-ffff-ffffffffffff", "was found on this server"},
		{"abcdefab-1234-5678-90ab-000000000003", "history unavailable"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			gw, f, _ := newTestGateway(t)
			enableLoadAgents(gw)
			gw.handleUpdate(msg(0, "/codex "+tc.arg))
			f.waitFor(t, "sendMessage", tc.want, 3*time.Second)
			if f.find("createForumTopic", "") != nil || f.find("sendMessage", "What should") != nil {
				t.Fatal("invalid session proceeded to naming or creation")
			}
		})
	}
	gw, f, _ := newTestGateway(t)
	enableLoadAgents(gw)
	gw.agentsOK["antigravity"] = false
	gw.handleUpdate(msg(0, "/agy "+loadUUID))
	f.waitFor(t, "sendMessage", "not installed", 3*time.Second)
}

func TestUUIDLoadGuestsCannotLoadOtherSessions(t *testing.T) {
	for _, command := range []string{"claude", "codex", "agy"} {
		t.Run(command, func(t *testing.T) {
			gw, f, work := newTestGateway(t)
			gw.store.Put(&Session{ThreadID: 111, Agent: "codex", Cwd: work, OwnerID: 77, Isolated: true})
			m := msg(111, "/"+command+" "+loadUUID)
			m.Message.From.ID = 77
			gw.handleUpdate(m)
			f.waitFor(t, "sendMessage", "Only an administrator", 3*time.Second)
			if f.find("sendMessage", "Looking for") != nil {
				t.Fatal("guest reached history lookup")
			}
		})
	}
}

func TestUUIDLoadNamingIsBoundToUserAndTopicAndCanCancel(t *testing.T) {
	gw, f, _ := newTestGateway(t)
	enableLoadAgents(gw)
	gw.cfg.AllowedUserIDs = []int64{42, 43}
	c := awaitLoadName(t, gw, f, 0, "/codex "+loadUUID)
	gw.handleUpdate(msg(111, "Message in another topic"))
	other := msg(0, "Other user's message")
	other.Message.From.ID = 43
	gw.handleUpdate(other)
	if f.find("createForumTopic", "") != nil {
		t.Fatal("unrelated message was used as the name")
	}
	gw.handleUpdate(msg(0, strings.Repeat("x", 101)))
	f.waitFor(t, "sendMessage", "up to 100 characters", 3*time.Second)
	wrong := press(0, c.ID, buttons(c)[0])
	wrong.CallbackQuery.From.ID = 43
	gw.handleUpdate(wrong)
	gw.handleUpdate(press(111, c.ID, buttons(c)[0]))
	gw.handleUpdate(press(0, c.ID, buttons(c)[0]))
	f.waitFor(t, "editMessageText", "Session loading cancelled", 3*time.Second)
	gw.handleUpdate(msg(0, "Should not create anything"))
	if f.find("createForumTopic", "") != nil {
		t.Fatal("cancelled naming request created a topic")
	}
}

func TestUUIDLoadCancelCommandDoesNotTreatNameAsCommand(t *testing.T) {
	gw, f, _ := newTestGateway(t)
	enableLoadAgents(gw)
	awaitLoadName(t, gw, f, 0, "/claude "+loadUUID)
	gw.handleUpdate(msg(0, "/cancel@testbot"))
	f.waitFor(t, "sendMessage", "Session loading cancelled", 3*time.Second)
	gw.handleUpdate(msg(0, "/claude "+loadUUID))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		p := gw.awaitTP[42]
		ready := p != nil && p.kind == "loadname"
		gw.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gw.handleUpdate(msg(0, "Cancel project"))
	f.waitFor(t, "sendMessage", "resumed", 3*time.Second)
	if s := gw.store.Get(555); s == nil || s.Name != "Cancel project" {
		t.Fatal("ordinary name was treated as /cancel")
	}
}

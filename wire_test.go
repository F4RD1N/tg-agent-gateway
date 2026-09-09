package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The command Go sends must carry every setting, including the ones that are
// currently empty: the worker treats a missing field as "keep what you had".
func TestCommandCarriesTheWholeConfig(t *testing.T) {
	gw, _, work := newTestGateway(t)
	sess := &Session{
		ThreadID: 5, Agent: "claude", Cwd: work,
		NoUserSettings: true, PermMode: "plan", Thinking: 8000, MaxTurns: 25,
		BudgetUSD: 5, Fallback: "sonnet", Created: time.Now(), LastUsed: time.Now(),
	}
	data, err := json.Marshal(gw.command("prompt", sess, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, key := range []string{
		`"no_user_settings":true`, `"perm_mode":"plan"`, `"thinking":8000`,
		`"max_turns":25`, `"budget_usd":5`, `"fallback_model":"sonnet"`,
		`"model":""`, `"effort":""`, `"resume":""`, `"sandbox":""`,
		`"tg_topic":"5"`,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("the command is missing %s\n%s", key, body)
		}
	}
}

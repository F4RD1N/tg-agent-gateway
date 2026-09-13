package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestBridgeLints keeps the sidecar honest. A missed edit once shipped a
// worker that referenced a variable it never declared, and nothing noticed
// until the branch ran against a live agent hours later: `node --check` sees
// only syntax, so the name check has to come from a linter.
func TestBridgeLints(t *testing.T) {
	if _, err := os.Stat("bridge/node_modules/eslint"); err != nil {
		t.Skip("eslint is not installed (npm install in bridge/)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npx", "eslint", "index.mjs", "worker.mjs", "codex-turn.mjs", "codex-turn.test.mjs")
	cmd.Dir = "bridge"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the bridge does not lint clean:\n%s", strings.TrimSpace(string(out)))
	}
}

// Both bridge files must at least parse, which is cheap and needs nothing
// installed.
func TestBridgeParses(t *testing.T) {
	for _, f := range []string{"bridge/index.mjs", "bridge/worker.mjs", "bridge/codex-turn.mjs", "bridge/codex-turn.test.mjs", "testdata/fakebridge.mjs"} {
		out, err := exec.Command("node", "--check", f).CombinedOutput()
		if err != nil {
			t.Errorf("%s does not parse: %s", f, strings.TrimSpace(string(out)))
		}
	}
}

func TestCodexStreamRecovery(t *testing.T) {
	out, err := exec.Command("node", "--test", "bridge/codex-turn.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("Codex retry regression tests failed:\n%s", out)
	}
}

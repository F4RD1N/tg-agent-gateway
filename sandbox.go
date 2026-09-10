package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// An isolated session is one somebody else can be given without giving them
// this machine. It works in a folder of its own under the isolated root, and
// the agent inside sees that folder as /workspace on a system with no /root:
// no other projects, no memories, no settings, no other session's history.
// Everything else a developer needs - the shell, the compilers, the network -
// is there, and nothing it does reaches the services running outside.

// guestWork is where an isolated session's folder appears from the inside.
const guestWork = "/workspace"

// agentFolder is the per-agent folder inside the isolated root. The three are
// created up front so the layout is obvious to anyone who looks.
func agentFolder(agent string) string {
	switch agent {
	case "codex":
		return "Codex"
	case "antigravity":
		return "Antigravity"
	}
	return "Claude"
}

// isolatedRoot is the folder that holds every sandboxed session.
func (gw *Gateway) isolatedRoot() string {
	if gw.cfg.IsolatedRoot != "" {
		return gw.cfg.IsolatedRoot
	}
	return "/root/isolated"
}

// newSessionID is the name an isolated session's folder gets.
func newSessionID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "session"
	}
	return hex.EncodeToString(b[:])
}

// makeIsolatedDir creates the folder a new sandboxed session works in. No
// question is asked about where it should go: that is the whole point.
func (gw *Gateway) makeIsolatedDir(agent string) (string, error) {
	if !sandboxAvailable() {
		return "", fmt.Errorf("isolated sessions need bubblewrap; install it with: apt-get install -y bubblewrap")
	}
	dir := filepath.Join(gw.isolatedRoot(), agentFolder(agent), newSessionID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("could not create the sandbox folder: %w", err)
	}
	// The outbox is how work comes back out: the agent drops a file in it and
	// the gateway posts it to the topic. It is the sandbox's only way to
	// reach Telegram, because the bot token stays outside.
	_ = os.MkdirAll(filepath.Join(dir, "outbox"), 0o700)
	return dir, nil
}

// sandboxAvailable reports whether this machine can isolate anything at all.
func sandboxAvailable() bool {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return false
	}
	return sandboxRunPath() != ""
}

// sandboxRunPath finds the wrapper script that builds the sandbox. It sits
// next to the bridge once installed, and in the source tree while developing.
func sandboxRunPath() string {
	if p := os.Getenv("TG_SANDBOX_RUN"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "tools", "sandbox-run"),
			filepath.Join(filepath.Dir(dir), "tools", "sandbox-run"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "tools", "sandbox-run"))
	}
	candidates = append(candidates,
		"/opt/tg-agent-gateway/tools/sandbox-run",
		"/usr/local/lib/tg-agent-gateway/sandbox-run",
	)
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// sandboxRoot is the folder an isolated session is confined to. Older
// sessions were stored before the field existed, so the working folder
// stands in for it.
func sandboxRoot(sess *Session) string {
	if sess == nil {
		return ""
	}
	if sess.Root != "" {
		return sess.Root
	}
	return sess.Cwd
}

// guestPath translates a path on this machine into the path the agent sees.
// Outside an isolated session the two are the same.
func guestPath(sess *Session, p string) string {
	root := sandboxRoot(sess)
	if sess == nil || !sess.Isolated || root == "" {
		return p
	}
	if p == root {
		return guestWork
	}
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join(guestWork, rel)
	}
	return guestWork
}

// hostPath translates a path the agent or the user typed back into a path on
// this machine, and refuses anything that would leave the sandbox.
func hostPath(sess *Session, base, p string) (string, error) {
	if sess == nil || !sess.Isolated {
		return resolvePath(base, p), nil
	}
	root := sandboxRoot(sess)
	if p == guestWork || strings.HasPrefix(p, guestWork+"/") {
		p = filepath.Join(root, strings.TrimPrefix(strings.TrimPrefix(p, guestWork), "/"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	if !within(root, p) {
		return "", fmt.Errorf("this session is isolated, so it can only reach its own folder")
	}
	return p, nil
}

// within reports whether p is root or sits inside it, symlinks resolved, so a
// link planted in the sandbox cannot point the gateway at the rest of the disk.
func within(root, p string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	// A path that does not exist yet is judged by the nearest parent that does.
	probe := p
	for {
		if r, err := filepath.EvalSymlinks(probe); err == nil {
			rest := strings.TrimPrefix(p, probe)
			p = filepath.Clean(r + rest)
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// pathAllowed decides whether a session may touch a path: its own folder and
// nothing else if it is isolated, the configured roots otherwise.
func (gw *Gateway) pathAllowed(sess *Session, p string) bool {
	if sess != nil && sess.Isolated {
		return within(sandboxRoot(sess), p)
	}
	return gw.cfg.PathAllowed(p)
}

// sandboxCommand wraps a shell command so it runs inside a session's sandbox.
// /run in an isolated topic must be as confined as the agent is, or it would
// be the way straight out.
func sandboxCommand(sess *Session, cwd, cmdline string) (name string, argv []string, dir string, err error) {
	if sess == nil || !sess.Isolated {
		return "bash", []string{"-lc", cmdline}, cwd, nil
	}
	run := sandboxRunPath()
	if run == "" {
		return "", nil, "", fmt.Errorf("this session is isolated but the sandbox helper is missing, so nothing will be run here")
	}
	root := sandboxRoot(sess)
	inner := guestPath(sess, cwd)
	return run, []string{root, "/bin/bash", "-lc", "cd " + shellQuote(inner) + " && " + cmdline}, root, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

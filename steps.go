package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

// This file turns an agent's raw tool calls into the kind of line an app
// builder shows while it works: "Installing dependencies", not
// `/bin/bash -lc "npm install --no-fund 2>&1 | tail -20"`. Telegram is not a
// terminal, so the command line and its log are noise; the step is the news.

var (
	// The flags are usually combined, as in `bash -lc "…"`, so the -c can be
	// the tail of a flag group rather than a flag of its own.
	reShellWrap = regexp.MustCompile(`^\s*(?:/(?:usr/)?bin/)?(?:ba|z|da|k)?sh\s+(?:-[a-zA-Z]+\s+)*-[a-zA-Z]*c\s+`)
	reEnvPrefix = regexp.MustCompile(`^(?:[A-Z_][A-Z0-9_]*=(?:"[^"]*"|'[^']*'|\S*)\s+)+`)
	reSpaces    = regexp.MustCompile(`\s+`)
)

// cleanCommand unwraps the `bash -lc "…"` the agents wrap everything in and
// flattens it to one line.
func cleanCommand(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < 3; i++ {
		if m := reShellWrap.FindString(s); m != "" {
			s = strings.TrimSpace(s[len(m):])
			s = unquoteShell(s)
			continue
		}
		break
	}
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func unquoteShell(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if (q == '\'' || q == '"') && s[len(s)-1] == q {
		s = s[1 : len(s)-1]
		if q == '"' {
			s = strings.ReplaceAll(s, `\"`, `"`)
			s = strings.ReplaceAll(s, `\$`, `$`)
		} else {
			s = strings.ReplaceAll(s, `'\''`, `'`)
		}
	}
	return s
}

// firstCommand takes the first command out of a pipeline or a && chain, which
// is almost always the one that says what the step is about.
func firstCommand(s string) string {
	for _, sep := range []string{"&&", "||", ";", "|"} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	s = reEnvPrefix.ReplaceAllString(strings.TrimSpace(s), "")
	return strings.TrimSpace(s)
}

type stepRule struct {
	re     *regexp.Regexp
	icon   string
	phrase string
}

// Order matters: the first match wins.
var stepRules = []stepRule{
	{regexp.MustCompile(`^(?:sudo\s+)?(?:npm|pnpm|yarn|bun)\s+(?:i|install|ci|add)\b`), "📦", "Installing dependencies"},
	{regexp.MustCompile(`^(?:sudo\s+)?(?:apt|apt-get|dnf|yum|apk|brew|pip3?|pipx|gem|cargo\s+install|go\s+install)\b`), "📦", "Installing packages"},
	{regexp.MustCompile(`\b(?:go\s+test|pytest|jest|vitest|mocha|npm\s+(?:run\s+)?test|cargo\s+test|gradlew?\s+test|make\s+test)\b`), "🧪", "Running tests"},
	{regexp.MustCompile(`\b(?:go\s+build|cargo\s+build|make\b|gradlew?\s+(?:assemble|build)|npm\s+run\s+build|tsc\b|webpack|vite\s+build)`), "🔨", "Building"},
	{regexp.MustCompile(`^git\s+(?:commit|add|stage)\b`), "💾", "Committing changes"},
	{regexp.MustCompile(`^git\s+(?:push|pull|fetch|clone)\b`), "🔄", "Syncing with git"},
	{regexp.MustCompile(`^git\b`), "🔍", "Checking the repository"},
	{regexp.MustCompile(`^(?:rg|grep|ag|ack|fd|find)\b`), "🔍", "Searching the project"},
	{regexp.MustCompile(`^(?:ls|tree|stat|file|du|df|wc|pwd|which|command\s+-v|whereis)\b`), "🗂", "Looking around"},
	{regexp.MustCompile(`^(?:cat|head|tail|less|more|sed\s+-n|jq|awk)\b`), "📖", "Reading files"},
	{regexp.MustCompile(`^(?:mkdir|touch|cp|mv|install|ln)\b`), "📁", "Setting up files"},
	{regexp.MustCompile(`^(?:rm|rmdir)\b`), "🗑", "Removing files"},
	{regexp.MustCompile(`^(?:curl|wget|http|ping|dig|nslookup|nc)\b`), "🌐", "Checking the network"},
	{regexp.MustCompile(`^(?:tee|printf|echo)\b.*>`), "✏️", "Writing a file"},
	{regexp.MustCompile(`^(?:cat)\b.*<<`), "✏️", "Writing a file"},
	{regexp.MustCompile(`^(?:python3?|node|deno|bun|ruby|perl|php)\b`), "▶️", "Running a script"},
	{regexp.MustCompile(`^(?:systemctl|service|journalctl|systemd-run)\b`), "⚙️", "Managing services"},
	{regexp.MustCompile(`^(?:docker|podman|kubectl|helm)\b`), "🐳", "Working with containers"},
	{regexp.MustCompile(`^(?:zip|unzip|tar|gzip|xz|7z)\b`), "🗜", "Packaging files"},
	{regexp.MustCompile(`^(?:chmod|chown|chgrp|setfacl)\b`), "🔐", "Setting permissions"},
	{regexp.MustCompile(`^(?:nginx|certbot|caddy|apache2ctl)\b`), "🌐", "Configuring the web server"},
	{regexp.MustCompile(`^(?:sleep|wait|timeout)\b`), "⏳", "Waiting"},
	{regexp.MustCompile(`^(?:ps|top|htop|kill|pkill|pgrep|ss|netstat|lsof)\b`), "📊", "Checking processes"},
	{regexp.MustCompile(`^(?:gofmt|go\s+vet|eslint|prettier|ruff|black|golangci-lint)\b`), "🧹", "Tidying the code"},
}

// describeStep is the whole point of this file: one short, human line.
func describeStep(tool, detail string) (icon, phrase string) {
	switch tool {
	case "Bash", "bash", "shell":
		cmd := firstCommand(cleanCommand(detail))
		for _, r := range stepRules {
			if r.re.MatchString(cmd) {
				return r.icon, r.phrase
			}
		}
		if word := firstWord(cmd); word != "" {
			return "⚙️", "Running " + word
		}
		return "⚙️", "Running a command"
	case "Read", "NotebookRead":
		return "📖", "Reading " + shortTarget(detail)
	case "Write":
		return "✏️", "Writing " + shortTarget(detail)
	case "Edit", "NotebookEdit", "MultiEdit":
		return "✏️", "Editing " + shortTarget(detail)
	case "Glob", "Grep", "Search":
		return "🔍", "Searching the project"
	case "WebFetch":
		return "🌐", "Reading " + hostOf(detail)
	case "WebSearch":
		return "🌐", "Searching the web"
	case "Task", "Agent":
		return "🤝", "Asking a subagent"
	case "TodoWrite", "todo":
		return "📋", "Planning the work"
	case "KillShell", "BashOutput":
		return "📊", "Checking a background job"
	}
	if strings.Contains(tool, "/") { // an MCP tool: server/tool
		return "🔌", strings.SplitN(tool, "/", 2)[0]
	}
	if tool != "" {
		return "⚙️", tool
	}
	return "⚙️", "Working"
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	w := filepath.Base(f[0])
	if len(w) > 24 {
		w = w[:24]
	}
	return w
}

// shortTarget keeps a file recognisable without a wall of path.
func shortTarget(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "a file"
	}
	if i := strings.IndexAny(p, " \n"); i > 0 {
		p = p[:i]
	}
	base := filepath.Base(p)
	parent := filepath.Base(filepath.Dir(p))
	if parent != "." && parent != "/" && parent != "" && len(parent)+len(base) < 40 {
		return parent + "/" + base
	}
	return truncate(base, 40)
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexAny(u, "/?"); i > 0 {
		u = u[:i]
	}
	if u == "" {
		return "a page"
	}
	return truncate(u, 40)
}

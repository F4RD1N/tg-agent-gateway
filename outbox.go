package main

import (
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// An isolated session cannot be handed the bot token: whoever works in that
// topic would then be able to drive the whole bot. So it delivers the way a
// build server does - it drops a file in its outbox, and the gateway posts
// what appears there into the session's own topic.
//
// A file may bring a caption with it, either as "name.txt.caption" beside it
// or as a line in "outbox/caption.txt". Delivered files move into
// outbox/.sent, so nothing is ever posted twice.

const outboxName = "outbox"

func outboxDir(sess *Session) string {
	if sess == nil || sess.Cwd == "" {
		return ""
	}
	return filepath.Join(sess.Cwd, outboxName)
}

// drainOutbox posts whatever the session has left for delivery. It is called
// while a turn streams and once more when it ends, so a file shows up in the
// topic at about the moment the agent says it made one.
func (gw *Gateway) drainOutbox(sess *Session) {
	if sess == nil || !sess.Isolated {
		return
	}
	dir := outboxDir(sess)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sent := filepath.Join(dir, ".sent")
	captions := readCaptions(filepath.Join(dir, "caption.txt"))

	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || name == "caption.txt" || strings.HasSuffix(name, ".caption") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			continue
		}
		// Give a file that is still being written a moment to finish.
		if time.Since(st.ModTime()) < 700*time.Millisecond {
			continue
		}
		caption := captions[name]
		if c, err := os.ReadFile(path + ".caption"); err == nil {
			caption = strings.TrimSpace(string(c))
		}
		text := "<code>" + html.EscapeString(name) + "</code>"
		if caption != "" {
			text = html.EscapeString(truncate(caption, 900))
		}
		if st.Size() == 0 {
			continue
		}
		if err := gw.tg.SendDocument(gw.ctx, gw.cfg.ChatID, sess.ThreadID, path, text); err != nil {
			logf("outbox %s: %v", path, err)
			// Leave it in place: the next drain tries again rather than
			// losing the file.
			continue
		}
		if err := os.MkdirAll(sent, 0o700); err == nil {
			_ = os.Rename(path, filepath.Join(sent, name))
		} else {
			_ = os.Remove(path)
		}
		_ = os.Remove(path + ".caption")
	}
}

// readCaptions parses the optional "name<TAB>caption" list.
func readCaptions(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

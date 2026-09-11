package main

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

// A workflow is Claude Code driving a crowd of agents through phases from a
// script it wrote itself. Nothing of that shows in a topic while it happens -
// the turn just takes a long time - so /workflows is the window into it: the
// recent runs, and for one still going, what each of its agents is doing right
// now, refreshed in place until it ends.

// workflowsListed is how many runs the browser shows.
const workflowsListed = 7

// liveRefresh is how often a live workflow view redraws, and for how long it
// keeps doing so unattended.
const (
	liveRefresh  = 6 * time.Second
	liveMaxWatch = 30 * time.Minute
)

// fetchWorkflows asks for this session's recent workflow runs.
func (gw *Gateway) fetchWorkflows(sess *Session) ([]WorkflowRun, error) {
	return gw.workflowCall(Command{
		Type: "workflows", Cwd: sess.Cwd, Resume: sess.Ref, Limit: workflowsListed,
	})
}

// fetchWorkflow asks for one run, including what its agents are doing.
func (gw *Gateway) fetchWorkflow(sess *Session, runID string) (*WorkflowRun, error) {
	list, err := gw.workflowCall(Command{
		Type: "workflow", Cwd: sess.Cwd, Resume: sess.Ref, RunID: runID,
	})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("that run is no longer on disk")
	}
	return &list[0], nil
}

func (gw *Gateway) workflowCall(cmd Command) ([]WorkflowRun, error) {
	gw.mu.Lock()
	gw.reqSeq++
	sid := "wf-" + strconv.FormatInt(gw.reqSeq, 10)
	gw.mu.Unlock()

	ch := gw.bridge.Subscribe(sid)
	defer gw.bridge.Unsubscribe(sid, ch)
	cmd.SID = sid
	if err := gw.bridge.Send(cmd); err != nil {
		return nil, err
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev := <-ch:
			switch ev.Type {
			case "workflows", "workflow":
				return append([]WorkflowRun(nil), ev.Workflows...), nil
			case "error":
				return nil, fmt.Errorf("%s", ev.Message)
			}
		case <-deadline:
			return nil, fmt.Errorf("timed out reading the workflow runs")
		case <-gw.ctx.Done():
			return nil, fmt.Errorf("shutting down")
		}
	}
}

// showWorkflows lists the recent runs.
func (gw *Gateway) showWorkflows(thread, editMsg int, sess *Session) {
	if sess.Agent != "claude" {
		gw.notify(thread, editMsg, "Workflows are a Claude Code thing: it writes a script that runs a crowd of agents. "+
			agentLabel(sess.Agent)+" does not have them.")
		return
	}
	msgID := editMsg
	if msgID > 0 {
		_ = gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, "⏳ looking for workflow runs…", nil)
	} else if m := gw.reply(thread, "⏳ looking for workflow runs…", nil); m != nil {
		msgID = m.MessageID
	}
	guard("workflow list", func() {
		runs, err := gw.fetchWorkflows(sess)
		if err != nil {
			gw.notify(thread, msgID, "⚠️ "+html.EscapeString(err.Error()))
			return
		}
		if len(runs) == 0 {
			gw.notify(thread, msgID, "No workflow has run in this folder yet.\n\n"+
				"<i>Set the effort to Ultracode, or say \"ultracode\" in a message, and Claude "+
				"may orchestrate one for a big job.</i>", Rows(
				[]Button{{Text: "⚡ Effort", CallbackData: "effort"}}))
			return
		}
		var b strings.Builder
		b.WriteString("<b>Workflows</b>\n<i>the last " + strconv.Itoa(len(runs)) + " runs in this folder</i>\n")
		var buttons []Button
		for i, w := range runs {
			b.WriteString("\n" + workflowLine(w))
			buttons = append(buttons, Button{
				Text:         truncate(workflowMark(w)+" "+workflowName(w), 36),
				CallbackData: "wf:" + strconv.Itoa(i),
			})
		}
		kb := Grid(1, buttons)
		kb.InlineKeyboard = append(kb.InlineKeyboard, []Button{{Text: "🔄 Refresh", CallbackData: "wf:reload"}})
		gw.rememberMenu(msgID, &pending{kind: "workflows", agent: sess.Agent, runs: runs, threadID: thread})
		if e := gw.tg.Edit(gw.ctx, gw.cfg.ChatID, msgID, b.String(), kb); e != nil {
			logf("workflow list: %v", e)
		}
	})
}

func workflowName(w WorkflowRun) string {
	if w.Name != "" {
		return w.Name
	}
	return w.ID
}

func workflowMark(w WorkflowRun) string {
	if w.Live {
		return "🔴"
	}
	switch w.Status {
	case "completed":
		return "✅️"
	case "killed", "aborted":
		return "💀"
	case "failed", "error":
		return "✖️"
	case "abandoned":
		return "⚪️"
	}
	return "•"
}

func workflowLine(w WorkflowRun) string {
	s := workflowMark(w) + " <b>" + html.EscapeString(truncate(workflowName(w), 40)) + "</b>"
	if w.AgentCount > 0 {
		s += fmt.Sprintf(" · %d agents", w.AgentCount)
	}
	if d := time.Duration(w.DurationMS) * time.Millisecond; d > 0 {
		s += " · " + shortDuration(d)
	}
	if when, err := time.Parse(time.RFC3339, w.When); err == nil {
		s += " · " + since(when)
	}
	return s
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// renderWorkflow is the view of one run: what it is, how it went, and what
// each of its agents is doing or did.
func renderWorkflow(w WorkflowRun) string {
	var b strings.Builder
	b.WriteString(workflowMark(w) + " <b>" + html.EscapeString(workflowName(w)) + "</b>\n")
	b.WriteString("<code>" + html.EscapeString(w.ID) + "</code>\n")

	status := w.Status
	if w.Live {
		status = "running"
	}
	line := status
	if w.AgentCount > 0 {
		line += fmt.Sprintf(" · %d agents", w.AgentCount)
	}
	if d := time.Duration(w.DurationMS) * time.Millisecond; d > 0 {
		line += " · " + shortDuration(d)
	}
	if w.Tokens > 0 {
		line += fmt.Sprintf(" · %s tokens", thousands(w.Tokens))
	}
	if w.ToolCalls > 0 {
		line += fmt.Sprintf(" · %d tool calls", w.ToolCalls)
	}
	b.WriteString("<i>" + html.EscapeString(line) + "</i>\n")

	if len(w.Phases) > 0 {
		b.WriteString("\n🧭 " + html.EscapeString(truncate(strings.Join(w.Phases, " › "), 300)) + "\n")
	}
	if w.Summary != "" {
		b.WriteString("\n" + html.EscapeString(truncate(w.Summary, 400)) + "\n")
	}

	if len(w.Agents) > 0 {
		b.WriteString("\n")
		shown := w.Agents
		const maxAgents = 12
		extra := 0
		if len(shown) > maxAgents {
			extra = len(shown) - maxAgents
			shown = shown[:maxAgents]
		}
		for _, a := range shown {
			b.WriteString(agentLine(a) + "\n")
		}
		if extra > 0 {
			fmt.Fprintf(&b, "<i>and %d more</i>\n", extra)
		}
	}

	if len(w.Logs) > 0 {
		b.WriteString("\n<b>Log</b>\n")
		logs := w.Logs
		const maxLogs = 12
		if len(logs) > maxLogs {
			logs = logs[len(logs)-maxLogs:]
		}
		for _, l := range logs {
			b.WriteString("· " + html.EscapeString(truncate(l, 150)) + "\n")
		}
	}
	if w.Error != "" {
		b.WriteString("\n⚠️ <i>" + html.EscapeString(truncate(w.Error, 300)) + "</i>\n")
	}
	if w.Live {
		b.WriteString("\n<i>live — this updates itself</i>")
	}
	return strings.TrimSpace(b.String())
}

func agentLine(a WorkflowAgent) string {
	mark := "✳️"
	switch a.State {
	case "done", "completed":
		mark = "✅️"
	case "failed", "error":
		mark = "✖️"
	case "queued", "pending":
		mark = "▫️"
	}
	s := mark + " " + html.EscapeString(truncate(a.Label, 40))
	what := a.Note
	if what == "" {
		what = a.Tool
	}
	if what != "" {
		s += " — <i>" + html.EscapeString(truncate(what, 90)) + "</i>"
	}
	return s
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// showWorkflow opens one run. A run that is still going keeps redrawing on its
// own until it finishes, which is what "live logs" means here.
func (gw *Gateway) showWorkflow(thread, editMsg int, sess *Session, runID string) {
	guard("workflow view", func() {
		w, err := gw.fetchWorkflow(sess, runID)
		if err != nil {
			gw.notify(thread, editMsg, "⚠️ "+html.EscapeString(err.Error()))
			return
		}
		gw.drawWorkflow(thread, editMsg, *w)
		if w.Live {
			gw.watchWorkflow(thread, editMsg, sess, runID)
		}
	})
}

func (gw *Gateway) drawWorkflow(thread, msgID int, w WorkflowRun) {
	kb := Rows(
		[]Button{{Text: "🔄 Refresh", CallbackData: "wfr:" + w.ID}, {Text: "‹ Workflows", CallbackData: "wf:reload"}},
	)
	gw.notify(thread, msgID, renderWorkflow(w), kb)
}

// watchWorkflow redraws a live run until it stops being live. Only one watcher
// runs per message, so opening the same run twice does not double the edits.
func (gw *Gateway) watchWorkflow(thread, msgID int, sess *Session, runID string) {
	if msgID == 0 {
		return
	}
	gw.mu.Lock()
	if gw.watching == nil {
		gw.watching = map[int]string{}
	}
	if gw.watching[msgID] == runID {
		gw.mu.Unlock()
		return
	}
	gw.watching[msgID] = runID
	gw.mu.Unlock()

	guard("workflow watch", func() {
		defer func() {
			gw.mu.Lock()
			if gw.watching[msgID] == runID {
				delete(gw.watching, msgID)
			}
			gw.mu.Unlock()
		}()
		deadline := time.Now().Add(liveMaxWatch)
		for time.Now().Before(deadline) {
			select {
			case <-gw.ctx.Done():
				return
			case <-time.After(liveRefresh):
			}
			// The watcher stops if somebody opened something else here.
			gw.mu.Lock()
			still := gw.watching[msgID] == runID
			gw.mu.Unlock()
			if !still {
				return
			}
			w, err := gw.fetchWorkflow(sess, runID)
			if err != nil {
				return
			}
			gw.drawWorkflow(thread, msgID, *w)
			if !w.Live {
				return
			}
		}
	})
}

// tg-agent-gateway drives Claude Code and Codex from a Telegram forum group:
// one topic per session, everything handled with inline buttons.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `tg-agent-gateway %s

  tg-agent-gateway [-config FILE] serve     run the bot
  tg-agent-gateway [-config FILE] check     verify the token, group, agents and bridge
  tg-agent-gateway [-config FILE] init [flags]
  tg-agent-gateway [-config FILE] sessions  list the sessions it remembers
  tg-agent-gateway version

Default config: %s
`, version, DefaultConfigPath)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	fs := flag.NewFlagSet("tg-agent-gateway", flag.ExitOnError)
	cfgPath := fs.String("config", DefaultConfigPath, "config file")
	fs.Usage = usage
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	var err error
	switch args[0] {
	case "serve":
		err = cmdServe(*cfgPath)
	case "check":
		err = cmdCheck(*cfgPath)
	case "init":
		err = cmdInit(*cfgPath, args[1:])
	case "sessions":
		err = cmdSessions(*cfgPath)
	case "version":
		fmt.Println("tg-agent-gateway", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdServe(path string) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down")
		cancel()
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}()
	gw := NewGateway(ctx, cfg, store)
	log.Printf("tg-agent-gateway %s starting", version)
	return gw.Run()
}

func cmdInit(path string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	token := fs.String("token", "", "bot token from @BotFather")
	chat := fs.Int64("chat", 0, "forum supergroup id, e.g. -1001234567890")
	users := fs.String("users", "", "comma separated Telegram user ids allowed to use the bot")
	admins := fs.String("admins", "", "comma separated ids who may run the gateway (default: everyone allowed)")
	cwd := fs.String("cwd", "/root", "default working directory")
	roots := fs.String("roots", "/root", "comma separated roots sessions may work in")
	isolated := fs.String("isolated", "", "where isolated sessions get their folders")
	bridge := fs.String("bridge", "", "path to the node bridge (index.mjs)")
	force := fs.Bool("force", false, "overwrite an existing config")
	_ = fs.Parse(args)
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s exists (use -force)", path)
	}
	c := DefaultConfig()
	c.BotToken = *token
	c.ChatID = *chat
	c.DefaultCwd = *cwd
	for _, r := range strings.Split(*roots, ",") {
		if r = strings.TrimSpace(r); r != "" {
			c.WorkspaceRoots = append([]string{}, append(c.WorkspaceRoots[:0], r)...)
		}
	}
	c.WorkspaceRoots = splitList(*roots)
	for _, u := range splitList(*users) {
		id, err := strconv.ParseInt(u, 10, 64)
		if err != nil {
			return fmt.Errorf("bad user id %q", u)
		}
		c.AllowedUserIDs = append(c.AllowedUserIDs, id)
	}
	for _, u := range splitList(*admins) {
		id, err := strconv.ParseInt(u, 10, 64)
		if err != nil {
			return fmt.Errorf("bad admin id %q", u)
		}
		c.AdminUserIDs = append(c.AdminUserIDs, id)
	}
	if *isolated != "" {
		c.IsolatedRoot = *isolated
	}
	if *bridge != "" {
		c.BridgeCmd = []string{"node", *bridge}
	}
	if err := c.Save(path); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdCheck(path string) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	fmt.Printf("config:   %s\n", path)
	tg := NewTelegram(cfg.APIBase, cfg.BotToken)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	me, err := tg.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	fmt.Printf("bot:      @%s (%d)\n", me.Username, me.ID)
	chat, err := tg.GetChat(ctx, cfg.ChatID)
	if err != nil {
		return fmt.Errorf("getChat: %w", err)
	}
	fmt.Printf("group:    %q (%d) forum=%v\n", chat.Title, chat.ID, chat.IsForum)
	fmt.Printf("users:    %v\n", cfg.AllowedUserIDs)
	fmt.Printf("roots:    %v (default %s)\n", cfg.WorkspaceRoots, cfg.DefaultCwd)

	for _, bin := range []string{"claude", "codex", "agy", "node"} {
		if p, err := exec.LookPath(bin); err == nil {
			fmt.Printf("%-9s %s\n", bin+":", p)
		} else {
			fmt.Printf("%-9s NOT FOUND\n", bin+":")
		}
	}

	// The bridge must start and answer a ping.
	b := NewBridge(cfg.BridgeCmd)
	if err := b.Start(); err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	done := make(chan bool, 1)
	ch := b.Subscribe("")
	go func() {
		for ev := range ch {
			if ev.Type == "pong" || ev.Type == "hello" {
				done <- true
				return
			}
		}
	}()
	_ = b.Send(Command{Type: "ping"})
	select {
	case <-done:
		fmt.Println("bridge:   ok")
	case <-time.After(20 * time.Second):
		fmt.Println("bridge:   NO RESPONSE")
	}

	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	list := store.List()
	fmt.Printf("sessions: %d\n", len(list))
	for _, s := range list {
		fmt.Printf("  topic %-6d %-12s %s\n", s.ThreadID, s.Agent, s.Cwd)
	}
	return nil
}

func cmdSessions(path string) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(store.List(), "", "  ")
	fmt.Println(string(out))
	return nil
}

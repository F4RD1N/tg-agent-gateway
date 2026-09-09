package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- Bot API types (only the fields this gateway uses) ----------

type TGUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type TGChat struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	IsForum bool   `json:"is_forum"`
}

type TGDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	FileSize int64  `json:"file_size"`
	MimeType string `json:"mime_type"`
}

type TGPhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type TGMessage struct {
	MessageID       int           `json:"message_id"`
	MessageThreadID int           `json:"message_thread_id"`
	IsTopicMessage  bool          `json:"is_topic_message"`
	From            *TGUser       `json:"from"`
	Chat            TGChat        `json:"chat"`
	Date            int64         `json:"date"`
	Text            string        `json:"text"`
	Caption         string        `json:"caption"`
	MediaGroupID    string        `json:"media_group_id"`
	Document        *TGDocument   `json:"document"`
	Photo           []TGPhotoSize `json:"photo"`
	ForumTopic      *struct {
		Name string `json:"name"`
	} `json:"forum_topic_created"`
}

type TGCallbackQuery struct {
	ID      string     `json:"id"`
	From    *TGUser    `json:"from"`
	Message *TGMessage `json:"message"`
	Data    string     `json:"data"`
}

type TGUpdate struct {
	UpdateID      int              `json:"update_id"`
	Message       *TGMessage       `json:"message"`
	EditedMessage *TGMessage       `json:"edited_message"`
	CallbackQuery *TGCallbackQuery `json:"callback_query"`
}

// Button is one inline ("glass") keyboard button.
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// Keyboard is rows of glass buttons. Every choice this bot offers is made of
// these rather than "reply with 1/2/3".
type Keyboard struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

func Rows(rows ...[]Button) *Keyboard { return (&Keyboard{InlineKeyboard: rows}).normalise() }

// Grid lays buttons out n per row, never more than two: three across is
// unreadable on a phone, where the labels get clipped.
func Grid(n int, buttons []Button) *Keyboard {
	if n < 1 || n > maxButtonsPerRow {
		n = maxButtonsPerRow
	}
	var rows [][]Button
	for i := 0; i < len(buttons); i += n {
		end := i + n
		if end > len(buttons) {
			end = len(buttons)
		}
		rows = append(rows, buttons[i:end])
	}
	return &Keyboard{InlineKeyboard: rows}
}

// maxButtonsPerRow is a hard rule of this bot's layout, not a suggestion.
const maxButtonsPerRow = 2

// normalise splits any row that carries more than two buttons.
func (k *Keyboard) normalise() *Keyboard {
	if k == nil {
		return nil
	}
	var rows [][]Button
	for _, row := range k.InlineKeyboard {
		for i := 0; i < len(row); i += maxButtonsPerRow {
			end := i + maxButtonsPerRow
			if end > len(row) {
				end = len(row)
			}
			rows = append(rows, row[i:end])
		}
	}
	k.InlineKeyboard = rows
	return k
}

// padForButtons widens a message bubble. Telegram sizes an inline keyboard to
// the width of the message above it, so a short line like "Which agent?"
// squeezes the buttons until their labels are cut off. A blank Braille line
// (U+2800, which renders as space and is not trimmed) sets a floor.
const bubbleWidth = 36

func padForButtons(text string) string {
	widest := 0
	for _, line := range strings.Split(text, "\n") {
		if n := len([]rune(stripTags(line))); n > widest {
			widest = n
		}
	}
	if widest >= bubbleWidth {
		return text
	}
	return text + "\n" + strings.Repeat("\u2800", bubbleWidth)
}

// ---------- client ----------

type Telegram struct {
	token  string
	base   string
	client *http.Client
	api    string

	mu       sync.Mutex
	nextSend time.Time // crude global pacing: Telegram allows ~30 calls/s overall
}

type apiError struct {
	Code       int
	Desc       string
	RetryAfter int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram %d: %s", e.Code, e.Desc)
}

func NewTelegram(base, token string) *Telegram {
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &Telegram{
		token:  token,
		base:   strings.TrimSuffix(base, "/"),
		api:    strings.TrimSuffix(base, "/") + "/bot" + token + "/",
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

func (t *Telegram) pace() {
	t.mu.Lock()
	now := time.Now()
	if t.nextSend.After(now) {
		wait := t.nextSend.Sub(now)
		t.nextSend = t.nextSend.Add(35 * time.Millisecond)
		t.mu.Unlock()
		time.Sleep(wait)
		return
	}
	t.nextSend = now.Add(35 * time.Millisecond)
	t.mu.Unlock()
}

// call posts JSON and decodes result into out (which may be nil).
func (t *Telegram) call(ctx context.Context, method string, params map[string]any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		t.pace()
		req, err := http.NewRequestWithContext(ctx, "POST", t.api+method, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := t.client.Do(req)
		if err != nil {
			if attempt < 3 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
			return err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var env struct {
			OK          bool            `json:"ok"`
			Result      json.RawMessage `json:"result"`
			Description string          `json:"description"`
			ErrorCode   int             `json:"error_code"`
			Parameters  struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("%s: bad response: %s", method, truncate(string(data), 200))
		}
		if !env.OK {
			e := &apiError{Code: env.ErrorCode, Desc: env.Description, RetryAfter: env.Parameters.RetryAfter}
			if e.RetryAfter > 0 && attempt < 3 {
				time.Sleep(time.Duration(e.RetryAfter+1) * time.Second)
				continue
			}
			if env.ErrorCode >= 500 && attempt < 3 {
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
			return e
		}
		if out != nil && len(env.Result) > 0 {
			return json.Unmarshal(env.Result, out)
		}
		return nil
	}
}

func (t *Telegram) GetMe(ctx context.Context) (*TGUser, error) {
	var u TGUser
	err := t.call(ctx, "getMe", map[string]any{}, &u)
	return &u, err
}

// BotCommand is one entry in Telegram's "/" menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetCommands publishes the command menu, both globally and for the group, so
// typing "/" in the chat lists what the bot understands.
func (t *Telegram) SetCommands(ctx context.Context, chatID int64, cmds []BotCommand) error {
	if err := t.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil); err != nil {
		return err
	}
	return t.call(ctx, "setMyCommands", map[string]any{
		"commands": cmds,
		"scope":    map[string]any{"type": "chat", "chat_id": chatID},
	}, nil)
}

func (t *Telegram) GetChat(ctx context.Context, chatID int64) (*TGChat, error) {
	var c TGChat
	err := t.call(ctx, "getChat", map[string]any{"chat_id": chatID}, &c)
	return &c, err
}

func (t *Telegram) GetUpdates(ctx context.Context, offset int, timeout int) ([]TGUpdate, error) {
	var ups []TGUpdate
	err := t.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

type SendOpts struct {
	ThreadID int
	Keyboard *Keyboard
	Silent   bool
	ReplyTo  int
}

func (t *Telegram) Send(ctx context.Context, chatID int64, text string, o SendOpts) (*TGMessage, error) {
	p := map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if o.ThreadID > 0 {
		p["message_thread_id"] = o.ThreadID
	}
	if o.Keyboard != nil {
		p["reply_markup"] = o.Keyboard.normalise()
		p["text"] = padForButtons(text)
	}
	if o.Silent {
		p["disable_notification"] = true
	}
	if o.ReplyTo > 0 {
		p["reply_parameters"] = map[string]any{"message_id": o.ReplyTo, "allow_sending_without_reply": true}
	}
	var m TGMessage
	if err := t.call(ctx, "sendMessage", p, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (t *Telegram) Edit(ctx context.Context, chatID int64, messageID int, text string, kb *Keyboard) error {
	p := map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if kb != nil {
		p["reply_markup"] = kb.normalise()
		p["text"] = padForButtons(text)
	}
	err := t.call(ctx, "editMessageText", p, nil)
	if e, ok := err.(*apiError); ok && strings.Contains(e.Desc, "message is not modified") {
		return nil
	}
	return err
}

func (t *Telegram) EditKeyboard(ctx context.Context, chatID int64, messageID int, kb *Keyboard) error {
	p := map[string]any{"chat_id": chatID, "message_id": messageID}
	if kb != nil {
		p["reply_markup"] = kb.normalise()
	} else {
		p["reply_markup"] = map[string]any{"inline_keyboard": [][]Button{}}
	}
	err := t.call(ctx, "editMessageReplyMarkup", p, nil)
	if e, ok := err.(*apiError); ok && strings.Contains(e.Desc, "message is not modified") {
		return nil
	}
	return err
}

func (t *Telegram) AnswerCallback(ctx context.Context, id, text string, alert bool) error {
	p := map[string]any{"callback_query_id": id}
	if text != "" {
		p["text"] = text
		p["show_alert"] = alert
	}
	return t.call(ctx, "answerCallbackQuery", p, nil)
}

func (t *Telegram) TypingAction(ctx context.Context, chatID int64, threadID int) {
	p := map[string]any{"chat_id": chatID, "action": "typing"}
	if threadID > 0 {
		p["message_thread_id"] = threadID
	}
	_ = t.call(ctx, "sendChatAction", p, nil)
}

// ---------- forum topics ----------

type ForumTopic struct {
	MessageThreadID int    `json:"message_thread_id"`
	Name            string `json:"name"`
	IconColor       int    `json:"icon_color"`
}

func (t *Telegram) CreateTopic(ctx context.Context, chatID int64, name string, color int) (*ForumTopic, error) {
	p := map[string]any{"chat_id": chatID, "name": name}
	if color != 0 {
		p["icon_color"] = color
	}
	var ft ForumTopic
	if err := t.call(ctx, "createForumTopic", p, &ft); err != nil {
		return nil, err
	}
	return &ft, nil
}

func (t *Telegram) EditTopic(ctx context.Context, chatID int64, threadID int, name string) error {
	return t.call(ctx, "editForumTopic", map[string]any{
		"chat_id": chatID, "message_thread_id": threadID, "name": name,
	}, nil)
}

func (t *Telegram) CloseTopic(ctx context.Context, chatID int64, threadID int) error {
	return t.call(ctx, "closeForumTopic", map[string]any{
		"chat_id": chatID, "message_thread_id": threadID,
	}, nil)
}

func (t *Telegram) DeleteTopic(ctx context.Context, chatID int64, threadID int) error {
	return t.call(ctx, "deleteForumTopic", map[string]any{
		"chat_id": chatID, "message_thread_id": threadID,
	}, nil)
}

func (t *Telegram) Pin(ctx context.Context, chatID int64, messageID int) error {
	return t.call(ctx, "pinChatMessage", map[string]any{
		"chat_id": chatID, "message_id": messageID, "disable_notification": true,
	}, nil)
}

// ---------- files ----------

func (t *Telegram) SendDocument(ctx context.Context, chatID int64, threadID int, path, caption string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if threadID > 0 {
		_ = w.WriteField("message_thread_id", strconv.Itoa(threadID))
	}
	if caption != "" {
		_ = w.WriteField("caption", truncate(caption, 1000))
		_ = w.WriteField("parse_mode", "HTML")
	}
	part, err := w.CreateFormFile("document", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	w.Close()
	t.pace()
	req, err := http.NewRequestWithContext(ctx, "POST", t.api+"sendDocument", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(data, &env)
	if !env.OK {
		return fmt.Errorf("sendDocument: %s", env.Description)
	}
	return nil
}

// Download fetches a file the user sent and writes it under dir.
func (t *Telegram) Download(ctx context.Context, fileID, dir, name string) (string, error) {
	var info struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := t.call(ctx, "getFile", map[string]any{"file_id": fileID}, &info); err != nil {
		return "", err
	}
	if info.FilePath == "" {
		return "", fmt.Errorf("no file path")
	}
	if name == "" {
		name = filepath.Base(info.FilePath)
	}
	name = filepath.Base(name)
	if name == "." || name == "/" || name == "" {
		name = "upload.bin"
	}
	dest := filepath.Join(dir, name)
	u := t.base + "/file/bot" + t.token + "/" + info.FilePath
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return dest, nil
}

// TopicLink is a deep link to a forum topic, usable as a URL button.
func TopicLink(chatID int64, threadID int) string {
	s := strconv.FormatInt(chatID, 10)
	s = strings.TrimPrefix(s, "-100")
	return "https://t.me/c/" + url.PathEscape(s) + "/" + strconv.Itoa(threadID)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

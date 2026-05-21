package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NotifyMessage 是发往任何渠道的统一消息体
type NotifyMessage struct {
	Title     string // 简要标题
	Body      string // 详情正文
	Level     string // info / warn / error / ok(恢复)
	NodeName  string // 关联节点(可空)
	Timestamp int64
}

// Sender 是渠道的发送接口
type Sender interface {
	Send(msg *NotifyMessage) error
}

// NewSender 根据 Notifier 类型构造发送器
func NewSender(n *Notifier) (Sender, error) {
	switch n.Type {
	case "telegram":
		return newTelegramSender(n.Config)
	default:
		return nil, fmt.Errorf("unknown notifier type: %s", n.Type)
	}
}

// === Telegram ===

type telegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
	// 可选:消息推送线程(group topic)
	ThreadID int `json:"thread_id,omitempty"`
}

type telegramSender struct {
	cfg telegramConfig
}

func newTelegramSender(cfgJSON string) (Sender, error) {
	var c telegramConfig
	if err := json.Unmarshal([]byte(cfgJSON), &c); err != nil {
		return nil, fmt.Errorf("telegram config: %w", err)
	}
	if c.BotToken == "" {
		return nil, errors.New("telegram bot_token empty")
	}
	if c.ChatID == "" {
		return nil, errors.New("telegram chat_id empty")
	}
	return &telegramSender{cfg: c}, nil
}

func (t *telegramSender) Send(msg *NotifyMessage) error {
	icon := levelIcon(msg.Level)
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>%s</b>", icon, escapeHTML(msg.Title))
	if msg.NodeName != "" {
		fmt.Fprintf(&b, "\n节点: <code>%s</code>", escapeHTML(msg.NodeName))
	}
	if msg.Body != "" {
		fmt.Fprintf(&b, "\n\n%s", escapeHTML(msg.Body))
	}
	ts := msg.Timestamp
	if ts == 0 {
		ts = time.Now().Unix()
	}
	fmt.Fprintf(&b, "\n\n<i>%s</i>", time.Unix(ts, 0).Format("2006-01-02 15:04:05"))

	payload := map[string]any{
		"chat_id":    t.cfg.ChatID,
		"text":       b.String(),
		"parse_mode": "HTML",
	}
	if t.cfg.ThreadID > 0 {
		payload["message_thread_id"] = t.cfg.ThreadID
	}

	body, _ := json.Marshal(payload)
	api := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage",
		url.PathEscape(t.cfg.BotToken))

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram api %d: %s", resp.StatusCode, string(out))
	}
	return nil
}

func levelIcon(level string) string {
	switch level {
	case "error":
		return "🔴"
	case "warn":
		return "🟡"
	case "ok":
		return "🟢"
	default:
		return "🔵"
	}
}

// HTML escape for Telegram parse_mode=HTML
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// TestNotifier 用临时配置直接发一条测试消息(给"测试推送"按钮用)
func TestNotifier(n *Notifier) error {
	sender, err := NewSender(n)
	if err != nil {
		return err
	}
	return sender.Send(&NotifyMessage{
		Title:     "mote 测试通知",
		Body:      "如果你能看到这条消息,说明渠道配置正确。",
		Level:     "ok",
		Timestamp: time.Now().Unix(),
	})
}

// Dispatch 把消息发给一组通知渠道(并发,任一渠道失败不影响其他)
func Dispatch(store *Store, notifierIDs []int64, msg *NotifyMessage) {
	for _, nid := range notifierIDs {
		n, err := store.GetNotifier(nid)
		if err != nil || !n.Enabled {
			continue
		}
		sender, err := NewSender(n)
		if err != nil {
			log.Printf("notifier #%d build error: %v", nid, err)
			continue
		}
		// 并发发送,但限制超时不要太长
		go func(s Sender, nm *NotifyMessage, name string) {
			if err := s.Send(nm); err != nil {
				log.Printf("notifier %q send failed: %v", name, err)
			}
		}(sender, msg, n.Name)
	}
}

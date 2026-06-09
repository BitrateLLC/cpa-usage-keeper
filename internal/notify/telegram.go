package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// TelegramNotifier 通过 Bot API sendMessage 发送告警。
type TelegramNotifier struct {
	botToken string
	chatID   string
	apiBase  string
	client   *http.Client
}

// TelegramOptions 配置 Bot token、目标 chat 和可选的 API base/HTTP client（便于测试）。
type TelegramOptions struct {
	BotToken string
	ChatID   string
	APIBase  string
	Client   *http.Client
}

// NewTelegramNotifier 返回配置好的通道；token 或 chatID 缺失时返回 nil，调用方据此跳过。
func NewTelegramNotifier(opts TelegramOptions) *TelegramNotifier {
	token := strings.TrimSpace(opts.BotToken)
	chatID := strings.TrimSpace(opts.ChatID)
	if token == "" || chatID == "" {
		return nil
	}
	apiBase := strings.TrimRight(strings.TrimSpace(opts.APIBase), "/")
	if apiBase == "" {
		apiBase = "https://api.telegram.org"
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &TelegramNotifier{botToken: token, chatID: chatID, apiBase: apiBase, client: client}
}

func (t *TelegramNotifier) Notify(ctx context.Context, message string) {
	if t == nil {
		return
	}
	logSendError("telegram", t.send(ctx, message))
}

func (t *TelegramNotifier) send(ctx context.Context, message string) error {
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", t.apiBase, t.botToken)
	payload, err := json.Marshal(map[string]string{
		"chat_id": t.chatID,
		"text":    message,
	})
	if err != nil {
		return fmt.Errorf("marshal telegram payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("send telegram request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logSendError("telegram", fmt.Errorf("close response body: %w", cerr))
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram responded with status %d", resp.StatusCode)
	}
	return nil
}

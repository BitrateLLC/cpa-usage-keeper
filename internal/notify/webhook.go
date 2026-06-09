package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// WebhookNotifier 把告警以 JSON POST 到可配置 URL（企微/飞书/Slack/自建均可消费）。
type WebhookNotifier struct {
	url    string
	client *http.Client
}

// WebhookOptions 配置目标 URL 和可选 HTTP client（便于测试）。
type WebhookOptions struct {
	URL    string
	Client *http.Client
}

// NewWebhookNotifier 返回配置好的通道；URL 缺失时返回 nil，调用方据此跳过。
func NewWebhookNotifier(opts WebhookOptions) *WebhookNotifier {
	url := strings.TrimSpace(opts.URL)
	if url == "" {
		return nil
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &WebhookNotifier{url: url, client: client}
}

func (w *WebhookNotifier) Notify(ctx context.Context, message string) {
	if w == nil {
		return
	}
	logSendError("webhook", w.send(ctx, message))
}

func (w *WebhookNotifier) send(ctx context.Context, message string) error {
	payload, err := json.Marshal(map[string]any{
		"text":      message,
		"timestamp": time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("send webhook request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logSendError("webhook", fmt.Errorf("close response body: %w", cerr))
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook responded with status %d", resp.StatusCode)
	}
	return nil
}

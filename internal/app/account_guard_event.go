package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// cpaErrorEvent 是 CPA errors channel 推送的 error 事件子集，只解码守护逻辑需要的字段。
// 对应 CLIProxyAPI sdk/cliproxy/auth/error_events.go 的 errorEvent。
type cpaErrorEvent struct {
	AuthIndex  string `json:"auth_index"`
	StatusCode int    `json:"status_code"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
}

// parseCPAErrorEvent 解码一条 error channel payload；空或非法 JSON 返回 ok=false 让调用方忽略。
func parseCPAErrorEvent(payload string) (cpaErrorEvent, bool) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return cpaErrorEvent{}, false
	}
	var event cpaErrorEvent
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return cpaErrorEvent{}, false
	}
	return event, true
}

// sleepContext 等待 d 或 ctx 取消；返回 false 表示被 ctx 打断，调用方应退出。
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

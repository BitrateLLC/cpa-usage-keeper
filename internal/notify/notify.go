package notify

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// sendTimeout 限制单次通知发送耗时，避免慢通道阻塞调用方。
const sendTimeout = 5 * time.Second

// Notifier 是告警发送的统一出口；实现必须自带超时，不阻塞调用方业务逻辑。
type Notifier interface {
	Notify(ctx context.Context, message string)
}

// NoopNotifier 在未配置任何通道时使用，保证调用方永远拿到非 nil Notifier。
type NoopNotifier struct{}

func (NoopNotifier) Notify(context.Context, string) {}

// MultiNotifier 把一条消息并发分发到多个通道；单通道失败不影响其它通道。
type MultiNotifier struct {
	targets []Notifier
}

// NewMultiNotifier 组合多个通道；过滤 nil，全空时退化为 Noop 行为。
func NewMultiNotifier(targets ...Notifier) *MultiNotifier {
	filtered := make([]Notifier, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		filtered = append(filtered, target)
	}
	return &MultiNotifier{targets: filtered}
}

// Empty 报告是否没有任何可用通道，便于调用方决定是否退化为 Noop。
func (m *MultiNotifier) Empty() bool {
	return m == nil || len(m.targets) == 0
}

func (m *MultiNotifier) Notify(ctx context.Context, message string) {
	if m == nil || len(m.targets) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, target := range m.targets {
		wg.Add(1)
		go func(t Notifier) {
			defer wg.Done()
			// 每个通道各自带超时，慢通道不拖累其它通道。
			sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
			defer cancel()
			t.Notify(sendCtx, message)
		}(target)
	}
	wg.Wait()
}

// logSendError 统一记录通道发送失败，不向上抛出，保证告警是 best-effort。
func logSendError(channel string, err error) {
	if err == nil {
		return
	}
	logrus.WithError(err).Warnf("account guard alert: %s notify failed", channel)
}

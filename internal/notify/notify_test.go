package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramNotifierSendsPayload(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewTelegramNotifier(TelegramOptions{BotToken: "tok", ChatID: "123", APIBase: server.URL, Client: server.Client()})
	if n == nil {
		t.Fatal("expected non-nil telegram notifier")
	}
	n.Notify(context.Background(), "hello")

	if gotPath != "/bottok/sendMessage" {
		t.Fatalf("unexpected path %q", gotPath)
	}
	if gotBody["chat_id"] != "123" || gotBody["text"] != "hello" {
		t.Fatalf("unexpected body %+v", gotBody)
	}
}

func TestTelegramNotifierNilWhenUnconfigured(t *testing.T) {
	if NewTelegramNotifier(TelegramOptions{BotToken: "", ChatID: "123"}) != nil {
		t.Fatal("expected nil when token missing")
	}
	if NewTelegramNotifier(TelegramOptions{BotToken: "tok", ChatID: ""}) != nil {
		t.Fatal("expected nil when chat id missing")
	}
}

func TestWebhookNotifierSendsPayload(t *testing.T) {
	var gotText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		if v, ok := parsed["text"].(string); ok {
			gotText = v
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	n := NewWebhookNotifier(WebhookOptions{URL: server.URL, Client: server.Client()})
	if n == nil {
		t.Fatal("expected non-nil webhook notifier")
	}
	n.Notify(context.Background(), "report-body")

	if gotText != "report-body" {
		t.Fatalf("unexpected webhook text %q", gotText)
	}
}

func TestWebhookNotifierNilWhenURLMissing(t *testing.T) {
	if NewWebhookNotifier(WebhookOptions{URL: "  "}) != nil {
		t.Fatal("expected nil when url missing")
	}
}

func TestMultiNotifierFansOutAndIsolatesFailure(t *testing.T) {
	var okCount int32
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&okCount, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer okServer.Close()
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failServer.Close()

	good := NewWebhookNotifier(WebhookOptions{URL: okServer.URL, Client: okServer.Client()})
	bad := NewWebhookNotifier(WebhookOptions{URL: failServer.URL, Client: failServer.Client()})
	multi := NewMultiNotifier(good, bad, nil)

	if multi.Empty() {
		t.Fatal("expected non-empty multi notifier")
	}
	// 失败通道不应 panic 或阻断成功通道。
	multi.Notify(context.Background(), "x")
	if atomic.LoadInt32(&okCount) != 1 {
		t.Fatalf("expected ok channel to receive exactly 1, got %d", okCount)
	}
}

func TestMultiNotifierEmpty(t *testing.T) {
	multi := NewMultiNotifier(nil, nil)
	if !multi.Empty() {
		t.Fatal("expected empty multi notifier")
	}
	// 空通道 Notify 应安全无操作。
	multi.Notify(context.Background(), "x")
}

func TestNotifyDoesNotBlockBeyondTimeout(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer once.Do(func() { close(release) })

	// client 超时远小于 sendTimeout，确保慢服务不会长时间阻塞。
	client := &http.Client{Timeout: 200 * time.Millisecond}
	n := NewWebhookNotifier(WebhookOptions{URL: server.URL, Client: client})
	done := make(chan struct{})
	go func() {
		n.Notify(context.Background(), "x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked longer than client timeout")
	}
}

package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-usage-keeper/internal/notify"
	"cpa-usage-keeper/internal/poller"
	"cpa-usage-keeper/internal/quota"
	"cpa-usage-keeper/internal/service"
)

// --- stubs ---

type stubSubscription struct {
	mu     sync.Mutex
	events []string
	idx    int
	closed bool
}

func (s *stubSubscription) Receive(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx < len(s.events) {
		msg := s.events[s.idx]
		s.idx++
		return msg, nil
	}
	// 事件耗尽后返回断开错误，让订阅环结束本连接。
	return "", fmt.Errorf("eof")
}

func (s *stubSubscription) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

type stubSubSource struct {
	mu      sync.Mutex
	subs    []*stubSubscription
	handout int
}

func (s *stubSubSource) Subscribe(ctx context.Context) (poller.UsageSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handout < len(s.subs) {
		sub := s.subs[s.handout]
		s.handout++
		return sub, nil
	}
	// 没有更多预设连接时阻塞到 ctx 取消，模拟空闲订阅。
	<-ctx.Done()
	return nil, ctx.Err()
}

type disableCall struct {
	names    []string
	disabled bool
}

type stubDisabler struct {
	mu    sync.Mutex
	calls []disableCall
	err   error
}

func (s *stubDisabler) SetAuthFilesDisabled(ctx context.Context, names []string, disabled bool) (service.AuthFilesManagementResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, disableCall{names: append([]string(nil), names...), disabled: disabled})
	if s.err != nil {
		return service.AuthFilesManagementResponse{}, s.err
	}
	return service.AuthFilesManagementResponse{Names: names, Affected: len(names)}, nil
}

func (s *stubDisabler) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubDisabler) allNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, c.names...)
	}
	return out
}

// stubProber 按 auth_index 返回预设探测结果(completed=已恢复, failed=仍坏)。
type stubProber struct {
	mu           sync.Mutex
	results      map[string]quota.RefreshTaskStatus
	refreshCalls [][]string
	refreshErr   error
}

func (s *stubProber) Refresh(ctx context.Context, request quota.RefreshRequest) (quota.RefreshResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls = append(s.refreshCalls, append([]string(nil), request.AuthIndexes...))
	if s.refreshErr != nil {
		return quota.RefreshResponse{}, s.refreshErr
	}
	return quota.RefreshResponse{}, nil
}

func (s *stubProber) GetRefreshTaskByAuthIndex(ctx context.Context, authIndex string) (quota.RefreshTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.results[authIndex]
	if !ok {
		status = quota.RefreshTaskStatusFailed
	}
	return quota.RefreshTaskResponse{AuthIndex: authIndex, Status: status}, nil
}

type stubNotifier struct {
	mu       sync.Mutex
	messages []string
}

func (s *stubNotifier) Notify(ctx context.Context, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message)
}

func (s *stubNotifier) last() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) == 0 {
		return "", false
	}
	return s.messages[len(s.messages)-1], true
}

func staticResolver(mapping map[string]string) fileNameResolver {
	return func(ctx context.Context, authIndex string) (string, bool, error) {
		fileName, ok := mapping[authIndex]
		if !ok {
			return "", false, nil
		}
		return fileName, true, nil
	}
}

func staticDisabledLister(accounts []disabledAccount) disabledLister {
	return func(ctx context.Context) ([]disabledAccount, error) {
		return accounts, nil
	}
}

func staticHealthCounter(snapshot healthSnapshot) healthCounter {
	return func(ctx context.Context) (healthSnapshot, error) {
		return snapshot, nil
	}
}

// staticSettings 是固定运行时快照的 settings provider。
type staticSettings struct {
	runtime service.AccountGuardRuntime
}

func (s staticSettings) Snapshot() service.AccountGuardRuntime {
	return s.runtime
}

// codesSet 把状态码列表转成 runtime 用的集合。
func codesSet(codes ...int) map[int]bool {
	set := make(map[int]bool, len(codes))
	for _, code := range codes {
		set[code] = true
	}
	return set
}

// settingsWith 构造一个固定 runtime 的 settings provider。
func settingsWith(enabled bool, disable, monitor map[int]bool, notifier notify.Notifier, interval time.Duration) staticSettings {
	if notifier == nil {
		notifier = notify.NoopNotifier{}
	}
	if disable == nil {
		disable = map[int]bool{}
	}
	if monitor == nil {
		monitor = map[int]bool{}
	}
	return staticSettings{runtime: service.AccountGuardRuntime{
		Enabled:       enabled,
		DisableCodes:  disable,
		MonitorCodes:  monitor,
		AlertInterval: interval,
		Notifier:      notifier,
	}}
}

// baseDeps 提供满足 validate() 的最小可用依赖，单个测试只覆盖关心的字段。
// 默认：启用、禁用码 {401,402}、无监测码、Noop 通知。
func baseDeps() AccountGuardDeps {
	return AccountGuardDeps{
		SubSource:    &stubSubSource{},
		Prober:       &stubProber{},
		Disabler:     &stubDisabler{},
		ListDisabled: staticDisabledLister(nil),
		HealthCounts: staticHealthCounter(healthSnapshot{}),
		Resolve:      staticResolver(nil),
		Settings:     settingsWith(true, codesSet(401, 402), nil, nil, time.Minute),
	}
}

func newTestRunner(t *testing.T, deps AccountGuardDeps) *AccountGuardRunner {
	t.Helper()
	r := NewAccountGuardRunner(deps)
	// 测试用即时 sleep，避免真实等待。
	r.sleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	if err := r.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return r
}

// --- realtime subscribe path tests ---

func TestHandleErrorEventDisablesOn401WithFileName(t *testing.T) {
	disabler := &stubDisabler{}
	deps := baseDeps()
	deps.Disabler = disabler
	deps.Resolve = staticResolver(map[string]string{"idx-1": "acct-a.json"})
	r := newTestRunner(t, deps)

	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "idx-1", StatusCode: 401})
	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "idx-1", StatusCode: 402})

	// 同一 FileName 在冷却窗口内只禁用一次。
	if got := disabler.callCount(); got != 1 {
		t.Fatalf("expected 1 disable call, got %d", got)
	}
	names := disabler.allNames()
	if len(names) != 1 || names[0] != "acct-a.json" {
		t.Fatalf("expected disable acct-a.json, got %v", names)
	}
}

func TestHandleErrorEventIgnoresNon401(t *testing.T) {
	disabler := &stubDisabler{}
	deps := baseDeps()
	deps.Disabler = disabler
	deps.Resolve = staticResolver(map[string]string{"idx-1": "acct-a.json"})
	r := newTestRunner(t, deps)

	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "idx-1", StatusCode: 429})
	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "idx-1", StatusCode: 200})
	if got := disabler.callCount(); got != 0 {
		t.Fatalf("expected no disable calls, got %d", got)
	}
}

func TestHandleErrorEventUnresolvedSkipsDisable(t *testing.T) {
	disabler := &stubDisabler{}
	deps := baseDeps()
	deps.Disabler = disabler
	deps.Resolve = staticResolver(map[string]string{}) // 查不到映射
	r := newTestRunner(t, deps)

	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "unknown", StatusCode: 401})
	if got := disabler.callCount(); got != 0 {
		t.Fatalf("expected no disable calls for unresolved, got %d", got)
	}
	r.mu.Lock()
	unresolved := r.periodUnresolved
	r.mu.Unlock()
	if unresolved != 1 {
		t.Fatalf("expected 1 unresolved, got %d", unresolved)
	}
}

func TestHandleErrorEventDisableOff(t *testing.T) {
	disabler := &stubDisabler{}
	deps := baseDeps()
	deps.Disabler = disabler
	deps.Resolve = staticResolver(map[string]string{"idx-1": "acct-a.json"})
	// 禁用码集合为空：401 不在其中，不应禁用。
	deps.Settings = settingsWith(true, nil, nil, nil, time.Minute)
	r := newTestRunner(t, deps)

	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "idx-1", StatusCode: 401})
	if got := disabler.callCount(); got != 0 {
		t.Fatalf("expected no disable when 401 is not a disable code, got %d", got)
	}
}

func TestHandleErrorEventGuardDisabledSkips(t *testing.T) {
	disabler := &stubDisabler{}
	deps := baseDeps()
	deps.Disabler = disabler
	deps.Resolve = staticResolver(map[string]string{"idx-1": "acct-a.json"})
	// 守护整体关闭：即便 401 在禁用码内也不处理。
	deps.Settings = settingsWith(false, codesSet(401, 402), nil, nil, time.Minute)
	r := newTestRunner(t, deps)

	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "idx-1", StatusCode: 401})
	if got := disabler.callCount(); got != 0 {
		t.Fatalf("expected no disable when guard disabled, got %d", got)
	}
}

func TestHandleErrorEventMonitorOnlyCounts(t *testing.T) {
	disabler := &stubDisabler{}
	deps := baseDeps()
	deps.Disabler = disabler
	deps.Resolve = staticResolver(map[string]string{"idx-1": "acct-a.json"})
	// 429 仅在监测码内：只计数，不禁用。
	deps.Settings = settingsWith(true, codesSet(401, 402), codesSet(429), nil, time.Minute)
	r := newTestRunner(t, deps)

	r.handleErrorEvent(context.Background(), cpaErrorEvent{AuthIndex: "idx-1", StatusCode: 429})
	if got := disabler.callCount(); got != 0 {
		t.Fatalf("expected no disable for monitor-only code, got %d", got)
	}
	r.mu.Lock()
	monitored := r.periodMonitored
	r.mu.Unlock()
	if monitored != 1 {
		t.Fatalf("expected 1 monitored hit, got %d", monitored)
	}
}

// --- periodic recovery-probe path tests ---

func TestComposeReportRatios(t *testing.T) {
	r := newTestRunner(t, baseDeps())
	report := r.composeReport(healthSnapshot{Total: 10, Disabled: 2}, 2, nil)
	for _, want := range []string{"总数 10", "正常 8 (80.0%)", "禁用 2 (20.0%)"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q: %s", want, report)
		}
	}
}

func TestRunAlertCycleProbesDisabledAndReportsRecovery(t *testing.T) {
	disabled := []disabledAccount{
		{AuthIndex: "idx-1", FileName: "acct-a.json"},
		{AuthIndex: "idx-2", FileName: "acct-b.json"},
	}
	prober := &stubProber{results: map[string]quota.RefreshTaskStatus{
		"idx-1": quota.RefreshTaskStatusCompleted, // 已恢复
		"idx-2": quota.RefreshTaskStatusFailed,    // 仍坏
	}}
	disabler := &stubDisabler{}
	notifier := &stubNotifier{}
	deps := baseDeps()
	deps.Prober = prober
	deps.Disabler = disabler
	deps.ListDisabled = staticDisabledLister(disabled)
	deps.HealthCounts = staticHealthCounter(healthSnapshot{Total: 5, Disabled: 2})
	deps.Settings = settingsWith(true, codesSet(401, 402), nil, notifier, time.Minute)
	r := newTestRunner(t, deps)

	r.runAlertCycle(context.Background())

	// 恢复探测不应触发任何禁用/启用动作(只告警)。
	if got := disabler.callCount(); got != 0 {
		t.Fatalf("expected no disable/enable calls from recovery probe, got %d", got)
	}
	// 应对两个被禁号发起一次 Refresh。
	prober.mu.Lock()
	refreshCount := len(prober.refreshCalls)
	prober.mu.Unlock()
	if refreshCount != 1 {
		t.Fatalf("expected 1 refresh call, got %d", refreshCount)
	}
	msg, ok := notifier.last()
	if !ok {
		t.Fatal("expected a notification")
	}
	if !strings.Contains(msg, "已恢复 1 个") || !strings.Contains(msg, "acct-a.json") {
		t.Fatalf("report should mention recovered acct-a.json: %s", msg)
	}
	if strings.Contains(msg, "acct-b.json") {
		t.Fatalf("still-bad acct-b.json should not be in recovered list: %s", msg)
	}
}

func TestRunAlertCycleNoDisabledStillReports(t *testing.T) {
	notifier := &stubNotifier{}
	deps := baseDeps()
	deps.ListDisabled = staticDisabledLister(nil)
	deps.HealthCounts = staticHealthCounter(healthSnapshot{Total: 8, Disabled: 0})
	deps.Settings = settingsWith(true, codesSet(401, 402), nil, notifier, time.Minute)
	r := newTestRunner(t, deps)

	r.runAlertCycle(context.Background())

	msg, ok := notifier.last()
	if !ok {
		t.Fatal("expected a notification even with no disabled accounts")
	}
	if !strings.Contains(msg, "正常 8 (100.0%)") {
		t.Fatalf("report should show full health: %s", msg)
	}
}

func TestRunAlertCycleResetsPeriodCounters(t *testing.T) {
	deps := baseDeps()
	r := newTestRunner(t, deps)
	// 模拟实时环本周期记了一次禁用。
	r.recordDisabled("acct-x.json")
	r.runAlertCycle(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.periodDisabled) != 0 {
		t.Fatalf("expected period counters reset, got %d", len(r.periodDisabled))
	}
}

func TestParseCPAErrorEvent(t *testing.T) {
	event, ok := parseCPAErrorEvent(`{"auth_index":"x","status_code":401,"provider":"claude"}`)
	if !ok {
		t.Fatal("expected ok")
	}
	if event.AuthIndex != "x" || event.StatusCode != 401 {
		t.Fatalf("unexpected event %+v", event)
	}
	if _, ok := parseCPAErrorEvent("  "); ok {
		t.Fatal("expected empty payload to be ignored")
	}
	if _, ok := parseCPAErrorEvent("not-json"); ok {
		t.Fatal("expected invalid json to be ignored")
	}
}

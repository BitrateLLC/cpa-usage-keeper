package test

import (
	"context"
	"errors"
	"testing"
	"time"

	"cpa-usage-keeper/internal/entities"
	. "cpa-usage-keeper/internal/quota"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
)

func TestStartAutoRefreshWithNilServiceReturns(t *testing.T) {
	var service *Service
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := service.StartAutoRefresh(ctx); err != nil {
		t.Fatalf("expected nil service auto refresh to return nil, got %v", err)
	}
}

func TestRunAutoRefreshSkipsWhenNoActiveStatus(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})

	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("RunAutoRefresh returned error: %v", err)
	}
	if handler.callCount() != 0 {
		t.Fatalf("expected inactive backend page to skip provider calls, got %d", handler.callCount())
	}
	if _, err := service.GetRefreshTaskByAuthIndex(context.Background(), "auth-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expected inactive backend page to leave auth-1 out of queue, got %v", err)
	}
}

func TestRecordActiveStatusStartsAutoRefreshWhenBecomingActive(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})

	service.RecordActiveStatus(time.Now())

	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusCompleted)
	service.WaitRefreshTasks()
	if handler.callCount() != 1 {
		t.Fatalf("expected first active heartbeat to start one auto refresh round, got %d calls", handler.callCount())
	}
}

func TestRecordActiveStatusDoesNotRestartAutoRefreshWhileAlreadyActive(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})
	now := time.Now()

	service.RecordActiveStatus(now)
	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusCompleted)
	service.WaitRefreshTasks()
	service.RecordActiveStatus(now.Add(30 * time.Second))
	service.WaitRefreshTasks()

	if handler.callCount() != 1 {
		t.Fatalf("expected repeated active heartbeat to avoid starting another auto refresh round, got %d calls", handler.callCount())
	}
}

func TestRecordActiveStatusDoesNotStartAutoRefreshWhenLastRoundIsRecent(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})
	now := time.Now()
	markBackendPageActiveForAutoRefreshTest(service, now.Add(-AutoRefreshActiveTTL-time.Second))
	setLastAutoRefreshRoundAt(service, now.Add(-time.Minute))

	service.RecordActiveStatus(now)
	service.WaitRefreshTasks()

	if handler.callCount() != 0 {
		t.Fatalf("expected recent auto refresh round to suppress heartbeat wakeup, got %d calls", handler.callCount())
	}
	if _, err := service.GetRefreshTaskByAuthIndex(context.Background(), "auth-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expected recent round suppression to avoid queueing auth-1, got %v", err)
	}
}

func TestRunAutoRefreshQueuesOnlyActiveAuthFiles(t *testing.T) {
	db := openQuotaTestDatabase(t)
	disabled := true
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "  ", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "disabled-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile, Disabled: &disabled})
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "deleted-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile, IsDeleted: true})
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "provider-1", Provider: "openai", Type: "openai", AuthType: entities.UsageIdentityAuthTypeAIProvider})
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "unsupported-1", Provider: "vertex", Type: "vertex", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})
	markBackendPageActiveForAutoRefreshTest(service, time.Now())

	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("RunAutoRefresh returned error: %v", err)
	}
	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusCompleted)
	service.WaitRefreshTasks()
	if handler.callCount() != 1 {
		t.Fatalf("expected only active auth file to refresh, got %d calls", handler.callCount())
	}
	for _, authIndex := range []string{"disabled-1", "deleted-1", "provider-1", "unsupported-1"} {
		if _, err := service.GetRefreshTaskByAuthIndex(context.Background(), authIndex); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("expected %s to stay out of auto refresh queue, got %v", authIndex, err)
		}
	}
	_, hasBlankTask := refreshTasks(service)["  "]
	if hasBlankTask {
		t.Fatal("expected blank identity to stay out of auto refresh queue")
	}
}

func TestRunAutoRefreshRunsWhenStatusIsActive(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-2", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})
	markBackendPageActiveForAutoRefreshTest(service, time.Now())

	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("RunAutoRefresh returned error: %v", err)
	}
	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusCompleted)
	waitForRefreshTask(t, service, "auth-2", RefreshTaskStatusCompleted)
	service.WaitRefreshTasks()
	if handler.callCount() != 2 {
		t.Fatalf("expected active backend page to refresh all auth files, got %d calls", handler.callCount())
	}
}

func TestRunAutoRefreshStoresAliasDisplayName(t *testing.T) {
	db := openQuotaTestDatabase(t)
	alias := "Friendly Alias"
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Alias: &alias, Name: "Upstream Name", Provider: "claude", Type: "claude", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})
	markBackendPageActiveForAutoRefreshTest(service, time.Now())

	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("RunAutoRefresh returned error: %v", err)
	}
	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusCompleted)
	service.WaitRefreshTasks()

	task := refreshTaskRecord(service, "auth-1")
	if task == nil || task.Name != "Friendly Alias" {
		t.Fatalf("expected auto refresh task name to use alias display name, got %+v", task)
	}
}

func TestRunAutoRefreshSkipsWhenPreviousAutoRoundIsActive(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	block := make(chan struct{})
	handler := &refreshHandlerStub{block: block, output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})
	markBackendPageActiveForAutoRefreshTest(service, time.Now())
	hook := logrustest.NewGlobal()
	t.Cleanup(func() {
		hook.Reset()
	})

	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("first RunAutoRefresh returned error: %v", err)
	}
	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusRunning)
	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("second RunAutoRefresh returned error: %v", err)
	}

	assertAutoRefreshRoundLogs(t, hook, 1, 0)
	close(block)
	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusCompleted)
	service.WaitRefreshTasks()
	assertAutoRefreshRoundLogs(t, hook, 1, 1)
}

func TestRunAutoRefreshSkipsCachedHTTPFailures(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{err: ProviderHTTPError{StatusCode: 401, Message: "expired token"}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})

	first, err := service.Refresh(context.Background(), RefreshRequest{AuthIndexes: []string{"auth-1"}, Source: RefreshSourceManual})
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	waitForRefreshTask(t, service, first.Tasks[0].AuthIndex, RefreshTaskStatusFailed)
	if handler.callCount() != 1 {
		t.Fatalf("expected one manual provider call, got %d", handler.callCount())
	}
	markBackendPageActiveForAutoRefreshTest(service, time.Now())
	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("RunAutoRefresh returned error: %v", err)
	}
	if handler.callCount() != 1 {
		t.Fatalf("expected auto refresh to skip cached 401, got %d calls", handler.callCount())
	}
}

func TestRunAutoRefreshLogsRoundStartAndEndOnce(t *testing.T) {
	db := openQuotaTestDatabase(t)
	seedUsageIdentity(t, db, entities.UsageIdentity{Identity: "auth-1", Provider: "claude", Type: "auth-file", AuthType: entities.UsageIdentityAuthTypeAuthFile})
	handler := &refreshHandlerStub{output: ProviderOutput{Result: ClaudeResult{Usage: &ClaudeUsagePayload{FiveHour: &ClaudeUsageWindow{Utilization: 25}}}}}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(map[string]ProviderHandler{"claude": handler}))
	setRefreshCooldown(service, func(time.Duration) {})
	markBackendPageActiveForAutoRefreshTest(service, time.Now())
	hook := logrustest.NewGlobal()
	t.Cleanup(func() {
		hook.Reset()
	})

	if err := service.RunAutoRefresh(context.Background()); err != nil {
		t.Fatalf("RunAutoRefresh returned error: %v", err)
	}

	waitForRefreshTask(t, service, "auth-1", RefreshTaskStatusCompleted)
	service.WaitRefreshTasks()
	assertAutoRefreshRoundLogs(t, hook, 1, 1)
}

func TestRunAutoRefreshLogsRoundEndWhenIdentityScanFails(t *testing.T) {
	db := openQuotaTestDatabase(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB returned error: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db returned error: %v", err)
	}
	service := newQuotaServiceWithRegistry(t, db, NewProviderRegistry(nil))
	markBackendPageActiveForAutoRefreshTest(service, time.Now())
	hook := logrustest.NewGlobal()
	t.Cleanup(func() {
		hook.Reset()
	})

	if err := service.RunAutoRefresh(context.Background()); err == nil {
		t.Fatal("expected RunAutoRefresh to return scan error")
	}

	assertAutoRefreshRoundLogs(t, hook, 1, 1)
	lastRoundAt := lastAutoRefreshRoundAt(service)
	lastAttemptAt := lastAutoRefreshAttemptAt(service)
	if !lastRoundAt.IsZero() {
		t.Fatalf("expected failed identity scan not to update last auto refresh round time, got %v", lastRoundAt)
	}
	if lastAttemptAt.IsZero() {
		t.Fatal("expected failed identity scan to record last auto refresh attempt time for backoff")
	}
}

func assertAutoRefreshRoundLogs(t *testing.T, hook *logrustest.Hook, wantStart int, wantEnd int) {
	t.Helper()
	startLogs := 0
	endLogs := 0
	for _, entry := range hook.AllEntries() {
		if entry.Level != logrus.InfoLevel {
			continue
		}
		switch entry.Message {
		case "quota auto refresh round started":
			startLogs++
		case "quota auto refresh round completed":
			endLogs++
		}
	}
	if startLogs != wantStart || endLogs != wantEnd {
		t.Fatalf("expected start=%d end=%d info logs, got start=%d end=%d entries=%+v", wantStart, wantEnd, startLogs, endLogs, hook.AllEntries())
	}
}

func markBackendPageActiveForAutoRefreshTest(service *Service, at time.Time) {
	setBackendPageActiveForAutoRefreshTest(service, at)
}

func TestNewServiceWithRegistryAndOptionsUsesConfiguredAutoRefreshInterval(t *testing.T) {
	db := openQuotaTestDatabase(t)
	service := newQuotaServiceWithRegistryAndOptions(t, db, NewProviderRegistry(nil), ServiceOptions{AutoRefreshInterval: 2 * time.Minute})
	if autoRefreshInterval(service) != 2*time.Minute {
		t.Fatalf("expected configured auto refresh interval 2m, got %s", autoRefreshInterval(service))
	}
}

func TestAutoRefreshActiveTTLStaysShortWhenIntervalIsLong(t *testing.T) {
	db := openQuotaTestDatabase(t)
	service := newQuotaServiceWithRegistryAndOptions(t, db, NewProviderRegistry(nil), ServiceOptions{AutoRefreshInterval: 5 * time.Minute})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.Local)

	service.RecordActiveStatus(now)
	service.WaitRefreshTasks()

	if !service.HasRecentActiveStatus(now.Add(AutoRefreshActiveTTL - time.Second)) {
		t.Fatal("expected status heartbeat to stay active inside the fixed TTL")
	}
	if service.HasRecentActiveStatus(now.Add(AutoRefreshActiveTTL + time.Second)) {
		t.Fatal("expected status heartbeat to expire soon after the fixed TTL even when auto refresh interval is long")
	}
}

func TestNextAutoRefreshDelayUsesLastRoundTime(t *testing.T) {
	db := openQuotaTestDatabase(t)
	service := newQuotaServiceWithRegistryAndOptions(t, db, NewProviderRegistry(nil), ServiceOptions{AutoRefreshInterval: 5 * time.Minute})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.Local)
	markBackendPageActiveForAutoRefreshTest(service, now)
	setLastAutoRefreshRoundAt(service, now.Add(-4*time.Minute))

	delay := nextAutoRefreshDelay(service, now)

	if delay != time.Minute {
		t.Fatalf("expected next auto refresh delay 1m after recent heartbeat round, got %s", delay)
	}
}

func TestNextAutoRefreshDelayUsesLastAttemptAfterScanFailure(t *testing.T) {
	db := openQuotaTestDatabase(t)
	service := newQuotaServiceWithRegistryAndOptions(t, db, NewProviderRegistry(nil), ServiceOptions{AutoRefreshInterval: 5 * time.Minute})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.Local)
	markBackendPageActiveForAutoRefreshTest(service, now)
	setLastAutoRefreshAttemptAt(service, now.Add(-4*time.Minute))

	delay := nextAutoRefreshDelay(service, now)

	if delay != time.Minute {
		t.Fatalf("expected next auto refresh delay 1m after failed attempt, got %s", delay)
	}
}

func TestNextAutoRefreshDelayWaitsForNextTriggerWhenRoundIsRunning(t *testing.T) {
	db := openQuotaTestDatabase(t)
	service := newQuotaServiceWithRegistryAndOptions(t, db, NewProviderRegistry(nil), ServiceOptions{AutoRefreshInterval: 5 * time.Minute})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.Local)
	markBackendPageActiveForAutoRefreshTest(service, now)
	setAutoRefreshRunning(service, true)
	setLastAutoRefreshRoundAt(service, now.Add(-5*time.Minute))

	delay := nextAutoRefreshDelay(service, now)

	if delay != 5*time.Minute {
		t.Fatalf("expected running round to wait for the next scheduled trigger, got %s", delay)
	}
}

func TestRefreshCacheableHTTPStatusCodesAlsoControlAutoRefreshSkip(t *testing.T) {
	if _, ok := RefreshCacheableHTTPStatusCodes[401]; !ok {
		t.Fatal("expected 401 to be configured as cacheable and auto-refresh-skipped")
	}
}

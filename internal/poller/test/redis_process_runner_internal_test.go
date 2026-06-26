package poller_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cpa-usage-keeper/internal/poller"
	servicedto "cpa-usage-keeper/internal/service/dto"
)

func TestRedisProcessRunnerSleepsAfterNonFullBatch(t *testing.T) {
	syncer := &sequenceRedisProcessSyncer{results: []redisProcessSyncerResult{{
		result: &servicedto.RedisBatchSyncResult{Status: "completed", ProcessedRows: 999},
	}}}
	runner := poller.NewRedisProcessRunner(syncer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	setRedisProcessRunnerSleep(t, runner, func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		cancel()
		return false
	})

	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if calls := syncer.callCount(); calls != 1 {
		t.Fatalf("expected one process call before sleep, got %d", calls)
	}
	requireDurations(t, delays, []time.Duration{time.Second})
}

func TestRedisProcessRunnerSkipsSleepAfterFullBatch(t *testing.T) {
	syncer := &sequenceRedisProcessSyncer{results: []redisProcessSyncerResult{
		{result: &servicedto.RedisBatchSyncResult{Status: "completed", ProcessedRows: 1000, BatchFull: true}},
		{result: &servicedto.RedisBatchSyncResult{Empty: true, Status: "empty"}},
	}}
	runner := poller.NewRedisProcessRunner(syncer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	setRedisProcessRunnerSleep(t, runner, func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		cancel()
		return false
	})

	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if calls := syncer.callCount(); calls != 2 {
		t.Fatalf("expected full batch to process again before sleeping, got %d calls", calls)
	}
	requireDurations(t, delays, []time.Duration{time.Second})
}

func TestRedisProcessRunnerSkipsSleepAfterWarningFullBatch(t *testing.T) {
	syncer := &sequenceRedisProcessSyncer{results: []redisProcessSyncerResult{
		{result: &servicedto.RedisBatchSyncResult{Status: "completed_with_warnings", ProcessedRows: 1000, BatchFull: true}, err: errors.New("decode warning")},
		{result: &servicedto.RedisBatchSyncResult{Empty: true, Status: "empty"}},
	}}
	runner := poller.NewRedisProcessRunner(syncer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	setRedisProcessRunnerSleep(t, runner, func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		cancel()
		return false
	})

	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if calls := syncer.callCount(); calls != 2 {
		t.Fatalf("expected warning full batch to process again before sleeping, got %d calls", calls)
	}
	requireDurations(t, delays, []time.Duration{time.Second})
}

func TestRedisProcessRunnerSleepsAfterFailedFullBatch(t *testing.T) {
	syncer := &sequenceRedisProcessSyncer{results: []redisProcessSyncerResult{{
		result: &servicedto.RedisBatchSyncResult{Status: "failed", ProcessedRows: 1000, BatchFull: true},
		err:    errors.New("sqlite locked"),
	}}}
	runner := poller.NewRedisProcessRunner(syncer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	setRedisProcessRunnerSleep(t, runner, func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		cancel()
		return false
	})

	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if calls := syncer.callCount(); calls != 1 {
		t.Fatalf("expected one failed full batch process call before sleep, got %d", calls)
	}
	requireDurations(t, delays, []time.Duration{time.Second})
}

type redisProcessSyncerResult struct {
	result *servicedto.RedisBatchSyncResult
	err    error
}

type sequenceRedisProcessSyncer struct {
	mu      sync.Mutex
	results []redisProcessSyncerResult
	calls   int
}

func (s *sequenceRedisProcessSyncer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *sequenceRedisProcessSyncer) ProcessRedisUsageInbox(context.Context) (*servicedto.RedisBatchSyncResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls >= len(s.results) {
		s.calls++
		return &servicedto.RedisBatchSyncResult{Empty: true, Status: "empty"}, nil
	}
	result := s.results[s.calls]
	s.calls++
	return result.result, result.err
}

package test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/cpa"
	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"
	"cpa-usage-keeper/internal/service"
	"gorm.io/gorm"
)

type requestLogClientStub struct {
	calls  int
	result *cpa.RequestLogResult
	err    error

	downloadCalls  int
	downloadResult *cpa.RequestLogStream
	downloadErr    error
}

func (s *requestLogClientStub) FetchRequestLogByID(context.Context, string) (*cpa.RequestLogResult, error) {
	s.calls++
	return s.result, s.err
}

func (s *requestLogClientStub) OpenRequestLogByID(context.Context, string) (*cpa.RequestLogStream, error) {
	s.downloadCalls++
	if s.downloadResult != nil || s.downloadErr != nil {
		return s.downloadResult, s.downloadErr
	}
	if s.result == nil {
		return nil, s.err
	}
	return &cpa.RequestLogStream{
		StatusCode:    s.result.StatusCode,
		Filename:      s.result.Filename,
		ContentType:   s.result.ContentType,
		ContentLength: int64(len(s.result.Body)),
		Body:          io.NopCloser(bytes.NewReader(s.result.Body)),
	}, s.err
}

func TestRequestLogServiceLoadsEventLogAndCachesByRequestID(t *testing.T) {
	db := openRequestLogTestDB(t)
	if _, _, err := repository.InsertUsageEvents(db, []entities.UsageEvent{{
		EventKey:    "event-1",
		RequestID:   "req-log-1",
		Timestamp:   time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local),
		Model:       "claude-sonnet",
		Source:      "source",
		AuthIndex:   "auth-1",
		APIGroupKey: "group",
	}}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}

	client := &requestLogClientStub{result: &cpa.RequestLogResult{
		StatusCode: http.StatusOK,
		Filename:   "v1-responses-req-log-1.log",
		Body:       []byte("=== REQUEST INFO ===\nURL: /v1/responses\n=== API RESPONSE ===\n{\"ok\":true}\n"),
	}}
	provider := service.NewRequestLogService(db, client)

	first, err := provider.GetUsageEventRequestLog(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetUsageEventRequestLog returned error: %v", err)
	}
	if first.RequestID != "req-log-1" || first.Filename != "v1-responses-req-log-1.log" || first.Cached {
		t.Fatalf("unexpected first response: %+v", first)
	}
	if len(first.Sections) != 2 || first.Sections[0].Title != "REQUEST INFO" || first.Sections[1].Title != "API RESPONSE" {
		t.Fatalf("unexpected sections: %+v", first.Sections)
	}

	second, err := provider.GetUsageEventRequestLog(context.Background(), 1)
	if err != nil {
		t.Fatalf("cached GetUsageEventRequestLog returned error: %v", err)
	}
	if !second.Cached {
		t.Fatalf("expected cached response, got %+v", second)
	}
	if client.calls != 1 {
		t.Fatalf("expected one CPA call, got %d", client.calls)
	}
}

func TestRequestLogServiceMapsCPANotFoundToUnavailable(t *testing.T) {
	db := openRequestLogTestDB(t)
	if _, _, err := repository.InsertUsageEvents(db, []entities.UsageEvent{{
		EventKey:  "event-404",
		RequestID: "req-missing",
	}}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}

	client := &requestLogClientStub{
		result: &cpa.RequestLogResult{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"missing"}`)},
		err:    errors.New("management request log request returned status 404"),
	}
	provider := service.NewRequestLogService(db, client)

	_, err := provider.GetUsageEventRequestLog(context.Background(), 1)
	if !errors.Is(err, service.ErrRequestLogUnavailable) {
		t.Fatalf("expected ErrRequestLogUnavailable, got %v", err)
	}

	_, err = provider.GetUsageEventRequestLog(context.Background(), 1)
	if !errors.Is(err, service.ErrRequestLogUnavailable) {
		t.Fatalf("expected cached ErrRequestLogUnavailable, got %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("expected 404 response to be negative cached, got %d calls", client.calls)
	}
}

func TestRequestLogServiceHandlesLargePreviewAsDownloadable(t *testing.T) {
	db := openRequestLogTestDB(t)
	if _, _, err := repository.InsertUsageEvents(db, []entities.UsageEvent{{
		EventKey:  "event-large",
		RequestID: "req-large",
	}}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}

	client := &requestLogClientStub{result: &cpa.RequestLogResult{
		StatusCode:    http.StatusOK,
		Filename:      "large-request.log",
		Body:          make([]byte, service.RequestLogPreviewMaxBytes()+1),
		BodyTruncated: true,
		ContentType:   "text/plain",
		ContentLength: int64(service.RequestLogPreviewMaxBytes() + 1),
	}}
	provider := service.NewRequestLogService(db, client)

	response, err := provider.GetUsageEventRequestLog(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetUsageEventRequestLog returned error: %v", err)
	}
	if !response.TooLarge || !response.Downloadable || response.Previewable || response.Raw != "" || len(response.Sections) != 0 {
		t.Fatalf("unexpected large preview response: %+v", response)
	}
	if response.Filename != "large-request.log" {
		t.Fatalf("unexpected filename %q", response.Filename)
	}
}

func TestRequestLogServicePrunesCacheByTotalBytes(t *testing.T) {
	db := openRequestLogTestDB(t)
	eventCount := 21
	events := make([]entities.UsageEvent, 0, eventCount)
	for i := 0; i < eventCount; i++ {
		events = append(events, entities.UsageEvent{
			EventKey:  "event-prune-" + strconv.Itoa(i),
			RequestID: "req-prune-" + strconv.Itoa(i),
		})
	}
	if _, _, err := repository.InsertUsageEvents(db, events); err != nil {
		t.Fatalf("insert usage events: %v", err)
	}

	client := &requestLogClientStub{result: &cpa.RequestLogResult{
		StatusCode: http.StatusOK,
		Filename:   "request.log",
		Body:       []byte("=== REQUEST INFO ===\n" + strings.Repeat("x", 5*1024*1024-64)),
	}}
	provider := service.NewRequestLogService(db, client)

	for eventID := int64(1); eventID <= int64(len(events)); eventID++ {
		if _, err := provider.GetUsageEventRequestLog(context.Background(), eventID); err != nil {
			t.Fatalf("load request log for event %d: %v", eventID, err)
		}
	}
	if client.calls != len(events) {
		t.Fatalf("expected initial CPA calls to match event count, got %d", client.calls)
	}

	if _, err := provider.GetUsageEventRequestLog(context.Background(), 1); err != nil {
		t.Fatalf("reload pruned request log: %v", err)
	}
	if client.calls != len(events)+1 {
		t.Fatalf("expected oldest byte-budget cache entry to be pruned and refetched, got %d calls", client.calls)
	}
}

func TestRequestLogServiceDownloadFetchesRawBody(t *testing.T) {
	db := openRequestLogTestDB(t)
	if _, _, err := repository.InsertUsageEvents(db, []entities.UsageEvent{{
		EventKey:  "event-download",
		RequestID: "req-download",
	}}); err != nil {
		t.Fatalf("insert usage event: %v", err)
	}

	client := &requestLogClientStub{downloadResult: &cpa.RequestLogStream{
		StatusCode:    http.StatusOK,
		Filename:      "download.log",
		ContentType:   "text/plain; charset=utf-8",
		ContentLength: 7,
		Body:          io.NopCloser(bytes.NewBufferString("raw log")),
	}}
	provider := service.NewRequestLogService(db, client)
	downloader, ok := provider.(interface {
		DownloadUsageEventRequestLog(context.Context, int64) (service.RequestLogDownload, error)
	})
	if !ok {
		t.Fatalf("request log provider does not support downloads")
	}

	download, err := downloader.DownloadUsageEventRequestLog(context.Background(), 1)
	if err != nil {
		t.Fatalf("DownloadUsageEventRequestLog returned error: %v", err)
	}
	body, err := io.ReadAll(download.Body)
	if err != nil {
		t.Fatalf("read download body: %v", err)
	}
	_ = download.Body.Close()
	if string(body) != "raw log" || download.Filename != "download.log" || download.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("unexpected download response: %+v", download)
	}
	if client.downloadCalls != 1 {
		t.Fatalf("expected one raw download call, got %d", client.downloadCalls)
	}
}

func openRequestLogTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := repository.OpenDatabase(config.Config{SQLitePath: filepath.Join(t.TempDir(), "request-log.db")})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	return db
}

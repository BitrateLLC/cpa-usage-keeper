package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-usage-keeper/internal/cpa"
	"cpa-usage-keeper/internal/repository"
	"gorm.io/gorm"
)

const (
	requestLogCacheTTL         = 10 * time.Minute
	requestLogNegativeCacheTTL = 10 * time.Second
	requestLogMaxBytes         = 5 * 1024 * 1024
	requestLogCacheMaxBytes    = 100 * 1024 * 1024
)

var (
	ErrRequestLogUnavailable = errors.New("request log unavailable")
	ErrRequestLogMissingID   = errors.New("usage event request id missing")
	ErrRequestLogTooLarge    = errors.New("request log too large")
)

type RequestLogClient interface {
	FetchRequestLogByID(ctx context.Context, requestID string) (*cpa.RequestLogResult, error)
	OpenRequestLogByID(ctx context.Context, requestID string) (*cpa.RequestLogStream, error)
}

type RequestLogProvider interface {
	GetUsageEventRequestLog(ctx context.Context, eventID int64) (RequestLogResponse, error)
	DownloadUsageEventRequestLog(ctx context.Context, eventID int64) (RequestLogDownload, error)
}

type RequestLogResponse struct {
	EventID      int64
	RequestID    string
	Filename     string
	Cached       bool
	Available    bool
	Previewable  bool
	TooLarge     bool
	Downloadable bool
	Sections     []RequestLogSection
	Raw          string
}

type RequestLogDownload struct {
	EventID      int64
	RequestID    string
	Filename     string
	ContentType  string
	Body         io.ReadCloser
	Downloadable bool
}

type RequestLogSection struct {
	Title   string
	Content string
}

type requestLogService struct {
	db     *gorm.DB
	client RequestLogClient
	now    func() time.Time

	mu            sync.Mutex
	cache         map[string]requestLogCacheEntry
	cacheSequence int64
	cacheBytes    int
}

type requestLogCacheEntry struct {
	response  RequestLogResponse
	err       error
	expiresAt time.Time
	createdAt time.Time
	sequence  int64
}

func NewRequestLogService(db *gorm.DB, client RequestLogClient) RequestLogProvider {
	return &requestLogService{
		db:     db,
		client: client,
		now:    time.Now,
		cache:  map[string]requestLogCacheEntry{},
	}
}

func (s *requestLogService) GetUsageEventRequestLog(ctx context.Context, eventID int64) (RequestLogResponse, error) {
	if s == nil {
		return RequestLogResponse{}, fmt.Errorf("request log service is nil")
	}
	if s.db == nil {
		return RequestLogResponse{}, fmt.Errorf("database is nil")
	}
	if s.client == nil {
		return RequestLogResponse{}, fmt.Errorf("request log client is not configured")
	}
	requestID, err := repository.FindUsageEventRequestIDByID(s.db.WithContext(ctx), eventID)
	if err != nil {
		return RequestLogResponse{}, err
	}
	if requestID == "" {
		return RequestLogResponse{EventID: eventID, Available: false}, ErrRequestLogMissingID
	}

	if cached, ok := s.getCached(requestID); ok {
		cached.response.EventID = eventID
		cached.response.Cached = true
		return cached.response, cached.err
	}

	result, err := s.client.FetchRequestLogByID(ctx, requestID)
	if err != nil {
		if result != nil && result.StatusCode == http.StatusNotFound {
			response := RequestLogResponse{EventID: eventID, RequestID: requestID, Available: false}
			s.setCached(requestID, response, ErrRequestLogUnavailable, requestLogNegativeCacheTTL)
			return response, ErrRequestLogUnavailable
		}
		return RequestLogResponse{}, err
	}
	if result == nil {
		return RequestLogResponse{}, fmt.Errorf("request log result is nil")
	}
	if result.BodyTruncated || len(result.Body) > requestLogMaxBytes {
		response := RequestLogResponse{
			EventID:      eventID,
			RequestID:    requestID,
			Filename:     strings.TrimSpace(result.Filename),
			Available:    true,
			Previewable:  false,
			TooLarge:     true,
			Downloadable: true,
		}
		s.setCached(requestID, response, nil, requestLogCacheTTL)
		return response, nil
	}

	raw := string(result.Body)
	response := RequestLogResponse{
		EventID:      eventID,
		RequestID:    requestID,
		Filename:     strings.TrimSpace(result.Filename),
		Available:    true,
		Previewable:  true,
		Downloadable: true,
		Sections:     ParseRequestLogSections(raw),
		Raw:          raw,
	}
	s.setCached(requestID, response, nil, requestLogCacheTTL)
	return response, nil
}

func (s *requestLogService) DownloadUsageEventRequestLog(ctx context.Context, eventID int64) (RequestLogDownload, error) {
	if s == nil {
		return RequestLogDownload{}, fmt.Errorf("request log service is nil")
	}
	if s.db == nil {
		return RequestLogDownload{}, fmt.Errorf("database is nil")
	}
	if s.client == nil {
		return RequestLogDownload{}, fmt.Errorf("request log client is not configured")
	}
	requestID, err := repository.FindUsageEventRequestIDByID(s.db.WithContext(ctx), eventID)
	if err != nil {
		return RequestLogDownload{}, err
	}
	if requestID == "" {
		return RequestLogDownload{EventID: eventID, Downloadable: false}, ErrRequestLogMissingID
	}
	result, err := s.client.OpenRequestLogByID(ctx, requestID)
	if err != nil {
		if result != nil && result.StatusCode == http.StatusNotFound {
			return RequestLogDownload{EventID: eventID, RequestID: requestID, Downloadable: false}, ErrRequestLogUnavailable
		}
		return RequestLogDownload{}, err
	}
	if result == nil {
		return RequestLogDownload{}, fmt.Errorf("request log result is nil")
	}
	return RequestLogDownload{
		EventID:      eventID,
		RequestID:    requestID,
		Filename:     strings.TrimSpace(result.Filename),
		ContentType:  strings.TrimSpace(result.ContentType),
		Body:         result.Body,
		Downloadable: true,
	}, nil
}

func (s *requestLogService) getCached(requestID string) (requestLogCacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[requestID]
	if !ok {
		return requestLogCacheEntry{}, false
	}
	if !s.now().Before(entry.expiresAt) {
		delete(s.cache, requestID)
		return requestLogCacheEntry{}, false
	}
	return entry, true
}

func (s *requestLogService) setCached(requestID string, response RequestLogResponse, err error, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.cacheSequence++
	if previous, ok := s.cache[requestID]; ok {
		s.cacheBytes -= requestLogCacheEntrySize(previous)
	}
	s.cache[requestID] = requestLogCacheEntry{
		response:  response,
		err:       err,
		expiresAt: now.Add(ttl),
		createdAt: now,
		sequence:  s.cacheSequence,
	}
	s.cacheBytes += requestLogCacheEntrySize(s.cache[requestID])
	s.pruneCacheLocked(now)
}

func (s *requestLogService) pruneCacheLocked(now time.Time) {
	for requestID, entry := range s.cache {
		if !now.Before(entry.expiresAt) {
			delete(s.cache, requestID)
			s.cacheBytes -= requestLogCacheEntrySize(entry)
		}
	}
	for s.cacheBytes > requestLogCacheMaxBytes {
		oldestRequestID := ""
		var oldestSequence int64
		for requestID, entry := range s.cache {
			if oldestRequestID == "" || entry.sequence < oldestSequence {
				oldestRequestID = requestID
				oldestSequence = entry.sequence
			}
		}
		if oldestRequestID == "" {
			return
		}
		entry := s.cache[oldestRequestID]
		delete(s.cache, oldestRequestID)
		s.cacheBytes -= requestLogCacheEntrySize(entry)
	}
}

func requestLogCacheEntrySize(entry requestLogCacheEntry) int {
	size := len(entry.response.Raw) + len(entry.response.Filename) + len(entry.response.RequestID)
	for _, section := range entry.response.Sections {
		size += len(section.Title) + len(section.Content)
	}
	return size
}

func RequestLogPreviewMaxBytes() int {
	return requestLogMaxBytes
}

func ParseRequestLogSections(raw string) []RequestLogSection {
	lines := strings.Split(raw, "\n")
	sections := make([]RequestLogSection, 0, 8)
	currentTitle := ""
	currentLines := []string{}
	flush := func() {
		if currentTitle == "" {
			return
		}
		sections = append(sections, RequestLogSection{
			Title:   currentTitle,
			Content: strings.TrimRight(strings.Join(currentLines, "\n"), "\n"),
		})
		currentLines = []string{}
	}
	for _, line := range lines {
		title, ok := parseRequestLogSectionTitle(line)
		if ok {
			flush()
			currentTitle = title
			continue
		}
		if currentTitle != "" {
			currentLines = append(currentLines, line)
		}
	}
	flush()
	if len(sections) == 0 && strings.TrimSpace(raw) != "" {
		return []RequestLogSection{{Title: "RAW LOG", Content: raw}}
	}
	return sections
}

func parseRequestLogSectionTitle(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "===") || !strings.HasSuffix(trimmed, "===") {
		return "", false
	}
	title := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "==="), "==="))
	if title == "" {
		return "", false
	}
	return title, true
}

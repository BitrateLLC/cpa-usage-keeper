package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/notify"
	"cpa-usage-keeper/internal/repository"
	"gorm.io/gorm"
)

// 渠道类型常量。
const (
	AccountGuardChannelTelegram = "telegram"
	AccountGuardChannelWebhook  = "webhook"
)

const (
	accountGuardDefaultIntervalSeconds = 900
	accountGuardMinIntervalSeconds     = 60
)

// 默认禁用码：401 认证失效 / 402 余额不足。
var accountGuardDefaultDisableCodes = []int{401, 402}

// AccountGuardRuntime 是解析后的运行时设置（含真实密钥与预构建的 Notifier），仅进程内使用，供 runner 无锁读取。
type AccountGuardRuntime struct {
	Enabled       bool
	DisableCodes  map[int]bool
	MonitorCodes  map[int]bool
	AlertInterval time.Duration
	Notifier      notify.Notifier
}

// accountGuardChannel 是渠道的存储形态（库内明文密钥）。
type accountGuardChannel struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Enabled          bool   `json:"enabled"`
	TelegramBotToken string `json:"telegram_bot_token,omitempty"`
	TelegramChatID   string `json:"telegram_chat_id,omitempty"`
	WebhookURL       string `json:"webhook_url,omitempty"`
}

// AccountGuardChannelDTO 是渠道的 API 出口形态：密钥掩码 + configured 标志。
type AccountGuardChannelDTO struct {
	ID                         string `json:"id"`
	Type                       string `json:"type"`
	Enabled                    bool   `json:"enabled"`
	TelegramBotToken           string `json:"telegram_bot_token"`
	TelegramBotTokenConfigured bool   `json:"telegram_bot_token_configured"`
	TelegramChatID             string `json:"telegram_chat_id"`
	WebhookURL                 string `json:"webhook_url"`
	WebhookURLConfigured       bool   `json:"webhook_url_configured"`
}

// AccountGuardSettingsDTO 是设置的 API 出口形态。
type AccountGuardSettingsDTO struct {
	Enabled              bool                     `json:"enabled"`
	DisableStatusCodes   []int                    `json:"disable_status_codes"`
	MonitorStatusCodes   []int                    `json:"monitor_status_codes"`
	AlertIntervalSeconds int                      `json:"alert_interval_seconds"`
	Channels             []AccountGuardChannelDTO `json:"channels"`
}

// AccountGuardChannelInput 是渠道的 API 入口形态；密钥字段留空表示沿用已存值（只写合并）。
type AccountGuardChannelInput struct {
	ID               string
	Type             string
	Enabled          bool
	TelegramBotToken string
	TelegramChatID   string
	WebhookURL       string
}

// UpdateAccountGuardSettingsInput 是整表替换的入参。
type UpdateAccountGuardSettingsInput struct {
	Enabled              bool
	DisableStatusCodes   []int
	MonitorStatusCodes   []int
	AlertIntervalSeconds int
	Channels             []AccountGuardChannelInput
}

// AccountGuardSettingsProvider 暴露给 API 层。
type AccountGuardSettingsProvider interface {
	Get(ctx context.Context) (AccountGuardSettingsDTO, error)
	Update(ctx context.Context, input UpdateAccountGuardSettingsInput) (AccountGuardSettingsDTO, error)
}

// AccountGuardSettingsService 持久化设置并维护一个原子运行时快照供 runner 热读取。
type AccountGuardSettingsService struct {
	db      *gorm.DB
	runtime atomic.Pointer[AccountGuardRuntime]
}

// NewAccountGuardSettingsService 构造服务并从 DB 加载一次快照；无行时退化为默认（未启用）。
func NewAccountGuardSettingsService(db *gorm.DB) *AccountGuardSettingsService {
	service := &AccountGuardSettingsService{db: db}
	setting, err := repository.GetAccountGuardSetting(context.Background(), db)
	if err != nil {
		// 无行（首启未 Seed）或读失败时，先用默认快照保证 runner 可运行。
		service.runtime.Store(defaultAccountGuardRuntime())
		return service
	}
	service.runtime.Store(buildAccountGuardRuntime(setting))
	return service
}

// Snapshot 返回当前运行时快照（永不为 nil）。
func (s *AccountGuardSettingsService) Snapshot() AccountGuardRuntime {
	rt := s.runtime.Load()
	if rt == nil {
		return *defaultAccountGuardRuntime()
	}
	return *rt
}

// Get 返回掩码后的设置 DTO；无行时返回默认 DTO。
func (s *AccountGuardSettingsService) Get(ctx context.Context) (AccountGuardSettingsDTO, error) {
	setting, err := repository.GetAccountGuardSetting(ctx, s.db)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return defaultAccountGuardSettingsDTO(), nil
		}
		return AccountGuardSettingsDTO{}, err
	}
	return settingToDTO(setting), nil
}

// Update 整表替换设置（密钥只写合并），存库后重建运行时快照并返回掩码 DTO。
func (s *AccountGuardSettingsService) Update(ctx context.Context, input UpdateAccountGuardSettingsInput) (AccountGuardSettingsDTO, error) {
	disableCodes, err := normalizeStatusCodes(input.DisableStatusCodes)
	if err != nil {
		return AccountGuardSettingsDTO{}, fmt.Errorf("disable_status_codes: %w", err)
	}
	monitorCodes, err := normalizeStatusCodes(input.MonitorStatusCodes)
	if err != nil {
		return AccountGuardSettingsDTO{}, fmt.Errorf("monitor_status_codes: %w", err)
	}
	interval := input.AlertIntervalSeconds
	if interval < accountGuardMinIntervalSeconds {
		return AccountGuardSettingsDTO{}, fmt.Errorf("alert_interval_seconds must be >= %d", accountGuardMinIntervalSeconds)
	}

	// 载入已存渠道，按 ID 建索引以支持密钥只写合并。
	existingByID := map[string]accountGuardChannel{}
	if current, err := repository.GetAccountGuardSetting(ctx, s.db); err == nil {
		for _, ch := range decodeChannels(current.Channels) {
			existingByID[ch.ID] = ch
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return AccountGuardSettingsDTO{}, err
	}

	channels, err := mergeChannels(input.Channels, existingByID)
	if err != nil {
		return AccountGuardSettingsDTO{}, err
	}

	setting := &entities.AccountGuardSetting{
		ID:                   entities.AccountGuardSettingSingletonID,
		Enabled:              input.Enabled,
		DisableStatusCodes:   encodeCodes(disableCodes),
		MonitorStatusCodes:   encodeCodes(monitorCodes),
		AlertIntervalSeconds: interval,
		Channels:             encodeChannels(channels),
	}
	if err := repository.SaveAccountGuardSetting(ctx, s.db, setting); err != nil {
		return AccountGuardSettingsDTO{}, err
	}
	s.runtime.Store(buildAccountGuardRuntime(setting))
	return settingToDTO(setting), nil
}

// Seed 在 DB 无设置行时，用 env 现值写入种子行；已有行则不覆盖。仅首启调用一次。
func (s *AccountGuardSettingsService) Seed(cfg config.Config) error {
	if _, err := repository.GetAccountGuardSetting(context.Background(), s.db); err == nil {
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	disableCodes := accountGuardDefaultDisableCodes
	if !cfg.AccountGuardDisableOn401 {
		disableCodes = []int{}
	}
	interval := int(cfg.AccountGuardAlertInterval / time.Second)
	if interval < accountGuardMinIntervalSeconds {
		interval = accountGuardDefaultIntervalSeconds
	}

	var channels []accountGuardChannel
	if token := strings.TrimSpace(cfg.AlertTelegramBotToken); token != "" && strings.TrimSpace(cfg.AlertTelegramChatID) != "" {
		channels = append(channels, accountGuardChannel{
			ID:               newChannelID(),
			Type:             AccountGuardChannelTelegram,
			Enabled:          true,
			TelegramBotToken: token,
			TelegramChatID:   strings.TrimSpace(cfg.AlertTelegramChatID),
		})
	}
	if url := strings.TrimSpace(cfg.AlertWebhookURL); url != "" {
		channels = append(channels, accountGuardChannel{
			ID:         newChannelID(),
			Type:       AccountGuardChannelWebhook,
			Enabled:    true,
			WebhookURL: url,
		})
	}

	setting := &entities.AccountGuardSetting{
		ID:                   entities.AccountGuardSettingSingletonID,
		Enabled:              cfg.AccountGuardEnabled,
		DisableStatusCodes:   encodeCodes(disableCodes),
		MonitorStatusCodes:   encodeCodes([]int{}),
		AlertIntervalSeconds: interval,
		Channels:             encodeChannels(channels),
	}
	if err := repository.SaveAccountGuardSetting(context.Background(), s.db, setting); err != nil {
		return err
	}
	s.runtime.Store(buildAccountGuardRuntime(setting))
	return nil
}

// mergeChannels 合并入参与已存渠道：分配缺失 ID、密钥只写合并、校验启用渠道完整性。
func mergeChannels(inputs []AccountGuardChannelInput, existingByID map[string]accountGuardChannel) ([]accountGuardChannel, error) {
	channels := make([]accountGuardChannel, 0, len(inputs))
	for index, in := range inputs {
		channelType := strings.ToLower(strings.TrimSpace(in.Type))
		if channelType != AccountGuardChannelTelegram && channelType != AccountGuardChannelWebhook {
			return nil, fmt.Errorf("channel %d: type must be telegram or webhook", index+1)
		}
		id := strings.TrimSpace(in.ID)
		prev, hasPrev := existingByID[id]
		if id == "" {
			id = newChannelID()
			hasPrev = false
		}

		ch := accountGuardChannel{ID: id, Type: channelType, Enabled: in.Enabled}
		switch channelType {
		case AccountGuardChannelTelegram:
			// token 留空=沿用已存值（只写合并）；chat_id 非密钥，始终采用入参。
			token := strings.TrimSpace(in.TelegramBotToken)
			if token == "" && hasPrev {
				token = prev.TelegramBotToken
			}
			ch.TelegramBotToken = token
			ch.TelegramChatID = strings.TrimSpace(in.TelegramChatID)
			if ch.Enabled && (ch.TelegramBotToken == "" || ch.TelegramChatID == "") {
				return nil, fmt.Errorf("channel %d: telegram requires bot token and chat id", index+1)
			}
		case AccountGuardChannelWebhook:
			url := strings.TrimSpace(in.WebhookURL)
			if url == "" && hasPrev {
				url = prev.WebhookURL
			}
			ch.WebhookURL = url
			if ch.Enabled && ch.WebhookURL == "" {
				return nil, fmt.Errorf("channel %d: webhook requires url", index+1)
			}
		}
		channels = append(channels, ch)
	}
	return channels, nil
}

// buildAccountGuardRuntime 把存储行解析成运行时快照（含动态构建的 Notifier）。
func buildAccountGuardRuntime(setting *entities.AccountGuardSetting) *AccountGuardRuntime {
	interval := time.Duration(setting.AlertIntervalSeconds) * time.Second
	if interval < accountGuardMinIntervalSeconds*time.Second {
		interval = accountGuardDefaultIntervalSeconds * time.Second
	}
	return &AccountGuardRuntime{
		Enabled:       setting.Enabled,
		DisableCodes:  codesToSet(decodeCodes(setting.DisableStatusCodes)),
		MonitorCodes:  codesToSet(decodeCodes(setting.MonitorStatusCodes)),
		AlertInterval: interval,
		Notifier:      buildNotifier(decodeChannels(setting.Channels)),
	}
}

func defaultAccountGuardRuntime() *AccountGuardRuntime {
	return &AccountGuardRuntime{
		Enabled:       false,
		DisableCodes:  codesToSet(accountGuardDefaultDisableCodes),
		MonitorCodes:  map[int]bool{},
		AlertInterval: accountGuardDefaultIntervalSeconds * time.Second,
		Notifier:      notify.NoopNotifier{},
	}
}

// buildNotifier 由启用渠道动态构建 MultiNotifier；缺参渠道被跳过，全空时退化为 Noop。
func buildNotifier(channels []accountGuardChannel) notify.Notifier {
	targets := make([]notify.Notifier, 0, len(channels))
	for _, ch := range channels {
		if !ch.Enabled {
			continue
		}
		switch ch.Type {
		case AccountGuardChannelTelegram:
			if n := notify.NewTelegramNotifier(notify.TelegramOptions{BotToken: ch.TelegramBotToken, ChatID: ch.TelegramChatID}); n != nil {
				targets = append(targets, n)
			}
		case AccountGuardChannelWebhook:
			if n := notify.NewWebhookNotifier(notify.WebhookOptions{URL: ch.WebhookURL}); n != nil {
				targets = append(targets, n)
			}
		}
	}
	multi := notify.NewMultiNotifier(targets...)
	if multi.Empty() {
		return notify.NoopNotifier{}
	}
	return multi
}

func settingToDTO(setting *entities.AccountGuardSetting) AccountGuardSettingsDTO {
	channels := decodeChannels(setting.Channels)
	dtoChannels := make([]AccountGuardChannelDTO, 0, len(channels))
	for _, ch := range channels {
		dtoChannels = append(dtoChannels, AccountGuardChannelDTO{
			ID:                         ch.ID,
			Type:                       ch.Type,
			Enabled:                    ch.Enabled,
			TelegramBotToken:           maskSecret(ch.TelegramBotToken),
			TelegramBotTokenConfigured: strings.TrimSpace(ch.TelegramBotToken) != "",
			TelegramChatID:             ch.TelegramChatID,
			WebhookURL:                 maskSecret(ch.WebhookURL),
			WebhookURLConfigured:       strings.TrimSpace(ch.WebhookURL) != "",
		})
	}
	return AccountGuardSettingsDTO{
		Enabled:              setting.Enabled,
		DisableStatusCodes:   decodeCodes(setting.DisableStatusCodes),
		MonitorStatusCodes:   decodeCodes(setting.MonitorStatusCodes),
		AlertIntervalSeconds: setting.AlertIntervalSeconds,
		Channels:             dtoChannels,
	}
}

func defaultAccountGuardSettingsDTO() AccountGuardSettingsDTO {
	return AccountGuardSettingsDTO{
		Enabled:              false,
		DisableStatusCodes:   append([]int(nil), accountGuardDefaultDisableCodes...),
		MonitorStatusCodes:   []int{},
		AlertIntervalSeconds: accountGuardDefaultIntervalSeconds,
		Channels:             []AccountGuardChannelDTO{},
	}
}

// normalizeStatusCodes 去重排序并校验范围 100–599。
func normalizeStatusCodes(codes []int) ([]int, error) {
	seen := map[int]struct{}{}
	out := make([]int, 0, len(codes))
	for _, code := range codes {
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("status code %d out of range (100-599)", code)
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	sort.Ints(out)
	return out, nil
}

func codesToSet(codes []int) map[int]bool {
	set := make(map[int]bool, len(codes))
	for _, code := range codes {
		set[code] = true
	}
	return set
}

func encodeCodes(codes []int) string {
	if codes == nil {
		codes = []int{}
	}
	data, err := json.Marshal(codes)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func decodeCodes(raw string) []int {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []int{}
	}
	var codes []int
	if err := json.Unmarshal([]byte(trimmed), &codes); err != nil {
		return []int{}
	}
	return codes
}

func encodeChannels(channels []accountGuardChannel) string {
	if channels == nil {
		channels = []accountGuardChannel{}
	}
	data, err := json.Marshal(channels)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func decodeChannels(raw string) []accountGuardChannel {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var channels []accountGuardChannel
	if err := json.Unmarshal([]byte(trimmed), &channels); err != nil {
		return nil
	}
	return channels
}

// maskSecret 保留尾 3 位、其余以 * 替换；空串原样返回。
func maskSecret(secret string) string {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return ""
	}
	runes := []rune(trimmed)
	if len(runes) <= 3 {
		return strings.Repeat("*", len(runes))
	}
	return strings.Repeat("*", len(runes)-3) + string(runes[len(runes)-3:])
}

func newChannelID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "ch-0"
	}
	return "ch-" + hex.EncodeToString(buf)
}

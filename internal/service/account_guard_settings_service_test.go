package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cpa-usage-keeper/internal/config"
	"cpa-usage-keeper/internal/repository"
	"gorm.io/gorm"
)

func setupAccountGuardDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := repository.OpenDatabase(config.Config{SQLitePath: filepath.Join(t.TempDir(), "account-guard.db")})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	return db
}

func TestAccountGuardSettingsUpdateWriteOnlyMerge(t *testing.T) {
	db := setupAccountGuardDB(t)
	svc := NewAccountGuardSettingsService(db)
	ctx := context.Background()

	// 首次写入：带完整 telegram 密钥。
	first, err := svc.Update(ctx, UpdateAccountGuardSettingsInput{
		Enabled:              true,
		DisableStatusCodes:   []int{401, 402},
		MonitorStatusCodes:   []int{429},
		AlertIntervalSeconds: 120,
		Channels: []AccountGuardChannelInput{{
			Type:             AccountGuardChannelTelegram,
			Enabled:          true,
			TelegramBotToken: "secret-token-XYZ",
			TelegramChatID:   "123",
		}},
	})
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if len(first.Channels) != 1 {
		t.Fatalf("expected 1 channel, got %d", len(first.Channels))
	}
	channelID := first.Channels[0].ID
	if first.Channels[0].TelegramBotToken == "secret-token-XYZ" {
		t.Fatalf("token should be masked in DTO, got plaintext")
	}
	if !first.Channels[0].TelegramBotTokenConfigured {
		t.Fatalf("token should be marked configured")
	}

	// 二次写入：token 留空（=保持原值），改 chat id。
	second, err := svc.Update(ctx, UpdateAccountGuardSettingsInput{
		Enabled:              true,
		DisableStatusCodes:   []int{401},
		AlertIntervalSeconds: 120,
		Channels: []AccountGuardChannelInput{{
			ID:             channelID,
			Type:           AccountGuardChannelTelegram,
			Enabled:        true,
			TelegramChatID: "456",
		}},
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if second.Channels[0].TelegramChatID != "456" {
		t.Fatalf("chat id should update to 456, got %s", second.Channels[0].TelegramChatID)
	}

	// 校验底层仍保留原 token：快照 Notifier 非 Noop 即说明 telegram 渠道完整可用。
	rt := svc.Snapshot()
	if _, ok := rt.Notifier.(interface{ Notify(context.Context, string) }); !ok {
		t.Fatalf("snapshot notifier should be usable")
	}
	if !rt.Enabled || !rt.DisableCodes[401] {
		t.Fatalf("snapshot should reflect latest settings")
	}

	// 直接读库确认 token 明文未丢失。
	stored, err := repository.GetAccountGuardSetting(ctx, db)
	if err != nil {
		t.Fatalf("get stored: %v", err)
	}
	channels := decodeChannels(stored.Channels)
	if len(channels) != 1 || channels[0].TelegramBotToken != "secret-token-XYZ" {
		t.Fatalf("stored token should be preserved, got %+v", channels)
	}
}

func TestAccountGuardSettingsUpdateValidatesEnabledChannel(t *testing.T) {
	db := setupAccountGuardDB(t)
	svc := NewAccountGuardSettingsService(db)
	_, err := svc.Update(context.Background(), UpdateAccountGuardSettingsInput{
		Enabled:              true,
		AlertIntervalSeconds: 120,
		Channels: []AccountGuardChannelInput{{
			Type:    AccountGuardChannelWebhook,
			Enabled: true,
			// 缺 URL
		}},
	})
	if err == nil {
		t.Fatal("expected validation error for enabled webhook without url")
	}
}

func TestAccountGuardSettingsUpdateRejectsShortInterval(t *testing.T) {
	db := setupAccountGuardDB(t)
	svc := NewAccountGuardSettingsService(db)
	_, err := svc.Update(context.Background(), UpdateAccountGuardSettingsInput{
		Enabled:              true,
		AlertIntervalSeconds: 30,
	})
	if err == nil {
		t.Fatal("expected error for interval < 60")
	}
}

func TestAccountGuardSettingsSeedFromEnvOnce(t *testing.T) {
	db := setupAccountGuardDB(t)
	svc := NewAccountGuardSettingsService(db)
	cfg := config.Config{
		AccountGuardEnabled:       true,
		AccountGuardDisableOn401:  true,
		AccountGuardAlertInterval: 5 * time.Minute,
		AlertWebhookURL:           "https://hook.example/abc",
	}
	if err := svc.Seed(cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled || got.AlertIntervalSeconds != 300 {
		t.Fatalf("seed should reflect env, got %+v", got)
	}
	if len(got.Channels) != 1 || got.Channels[0].Type != AccountGuardChannelWebhook {
		t.Fatalf("seed should create one webhook channel, got %+v", got.Channels)
	}

	// 二次 Seed 不应覆盖已有行。
	cfg.AccountGuardEnabled = false
	if err := svc.Seed(cfg); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	again, _ := svc.Get(context.Background())
	if !again.Enabled {
		t.Fatalf("second seed should not overwrite existing row")
	}
}

func TestMaskSecret(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"ab":               "**",
		"abcd":             "*bcd",
		"secret-token-XYZ": "*************XYZ",
	}
	for input, want := range cases {
		if got := maskSecret(input); got != want {
			t.Fatalf("maskSecret(%q)=%q want %q", input, got, want)
		}
	}
}

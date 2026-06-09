package api

import (
	"net/http"
	"strings"

	"cpa-usage-keeper/internal/service"
	"github.com/gin-gonic/gin"
)

type accountGuardChannelRequest struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Enabled          bool   `json:"enabled"`
	TelegramBotToken string `json:"telegram_bot_token"`
	TelegramChatID   string `json:"telegram_chat_id"`
	WebhookURL       string `json:"webhook_url"`
}

type updateAccountGuardSettingsRequest struct {
	Enabled              bool                         `json:"enabled"`
	DisableStatusCodes   []int                        `json:"disable_status_codes"`
	MonitorStatusCodes   []int                        `json:"monitor_status_codes"`
	AlertIntervalSeconds int                          `json:"alert_interval_seconds"`
	Channels             []accountGuardChannelRequest `json:"channels"`
}

func registerAccountGuardSettingsRoutes(router gin.IRoutes, provider service.AccountGuardSettingsProvider) {
	router.GET("/account-guard/settings", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "account guard settings provider is not configured"})
			return
		}
		settings, err := provider.Get(c.Request.Context())
		if err != nil {
			writeInternalError(c, "load account guard settings failed", err)
			return
		}
		c.JSON(http.StatusOK, settings)
	})

	router.PUT("/account-guard/settings", func(c *gin.Context) {
		if provider == nil {
			c.JSON(http.StatusNotImplemented, gin.H{"error": "account guard settings provider is not configured"})
			return
		}
		var request updateAccountGuardSettingsRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		channels := make([]service.AccountGuardChannelInput, 0, len(request.Channels))
		for _, ch := range request.Channels {
			channels = append(channels, service.AccountGuardChannelInput{
				ID:               ch.ID,
				Type:             ch.Type,
				Enabled:          ch.Enabled,
				TelegramBotToken: ch.TelegramBotToken,
				TelegramChatID:   ch.TelegramChatID,
				WebhookURL:       ch.WebhookURL,
			})
		}
		settings, err := provider.Update(c.Request.Context(), service.UpdateAccountGuardSettingsInput{
			Enabled:              request.Enabled,
			DisableStatusCodes:   request.DisableStatusCodes,
			MonitorStatusCodes:   request.MonitorStatusCodes,
			AlertIntervalSeconds: request.AlertIntervalSeconds,
			Channels:             channels,
		})
		if err != nil {
			if isAccountGuardValidationError(err) {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			writeInternalError(c, "update account guard settings failed", err)
			return
		}
		c.JSON(http.StatusOK, settings)
	})
}

// isAccountGuardValidationError 判断是否为入参校验类错误（应回 400 而非 500）。
func isAccountGuardValidationError(err error) bool {
	message := err.Error()
	for _, marker := range []string{"must", "requires", "range", "type must", "out of range"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

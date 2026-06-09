package entities

import "time"

// AccountGuardSettingSingletonID 是号池守护设置的固定单例行主键。
const AccountGuardSettingSingletonID int64 = 1

// AccountGuardSetting 是号池守护的可在网页编辑、运行时热生效的设置（单例行 id=1）。
// 状态码与渠道以 JSON 文本列存储，渠道列含密钥明文，仅经 API 出口时掩码。
type AccountGuardSetting struct {
	ID                   int64     `gorm:"primaryKey"`
	Enabled              bool      `gorm:"not null;default:false"`
	DisableStatusCodes   string    `gorm:"not null;default:'[401,402]'"`
	MonitorStatusCodes   string    `gorm:"not null;default:'[]'"`
	AlertIntervalSeconds int       `gorm:"not null;default:900"`
	Channels             string    `gorm:"not null;default:'[]'"`
	CreatedAt            time.Time `gorm:"serializer:storageTime"`
	UpdatedAt            time.Time `gorm:"serializer:storageTime"`
}

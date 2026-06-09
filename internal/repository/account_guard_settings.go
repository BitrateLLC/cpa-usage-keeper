package repository

import (
	"context"
	"fmt"

	"cpa-usage-keeper/internal/entities"
	"gorm.io/gorm"
)

// GetAccountGuardSetting 读取号池守护设置单例行（id=1）；不存在时返回 gorm.ErrRecordNotFound。
func GetAccountGuardSetting(ctx context.Context, db *gorm.DB) (*entities.AccountGuardSetting, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	var setting entities.AccountGuardSetting
	if err := db.WithContext(ctx).Where("id = ?", entities.AccountGuardSettingSingletonID).First(&setting).Error; err != nil {
		return nil, err
	}
	return &setting, nil
}

// SaveAccountGuardSetting 以固定单例主键 upsert 号池守护设置。
func SaveAccountGuardSetting(ctx context.Context, db *gorm.DB, setting *entities.AccountGuardSetting) error {
	if db == nil {
		return fmt.Errorf("database is nil")
	}
	if setting == nil {
		return fmt.Errorf("account guard setting is nil")
	}
	setting.ID = entities.AccountGuardSettingSingletonID
	if err := db.WithContext(ctx).Save(setting).Error; err != nil {
		return fmt.Errorf("save account guard setting: %w", err)
	}
	return nil
}

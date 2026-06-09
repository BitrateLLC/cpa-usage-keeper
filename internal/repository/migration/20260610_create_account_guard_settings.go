package migration

import (
	"cpa-usage-keeper/internal/entities"

	"gorm.io/gorm"
)

func createAccountGuardSettingsMigration(tx *gorm.DB) error {
	return tx.AutoMigrate(&entities.AccountGuardSetting{})
}

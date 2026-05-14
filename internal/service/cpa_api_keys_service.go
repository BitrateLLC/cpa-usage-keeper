package service

import (
	"context"
	"errors"

	"cpa-usage-keeper/internal/entities"
	"cpa-usage-keeper/internal/repository"

	"gorm.io/gorm"
)

var ErrInvalidID = errors.New("invalid id")

type CPAAPIKeyProvider interface {
	ListCPAAPIKeys(ctx context.Context) ([]entities.CPAAPIKey, error)
	UpdateCPAAPIKeyAlias(ctx context.Context, id int64, keyAlias string) (entities.CPAAPIKey, error)
}

type cpaAPIKeyService struct {
	db *gorm.DB
}

func NewCPAAPIKeyService(db *gorm.DB) CPAAPIKeyProvider {
	return &cpaAPIKeyService{db: db}
}

func (s *cpaAPIKeyService) ListCPAAPIKeys(context.Context) ([]entities.CPAAPIKey, error) {
	return repository.ListActiveCPAAPIKeys(s.db)
}

func (s *cpaAPIKeyService) UpdateCPAAPIKeyAlias(_ context.Context, id int64, keyAlias string) (entities.CPAAPIKey, error) {
	if id <= 0 {
		return entities.CPAAPIKey{}, ErrInvalidID
	}
	if err := repository.UpdateCPAAPIKeyAlias(s.db, id, keyAlias); err != nil {
		return entities.CPAAPIKey{}, err
	}
	return repository.FindActiveCPAAPIKeyByID(s.db, id)
}

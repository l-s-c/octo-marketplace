package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// AdminGet returns a single public skill by ID without Space restriction.
func (s *Service) AdminGet(ctx context.Context, id string) (*SkillItem, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil || row.Visibility != "public" {
		return nil, ErrNotFound
	}
	item := s.rowToItem(ctx, row)
	return &item, nil
}

// AdminGetSkillMD retrieves the SKILL.md content for a public skill.
func (s *Service) AdminGetSkillMD(ctx context.Context, id string) ([]byte, error) {
	row, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil || row.Visibility != "public" {
		return nil, ErrNotFound
	}

	// Parse VersionStorage to get skill_md_object_key
	if row.VersionStorage == "" {
		return nil, ErrNoFile
	}
	var vs model.VersionStorage
	if err := json.Unmarshal([]byte(row.VersionStorage), &vs); err != nil {
		return nil, ErrNoFile
	}
	if vs.SkillMdObjectKey == "" {
		return nil, ErrNoFile
	}

	reader, err := s.store.GetObject(ctx, vs.SkillMdObjectKey)
	if err != nil {
		return nil, fmt.Errorf("get skill md: %w", err)
	}
	defer reader.Close()

	data, err := readLimited(reader, maxSkillMDReadBytes)
	if err != nil {
		return nil, fmt.Errorf("read skill md: %w", err)
	}
	return data, nil
}

// AdminGetDownloadInfo resolves the artifact download URL for a public skill (no space/user check).
func (s *Service) AdminGetDownloadInfo(ctx context.Context, id string) (*DownloadInfo, error) {
	item, err := s.AdminGet(ctx, id)
	if err != nil {
		return nil, err
	}
	if item.FileURL == "" {
		return nil, ErrNoFile
	}
	url, err := s.store.PresignGet(ctx, item.FileURL, 1*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("presign download: %w", err)
	}
	return &DownloadInfo{
		DownloadURL: url,
		FileSHA256:  item.FileSHA256,
	}, nil
}

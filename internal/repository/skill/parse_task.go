package skill

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// ParseTaskRow holds parse_task data needed for skill creation.
type ParseTaskRow struct {
	ID                string
	UploadID          string
	FileName          string
	FileSize          int64
	FileURL           string
	FileSHA256        string
	Status            string
	ResultName        string
	ResultDescription *string
	ResultVersion     string
	ResultTags        json.RawMessage
	ResultReadme      *string
	ResultID          string
	ResultForkedFrom  string
	ResultMetadata    json.RawMessage
	Attempts          int
	OwnerID           string
	SpaceID           string
	SkillID           string
}

// GetParseTask retrieves a parse task by ID.
func (r *Repo) GetParseTask(ctx context.Context, id string) (*ParseTaskRow, error) {
	query := `
		SELECT id, upload_id, file_name, file_size, file_url, file_sha256, status,
			result_name, result_description, result_version, result_tags, result_readme,
			result_id, result_forked_from, result_metadata, attempts,
			owner_id, space_id, skill_id
		FROM parse_tasks
		WHERE id = ?
	`
	var pt ParseTaskRow
	// result_* columns are NULL until a parse succeeds (and stay NULL on
	// failed tasks), so every one of them must scan through a null-safe
	// carrier — import must see an unfinished task, not a 500.
	var resultName, resultVersion, resultID, resultForkedFrom, resultMetadata sql.NullString
	var resultTags []byte
	err := r.db.QueryRowContext(ctx, query, id).Scan(
		&pt.ID, &pt.UploadID, &pt.FileName, &pt.FileSize, &pt.FileURL, &pt.FileSHA256,
		&pt.Status, &resultName, &pt.ResultDescription, &resultVersion,
		&resultTags, &pt.ResultReadme,
		&resultID, &resultForkedFrom, &resultMetadata, &pt.Attempts,
		&pt.OwnerID, &pt.SpaceID, &pt.SkillID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pt.ResultName, pt.ResultVersion = resultName.String, resultVersion.String
	pt.ResultID, pt.ResultForkedFrom = resultID.String, resultForkedFrom.String
	if len(resultTags) > 0 {
		pt.ResultTags = json.RawMessage(resultTags)
	}
	if resultMetadata.Valid {
		pt.ResultMetadata = json.RawMessage(resultMetadata.String)
	}
	return &pt, nil
}

// MarkParseTaskConsumed marks a parse task as consumed (changes status to prevent reuse).
// It uses a conditional update with status='success' and ownership checks to prevent
// duplicate consumption and race conditions. Returns ErrParseTaskAlreadyConsumed if
// the task was already consumed or doesn't match the criteria.
func (r *Repo) MarkParseTaskConsumed(ctx context.Context, id, ownerID, spaceID, skillID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE parse_tasks SET status = 'consumed'
		 WHERE id = ? AND status = 'success' AND owner_id = ? AND space_id = ? AND skill_id = ?`,
		id, ownerID, spaceID, skillID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrParseTaskAlreadyConsumed
	}
	return nil
}

// ReleaseConsumedParseTask returns a consumed task to success so a failed
// downstream import (which consumes the task before writing its own rows) can
// be retried. Best-effort compensation: releasing an already-released task is
// a no-op.
func (r *Repo) ReleaseConsumedParseTask(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE parse_tasks SET status = 'success' WHERE id = ? AND status = 'consumed'`, id)
	return err
}

// UpdateSkillAndConsumeTask updates a skill, inserts a new version record,
// and marks the parse task as consumed within a single transaction, preventing
// duplicate reupload consumption.
func (r *Repo) UpdateSkillAndConsumeTask(ctx context.Context, skillID string, p UpdateParams, parseTaskID, ownerID, spaceID, taskSkillID string, ver *model.SkillVersion) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Consume parse task first (acts as a lock)
	res, err := tx.ExecContext(ctx,
		`UPDATE parse_tasks SET status = 'consumed'
		 WHERE id = ? AND status = 'success' AND owner_id = ? AND space_id = ? AND skill_id = ?`,
		parseTaskID, ownerID, spaceID, taskSkillID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrParseTaskAlreadyConsumed
	}

	if p.Tags != nil {
		tagIDs, err := resolveOrCreateTagIDs(ctx, tx, spaceID, ownerID, p.TagNames)
		if err != nil {
			return err
		}
		tags, err := tagIDsToRaw(tagIDs)
		if err != nil {
			return err
		}
		p.Tags = tags
	}

	// Build and execute the skill update
	sets, args := buildUpdateSets(p)
	if len(sets) == 0 {
		return tx.Commit()
	}

	query := "UPDATE skills SET " + joinStrings(sets, ", ") + " WHERE id = ? AND owner_id = ? AND space_id = ? AND is_deleted = 0"
	args = append(args, skillID, ownerID, spaceID)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return mapDuplicateName(err)
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrSkillNotFound
	}

	// Insert the new version record in the same transaction
	if ver != nil {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO skill_versions (id, skill_id, version, changelog, storage, changed_by)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			ver.ID, ver.SkillID, ver.Version, ver.Changelog, ver.Storage, ver.ChangedBy,
		)
		if err != nil {
			return fmt.Errorf("insert version: %w", err)
		}
	}

	return tx.Commit()
}

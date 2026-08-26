package skill

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Failed or still-pending parse tasks carry NULL result_* columns; GetParseTask
// must surface them as zero values so import can answer 400 invalid-task
// instead of a 500 scan error.
func TestGetParseTaskToleratesNullResultColumns(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	columns := []string{"id", "upload_id", "file_name", "file_size", "file_url", "file_sha256", "status",
		"result_name", "result_description", "result_version", "result_tags", "result_readme",
		"result_id", "result_forked_from", "result_metadata", "attempts",
		"owner_id", "space_id", "skill_id"}
	mock.ExpectQuery(`SELECT id, upload_id, .* FROM parse_tasks`).
		WillReturnRows(sqlmock.NewRows(columns).
			AddRow("task-1", "upload-1", "f.zip", int64(10), "tmp/f.zip", "abc", "failed",
				nil, nil, nil, nil, nil,
				nil, nil, nil, 1,
				"user-1", "space-a", ""))
	task, err := New(db).GetParseTask(context.Background(), "task-1")
	if err != nil {
		t.Fatalf("GetParseTask: %v", err)
	}
	if task == nil || task.Status != "failed" || task.ResultName != "" || task.ResultTags != nil {
		t.Fatalf("task = %#v", task)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

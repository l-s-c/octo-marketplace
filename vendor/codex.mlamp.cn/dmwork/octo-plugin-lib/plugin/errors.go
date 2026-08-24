package plugin

import "fmt"

type ErrorCode string

const (
	CodeInvalidJSON              ErrorCode = "invalid_json"
	CodeInvalidField             ErrorCode = "invalid_field"
	CodeInvalidPluginType        ErrorCode = "invalid_plugin_type"
	CodeInvalidRelationType      ErrorCode = "invalid_relation_type"
	CodeInvalidRelation          ErrorCode = "invalid_relation"
	CodeInvalidAttachmentPath    ErrorCode = "invalid_attachment_path"
	CodeDuplicateAttachmentPath  ErrorCode = "duplicate_attachment_path"
	CodeInvalidAttachmentContent ErrorCode = "invalid_attachment_content"
	CodeHashMismatch             ErrorCode = "hash_mismatch"
)

// ValidationError exposes a stable machine code and JSON path. Message is
// diagnostic text and must not be used for program flow.
type ValidationError struct {
	Code    ErrorCode `json:"code"`
	Path    string    `json:"path,omitempty"`
	Message string    `json:"message"`
}

func (err *ValidationError) Error() string {
	if err.Path == "" {
		return fmt.Sprintf("%s: %s", err.Code, err.Message)
	}
	return fmt.Sprintf("%s at %s: %s", err.Code, err.Path, err.Message)
}

func invalid(code ErrorCode, path, message string) error {
	return &ValidationError{Code: code, Path: path, Message: message}
}

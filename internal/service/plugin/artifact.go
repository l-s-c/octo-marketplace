package plugin

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/storage"
)

const (
	defaultMaxAttachmentBytes = int64(20 << 20)
	defaultMaxArchiveBytes    = int64(100 << 20)
	defaultMaxArchiveFiles    = 256
	attachmentPresignTTL      = time.Hour
)

var safeObjectSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// AttachmentUpload is a server-generated upload target. ObjectKey is stable;
// UploadURL and Headers are short-lived and must never be persisted in Package.
type AttachmentUpload struct {
	ObjectKey string
	UploadURL string
	Headers   http.Header
	ExpiresIn int
}

// AttachmentDownload is an authorized object stream and its package metadata.
type AttachmentDownload struct {
	Body        io.ReadCloser
	Path        string
	ContentType string
	Size        int64
}

// Archive is a validated, size-bounded Plugin package ready to be serialized.
type Archive struct {
	files []archiveFile
}

type archiveFile struct {
	path        string
	contentType string
	raw         []byte
	objectKey   string
	size        int64
}

type packageDocument struct {
	Schema      string            `json:"$schema"`
	Attachments []json.RawMessage `json:"attachments"`
}

type packageAttachment struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	MIMEType    string `json:"mime_type"`
	RawContent  string `json:"raw_content"`
	Content     string `json:"content"`
	ObjectKey   string `json:"object_key"`
	StorageURI  string `json:"storage_uri"`
	ContentSize *int64 `json:"content_size"`
}

// InitAttachmentUpload creates a key beneath the authenticated caller's Space.
// No caller-provided bucket, URI, prefix, or key is accepted.
func (s *Service) InitAttachmentUpload(ctx context.Context, caller Caller, fileName, contentType string, size int64) (*AttachmentUpload, error) {
	if validateCaller(caller) != nil || s.storage == nil || size <= 0 {
		return nil, ErrInvalidRequest
	}
	if size > s.maxAttachmentBytes {
		return nil, ErrTooLarge
	}
	if !safeObjectSegment.MatchString(caller.SpaceID) {
		return nil, ErrInvalidRequest
	}
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if !validMIMEType(contentType) {
		return nil, ErrInvalidRequest
	}
	ext := strings.ToLower(path.Ext(strings.TrimSpace(fileName)))
	if len(ext) > 16 || !regexp.MustCompile(`^\.[a-z0-9]+$`).MatchString(ext) {
		ext = ""
	}
	key := approvedAttachmentPrefix(caller.SpaceID) + s.id() + ext
	url, headers, err := s.storage.PresignPut(ctx, key, contentType, attachmentPresignTTL)
	if err != nil {
		return nil, fmt.Errorf("initialize plugin attachment upload: %w", err)
	}
	return &AttachmentUpload{ObjectKey: key, UploadURL: url, Headers: headers, ExpiresIn: int(attachmentPresignTTL.Seconds())}, nil
}

// OpenAttachment verifies Plugin visibility, exact Package reference, approved
// Space prefix, and stored size before opening bytes.
func (s *Service) OpenAttachment(ctx context.Context, caller Caller, pluginID, objectKey string) (*AttachmentDownload, error) {
	if validateCaller(caller) != nil || s.storage == nil || strings.TrimSpace(objectKey) == "" {
		return nil, ErrInvalidRequest
	}
	storageID, expectedType, err := parseWirePluginID(pluginID)
	if err != nil {
		return nil, err
	}
	p, _, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if p.Type != expectedType {
		return nil, ErrNotFound
	}
	files, err := s.parseArchiveFiles(ctx, p.Package, p.SpaceID)
	if err != nil {
		return nil, err
	}
	objectKey = strings.TrimSpace(objectKey)
	for _, file := range files {
		if file.objectKey != objectKey {
			continue
		}
		body, err := s.storage.GetObject(ctx, objectKey)
		if err != nil {
			return nil, fmt.Errorf("open plugin attachment: %w", err)
		}
		return &AttachmentDownload{Body: body, Path: file.path, ContentType: file.contentType, Size: file.size}, nil
	}
	return nil, ErrNotFound
}

// PrepareArchive resolves the current package, or the requested immutable
// version, and validates every entry before the handler sends response headers.
func (s *Service) PrepareArchive(ctx context.Context, caller Caller, pluginID, version string) (*Archive, error) {
	if validateCaller(caller) != nil || s.storage == nil {
		return nil, ErrInvalidRequest
	}
	storageID, expectedType, err := parseWirePluginID(pluginID)
	if err != nil {
		return nil, err
	}
	p, _, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if p.Type != expectedType {
		return nil, ErrNotFound
	}
	pkg := p.Package
	if strings.TrimSpace(version) != "" {
		if !validVersion(version) {
			return nil, ErrInvalidRequest
		}
		v, err := s.repo.GetVersion(ctx, scope(caller), p.ID, strings.TrimSpace(version))
		if err != nil {
			return nil, mapStoreError(err)
		}
		pkg = v.Package
	}
	files, err := s.parseArchiveFiles(ctx, pkg, p.SpaceID)
	if err != nil {
		return nil, err
	}
	return &Archive{files: files}, nil
}

// WriteArchive streams a prepared deterministic zip without exposing storage to
// the HTTP layer.
func (s *Service) WriteArchive(ctx context.Context, archive *Archive, dst io.Writer) error {
	if archive == nil || s.storage == nil {
		return ErrInvalidRequest
	}
	return archive.writeTo(ctx, s.storage, dst)
}

func (a *Archive) writeTo(ctx context.Context, store storage.Storage, dst io.Writer) error {
	zw := zip.NewWriter(dst)
	for _, file := range a.files {
		if err := ctx.Err(); err != nil {
			_ = zw.Close()
			return err
		}
		header := &zip.FileHeader{Name: file.path, Method: zip.Deflate}
		header.SetMode(0o644)
		header.Modified = time.Unix(0, 0).UTC()
		writer, err := zw.CreateHeader(header)
		if err != nil {
			_ = zw.Close()
			return err
		}
		if file.objectKey == "" {
			if _, err := writer.Write(file.raw); err != nil {
				_ = zw.Close()
				return err
			}
			continue
		}
		body, err := store.GetObject(ctx, file.objectKey)
		if err != nil {
			_ = zw.Close()
			return fmt.Errorf("read plugin archive attachment: %w", err)
		}
		_, copyErr := io.CopyN(writer, body, file.size)
		extra := make([]byte, 1)
		extraN, extraErr := body.Read(extra)
		closeErr := body.Close()
		if copyErr != nil || extraN != 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF)) || closeErr != nil {
			_ = zw.Close()
			return errors.New("plugin archive attachment changed while streaming")
		}
	}
	return zw.Close()
}

func (s *Service) parseArchiveFiles(ctx context.Context, raw json.RawMessage, spaceID *string) ([]archiveFile, error) {
	if spaceID == nil || !safeObjectSegment.MatchString(*spaceID) || len(raw) == 0 || len(raw) > maxJSONBytes {
		return nil, ErrInvalidRequest
	}
	var doc packageDocument
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Schema != pluginPackageSchema || len(doc.Attachments) > s.maxArchiveFiles {
		return nil, ErrInvalidRequest
	}
	files := make([]archiveFile, 0, len(doc.Attachments))
	seen := make(map[string]struct{}, len(doc.Attachments))
	var total int64
	for _, rawAttachment := range doc.Attachments {
		var generic map[string]any
		if json.Unmarshal(rawAttachment, &generic) != nil || hasSymlinkConcept(generic) {
			return nil, ErrInvalidRequest
		}
		var attachment packageAttachment
		if json.Unmarshal(rawAttachment, &attachment) != nil {
			return nil, ErrInvalidRequest
		}
		name, ok := normalizedArchivePath(attachment.Path)
		if !ok {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, ErrInvalidRequest
		}
		seen[name] = struct{}{}
		mimeType := strings.TrimSpace(attachment.MIMEType)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		if !validMIMEType(mimeType) {
			return nil, ErrInvalidRequest
		}
		file := archiveFile{path: name, contentType: mimeType}
		switch strings.ToLower(strings.TrimSpace(attachment.ContentType)) {
		case "raw", "inline":
			if attachment.RawContent != "" && attachment.Content != "" {
				return nil, ErrInvalidRequest
			}
			content := attachment.RawContent
			if attachment.Content != "" {
				content = attachment.Content
			}
			if !utf8.ValidString(content) {
				return nil, ErrInvalidRequest
			}
			file.raw, file.size = []byte(content), int64(len(content))
		case "storage":
			key := strings.TrimSpace(attachment.ObjectKey)
			if key == "" {
				key = strings.TrimSpace(attachment.StorageURI)
			}
			if !validReferencedObjectKey(key, *spaceID) {
				return nil, ErrInvalidRequest
			}
			info, err := s.storage.StatObject(ctx, key)
			if err != nil {
				return nil, fmt.Errorf("stat plugin archive attachment: %w", err)
			}
			file.objectKey, file.size = key, info.Size
		default:
			return nil, ErrInvalidRequest
		}
		if file.size < 0 || (attachment.ContentSize != nil && *attachment.ContentSize != file.size) {
			return nil, ErrInvalidRequest
		}
		if file.size > s.maxAttachmentBytes || total > s.maxArchiveBytes-file.size {
			return nil, ErrTooLarge
		}
		total += file.size
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func approvedAttachmentPrefix(spaceID string) string {
	return "plugins/" + spaceID + "/attachments/"
}

func validReferencedObjectKey(key, spaceID string) bool {
	prefix := approvedAttachmentPrefix(spaceID)
	if key == "" || strings.ContainsAny(key, "\\\x00?#") || strings.Contains(key, "://") || strings.HasPrefix(key, "/") || !strings.HasPrefix(key, prefix) {
		return false
	}
	clean := path.Clean(key)
	return clean == key && strings.HasPrefix(clean, prefix) && len(strings.TrimPrefix(clean, prefix)) > 0
}

func normalizedArchivePath(name string) (string, bool) {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return "", false
	}
	clean := path.Clean(name)
	if clean != name || clean == "." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return clean, true
}

func hasSymlinkConcept(value map[string]any) bool {
	for key, child := range value {
		normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
		switch normalized {
		case "symlink", "is_symlink", "link_target", "symlink_target":
			return true
		}
		if normalized == "type" {
			if text, ok := child.(string); ok && strings.Contains(strings.ToLower(text), "link") {
				return true
			}
		}
	}
	return false
}

func validMIMEType(value string) bool {
	if strings.ContainsAny(value, "\r\n\x00") || len(value) > 255 {
		return false
	}
	_, _, err := mime.ParseMediaType(value)
	return err == nil
}

// Compile-time guard for the version repository method used by artifacts.
var _ interface {
	GetVersion(context.Context, pluginrepo.Scope, string, string) (*model.PluginVersion, error)
} = (*pluginrepo.Repo)(nil)

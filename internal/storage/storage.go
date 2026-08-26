package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// ErrObjectNotFound is returned by StatObject when the object is genuinely
// absent, distinct from a transient/permission error. Callers that decide "safe
// to overwrite / genuinely new" must key on this rather than any stat error, so
// a throttle or 5xx is never mistaken for absence.
var ErrObjectNotFound = errors.New("storage: object not found")

// ObjectInfo describes stored object metadata needed before ingestion.
type ObjectInfo struct {
	Size int64
}

// Storage defines the object storage interface for skill file uploads.
type Storage interface {
	// PresignPut generates a presigned PUT URL for uploading an object.
	PresignPut(ctx context.Context, key string, contentType string, expires time.Duration) (url string, headers http.Header, err error)

	// PresignGet returns a public object URL when configured, otherwise a presigned GET URL.
	PresignGet(ctx context.Context, key string, expires time.Duration) (url string, err error)

	// PublicURL returns a non-expiring URL that clients can persist and
	// reload later. Local driver: an unsigned path served by the local
	// proxy; OSS driver: the object address on the public endpoint (bucket
	// must be readable there). Distinct from PresignGet, which produces
	// time-limited signed URLs unsuitable for storing in a DB column.
	PublicURL(ctx context.Context, key string) (string, error)

	// GetObject retrieves an object from storage.
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)

	// StatObject retrieves object metadata without downloading the body.
	StatObject(ctx context.Context, key string) (ObjectInfo, error)

	// PutObject uploads an object to storage from a reader.
	PutObject(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error

	// DeleteObject removes an object from storage.
	DeleteObject(ctx context.Context, key string) error

	// CopyObject copies an object from src to dst key. Used to relocate
	// uploaded files from temporary paths to permanent skill paths.
	CopyObject(ctx context.Context, srcKey, dstKey string) error
}

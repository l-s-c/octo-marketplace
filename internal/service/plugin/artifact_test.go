package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/storage"
)

type artifactStorage struct {
	objects  map[string][]byte
	putKey   string
	putType  string
	getCalls []string
	err      error
}

func (s *artifactStorage) PresignPut(_ context.Context, key, contentType string, _ time.Duration) (string, http.Header, error) {
	s.putKey, s.putType = key, contentType
	if s.err != nil {
		return "", nil, s.err
	}
	return "https://upload.invalid/signed", http.Header{"Content-Type": []string{contentType}}, nil
}
func (s *artifactStorage) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", errors.New("unexpected PresignGet")
}
func (s *artifactStorage) PublicURL(context.Context, string) (string, error) {
	return "", errors.New("unexpected PublicURL")
}
func (s *artifactStorage) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	s.getCalls = append(s.getCalls, key)
	if s.err != nil {
		return nil, s.err
	}
	value, ok := s.objects[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return io.NopCloser(bytes.NewReader(value)), nil
}
func (s *artifactStorage) StatObject(_ context.Context, key string) (storage.ObjectInfo, error) {
	if s.err != nil {
		return storage.ObjectInfo{}, s.err
	}
	value, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, errors.New("missing")
	}
	return storage.ObjectInfo{Size: int64(len(value))}, nil
}
func (s *artifactStorage) PutObject(context.Context, string, io.Reader, int64, string) error {
	return errors.New("unexpected PutObject")
}
func (s *artifactStorage) DeleteObject(context.Context, string) error {
	return errors.New("unexpected DeleteObject")
}
func (s *artifactStorage) CopyObject(context.Context, string, string) error {
	return errors.New("unexpected CopyObject")
}

func designPackage(body string) string {
	if strings.HasPrefix(body, `{"$schema":`) {
		return body
	}
	return strings.Replace(body, `{"attachments":`, `{"$schema":"cowork-plugin-package-1.0.json","attachments":`, 1)
}

func artifactService(pkg string, store *artifactStorage) *Service {
	pkg = designPackage(pkg)
	space := testCaller.SpaceID
	repo := &fakeStore{plugins: map[string]*model.Plugin{
		"plugin-1": {ID: "plugin-1", SpaceID: &space, Package: []byte(pkg)},
	}}
	return New(repo, store).WithRuntime(func() string { return "upload-id" }, nil)
}

func TestInitAttachmentUploadUsesServerGeneratedSpaceKey(t *testing.T) {
	store := &artifactStorage{}
	svc := artifactService(`{"attachments":[]}`, store)
	result, err := svc.InitAttachmentUpload(context.Background(), testCaller, "../../payload.JSON", "application/json", 12)
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectKey != "plugins/space-a/attachments/upload-id.json" || store.putKey != result.ObjectKey {
		t.Fatalf("result=%#v key=%q", result, store.putKey)
	}
	if strings.Contains(result.ObjectKey, "..") || store.putType != "application/json" || result.UploadURL == "" {
		t.Fatalf("unsafe or incomplete upload result: %#v", result)
	}
}

func TestInitAttachmentUploadRejectsOversizeAndMalformedMIME(t *testing.T) {
	store := &artifactStorage{}
	svc := artifactService(`{"attachments":[]}`, store)
	svc.SetArtifactLimits(10)
	if _, err := svc.InitAttachmentUpload(context.Background(), testCaller, "x.bin", "application/octet-stream", 11); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize err=%v", err)
	}
	if _, err := svc.InitAttachmentUpload(context.Background(), testCaller, "x.bin", "text/plain\r\nX-Test: yes", 10); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("MIME err=%v", err)
	}
	if store.putKey != "" {
		t.Fatalf("storage called for invalid request: %q", store.putKey)
	}
}

func TestOpenAttachmentRequiresExactPackageReferenceAndManagedPrefix(t *testing.T) {
	key := "plugins/space-a/attachments/file-id.bin"
	store := &artifactStorage{objects: map[string][]byte{key: []byte("hello"), "plugins/space-b/attachments/secret": []byte("secret")}}
	pkg := `{"attachments":[{"path":"assets/data.bin","content_type":"storage","mime_type":"application/octet-stream","object_key":"` + key + `","content_size":5}]}`
	svc := artifactService(pkg, store)
	result, err := svc.OpenAttachment(context.Background(), testCaller, "plugin-1", key)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	got, _ := io.ReadAll(result.Body)
	if string(got) != "hello" || result.Path != "assets/data.bin" {
		t.Fatalf("download=%q result=%#v", got, result)
	}
	if _, err := svc.OpenAttachment(context.Background(), testCaller, "plugin-1", "plugins/space-b/attachments/secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unreferenced err=%v", err)
	}
	if len(store.getCalls) != 1 {
		t.Fatalf("unexpected object reads: %#v", store.getCalls)
	}
}

func TestPrepareArchiveRejectsUnsafePathsKeysAndSymlinkConcepts(t *testing.T) {
	key := "plugins/space-a/attachments/x"
	tests := []string{
		`{"attachments":[{"path":"../escape","content_type":"raw","raw_content":"x"}]}`,
		`{"attachments":[{"path":"/absolute","content_type":"raw","raw_content":"x"}]}`,
		`{"attachments":[{"path":"dir\\evil","content_type":"raw","raw_content":"x"}]}`,
		`{"attachments":[{"path":"a","content_type":"raw","raw_content":"x","symlink_target":"b"}]}`,
		`{"attachments":[{"path":"a","content_type":"storage","object_key":"plugins/space-b/attachments/x"}]}`,
		`{"attachments":[{"path":"a","content_type":"storage","object_key":"https://metadata.invalid/x"}]}`,
		`{"attachments":[{"path":"same","content_type":"raw","raw_content":"1"},{"path":"same","content_type":"storage","object_key":"` + key + `"}]}`,
	}
	for _, pkg := range tests {
		t.Run(pkg, func(t *testing.T) {
			svc := artifactService(pkg, &artifactStorage{objects: map[string][]byte{key: []byte("x")}})
			if _, err := svc.PrepareArchive(context.Background(), testCaller, "plugin-1", ""); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestArchiveStreamsSortedRawAndStorageFiles(t *testing.T) {
	key := "plugins/space-a/attachments/blob"
	store := &artifactStorage{objects: map[string][]byte{key: []byte{0, 1, 2}}}
	pkg := `{"attachments":[` +
		`{"path":"z.bin","content_type":"storage","mime_type":"application/octet-stream","storage_uri":"` + key + `","content_size":3},` +
		`{"path":"a.txt","content_type":"raw","mime_type":"text/plain","raw_content":"hello","content_size":5}]}`
	svc := artifactService(pkg, store)
	archive, err := svc.PrepareArchive(context.Background(), testCaller, "plugin-1", "")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := svc.WriteArchive(context.Background(), archive, &output); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	contents := map[string][]byte{}
	for _, file := range zr.File {
		names = append(names, file.Name)
		if file.Mode()&fs.ModeSymlink != 0 {
			t.Fatalf("symlink entry: %q", file.Name)
		}
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		contents[file.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !sort.StringsAreSorted(names) || strings.Join(names, ",") != "a.txt,z.bin" {
		t.Fatalf("names=%#v", names)
	}
	if string(contents["a.txt"]) != "hello" || !bytes.Equal(contents["z.bin"], []byte{0, 1, 2}) {
		t.Fatalf("contents=%#v", contents)
	}
}

func TestPrepareArchiveEnforcesFileAndTotalLimits(t *testing.T) {
	svc := artifactService(`{"attachments":[{"path":"a","content_type":"raw","raw_content":"12"},{"path":"b","content_type":"raw","raw_content":"34"}]}`, &artifactStorage{})
	svc.maxArchiveFiles = 1
	if _, err := svc.PrepareArchive(context.Background(), testCaller, "plugin-1", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("file count err=%v", err)
	}
	svc.maxArchiveFiles, svc.maxArchiveBytes = 2, 3
	if _, err := svc.PrepareArchive(context.Background(), testCaller, "plugin-1", ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("total size err=%v", err)
	}
}

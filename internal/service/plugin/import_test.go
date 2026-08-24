package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	skillrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/skill"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/storage"
)

type importStorage struct {
	objects map[string][]byte
	deletes []string
}

func (s *importStorage) PresignPut(context.Context, string, string, time.Duration) (string, http.Header, error) {
	return "", nil, errors.New("unexpected PresignPut")
}
func (s *importStorage) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://cdn.invalid/" + key, nil
}
func (s *importStorage) PublicURL(context.Context, string) (string, error) {
	return "", errors.New("unexpected PublicURL")
}
func (s *importStorage) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	value, ok := s.objects[key]
	if !ok {
		return nil, errors.New("missing object " + key)
	}
	return io.NopCloser(bytes.NewReader(value)), nil
}
func (s *importStorage) StatObject(_ context.Context, key string) (storage.ObjectInfo, error) {
	value, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, errors.New("missing object " + key)
	}
	return storage.ObjectInfo{Size: int64(len(value))}, nil
}
func (s *importStorage) PutObject(_ context.Context, key string, reader io.Reader, _ int64, _ string) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	s.objects[key] = data
	return nil
}
func (s *importStorage) DeleteObject(_ context.Context, key string) error {
	s.deletes = append(s.deletes, key)
	delete(s.objects, key)
	return nil
}
func (s *importStorage) CopyObject(context.Context, string, string) error {
	return errors.New("unexpected CopyObject")
}

type fakeParseTasks struct {
	task       *skillrepo.ParseTaskRow
	consumed   []string
	released   []string
	consumeErr error
}

func (f *fakeParseTasks) GetParseTask(_ context.Context, id string) (*skillrepo.ParseTaskRow, error) {
	if f.task != nil && f.task.ID == id {
		return f.task, nil
	}
	return nil, nil
}
func (f *fakeParseTasks) MarkParseTaskConsumed(_ context.Context, id, ownerID, spaceID, skillID string) error {
	f.consumed = append(f.consumed, id+"|"+ownerID+"|"+spaceID+"|"+skillID)
	return f.consumeErr
}
func (f *fakeParseTasks) ReleaseConsumedParseTask(_ context.Context, id string) error {
	f.released = append(f.released, id)
	return nil
}

func skillZipFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	md, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := md.Write([]byte("---\nname: uploaded-skill\nversion: 1.0.0\n---\n# Uploaded Skill\nBody.")); err != nil {
		t.Fatal(err)
	}
	extra, err := zw.Create("scripts/run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.Write([]byte("echo ok")); err != nil {
		t.Fatal(err)
	}
	binary, err := zw.Create("assets/logo.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binary.Write([]byte{0x00, 0xff, 0xfe, 0x89, 0x50}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func importFixtures(t *testing.T) (*fakeStore, *importStorage, *fakeParseTasks, *Service) {
	t.Helper()
	zipBytes, sha := skillZipFixture(t)
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	blobs := &importStorage{objects: map[string][]byte{"tmp/upload.zip": zipBytes}}
	tasks := &fakeParseTasks{task: &skillrepo.ParseTaskRow{
		ID: "task-1", OwnerID: "user-1", SpaceID: "space-a", Status: "success",
		FileName: "orig.zip", FileURL: "tmp/upload.zip", FileSize: int64(len(zipBytes)), FileSHA256: sha,
		ResultName: "My Skill", ResultVersion: "2.0.0", ResultTags: []byte(`["deploy"]`),
	}}
	svc := New(store, blobs).WithParseTasks(tasks).WithRuntime(sequenceIDs("plugin-new", "zip-obj", "md-obj", "audit-new", "version-new", "placement-new", "extra-1", "extra-2"), func() time.Time { return time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC) })
	return store, blobs, tasks, svc
}

func sequenceIDs(ids ...string) func() string {
	n := 0
	return func() string {
		id := ids[n%len(ids)]
		n++
		return id
	}
}

func TestImportCreatesSkillPluginAndPublishesDefaultPlacement(t *testing.T) {
	store, blobs, tasks, svc := importFixtures(t)
	detail, err := svc.Import(context.Background(), testCaller, ImportParams{ParseTaskID: "task-1", Icon: "icons/s.png"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks.consumed) != 1 || tasks.consumed[0] != "task-1|user-1|space-a|" {
		t.Fatalf("consumed = %#v", tasks.consumed)
	}
	if len(tasks.released) != 0 {
		t.Fatalf("released = %#v", tasks.released)
	}
	created := store.create
	if created == nil || created.Type != model.PluginTypeSkill || created.Name != "My Skill" || created.Icon != "icons/s.png" {
		t.Fatalf("created = %#v", created)
	}
	if string(created.Tags) != `["deploy"]` {
		t.Fatalf("tags = %s", created.Tags)
	}
	pkg := string(created.Package)
	if !strings.Contains(pkg, `"SKILL.md"`) || !strings.Contains(pkg, "# Uploaded Skill") {
		t.Fatalf("package missing inline SKILL.md: %s", pkg)
	}
	// Each source file becomes its own attachment: text inline, binary storage.
	if !strings.Contains(pkg, `"scripts/run.sh"`) || !strings.Contains(pkg, "echo ok") {
		t.Fatalf("package missing inline text file: %s", pkg)
	}
	if !strings.Contains(pkg, `"assets/logo.bin"`) || !strings.Contains(pkg, `"content_type":"storage"`) {
		t.Fatalf("package missing binary storage attachment: %s", pkg)
	}
	if strings.Contains(pkg, "skill/ref.json") || strings.Contains(pkg, "skill/package.zip") {
		t.Fatalf("legacy ref.json/package.zip must be gone: %s", pkg)
	}
	if store.publishParams.Version != "2.0.0" || len(store.publishParams.Placements) != 1 || store.publishParams.Placements[0].PlacementCode != "default" {
		t.Fatalf("publish = %#v", store.publishParams)
	}
	uploaded := 0
	for key := range blobs.objects {
		if strings.HasPrefix(key, "plugins/space-a/attachments/") {
			uploaded++
		}
	}
	// Only the binary spills to object storage; the two text files stay inline.
	if uploaded != 1 {
		t.Fatalf("uploaded objects = %#v", blobs.objects)
	}
	if detail.Plugin.ID == "" {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestImportRejectsForeignUnfinishedOrReboundTasks(t *testing.T) {
	for _, mutate := range []func(*skillrepo.ParseTaskRow){
		func(task *skillrepo.ParseTaskRow) { task.OwnerID = "attacker" },
		func(task *skillrepo.ParseTaskRow) { task.SpaceID = "other-space" },
		func(task *skillrepo.ParseTaskRow) { task.Status = "processing" },
		func(task *skillrepo.ParseTaskRow) { task.SkillID = "legacy-skill" },
	} {
		_, _, tasks, svc := importFixtures(t)
		mutate(tasks.task)
		if _, err := svc.Import(context.Background(), testCaller, ImportParams{ParseTaskID: "task-1"}); !errors.Is(err, ErrInvalidParseTask) {
			t.Fatalf("err = %v", err)
		}
		if len(tasks.consumed) != 0 {
			t.Fatalf("foreign task was consumed: %#v", tasks.consumed)
		}
	}
}

func TestImportRejectsPublicVisibility(t *testing.T) {
	_, _, _, svc := importFixtures(t)
	if _, err := svc.Import(context.Background(), testCaller, ImportParams{ParseTaskID: "task-1", Visibility: model.PluginVisibilityPublic}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestImportReleasesTaskAndDeletesObjectsWhenCreateFails(t *testing.T) {
	store, blobs, tasks, svc := importFixtures(t)
	store.err = errors.New("db down")
	if _, err := svc.Import(context.Background(), testCaller, ImportParams{ParseTaskID: "task-1"}); err == nil {
		t.Fatal("expected error")
	}
	if len(tasks.released) != 1 || tasks.released[0] != "task-1" {
		t.Fatalf("released = %#v", tasks.released)
	}
	deleted := 0
	for _, key := range blobs.deletes {
		if strings.HasPrefix(key, "plugins/space-a/attachments/") {
			deleted++
		}
	}
	// The one spilled binary object is cleaned up when the create fails.
	if deleted != 1 {
		t.Fatalf("deletes = %#v", blobs.deletes)
	}
}

func TestSkillMarkdownPrefersRefObjectAndFallsBackToInline(t *testing.T) {
	space := "space-a"
	inlinePkg := packageWith(rawAtt("SKILL.md", "# inline doc"))
	legacyPkg := packageWith(rawAtt("skill/ref.json", `{"object_key":"skills/legacy/SKILL.md","zip_object_key":"skills/legacy/skill.zip","file_name":"pack.zip"}`))
	// Embedded member skills migrated from squads keep their objects under the
	// legacy squads/ root — the same read-only trust class as skills//experts/.
	squadPkg := packageWith(rawAtt("skill/ref.json", `{"object_key":"squads/team-1/members/member_01/skills/0.md"}`))
	store := &fakeStore{plugins: map[string]*model.Plugin{
		"inline-1": {ID: "inline-1", Name: "Inline", Type: model.PluginTypeSkill, SpaceID: &space, Package: inlinePkg},
		"legacy-1": {ID: "legacy-1", Name: "Legacy", Type: model.PluginTypeSkill, SpaceID: &space, Package: legacyPkg},
		"squad-1":  {ID: "squad-1", Name: "Member", Type: model.PluginTypeSkill, SpaceID: &space, Package: squadPkg},
	}}
	blobs := &importStorage{objects: map[string][]byte{
		"skills/legacy/SKILL.md":                      []byte("# legacy doc"),
		"skills/legacy/skill.zip":                     []byte("zip-bytes"),
		"squads/team-1/members/member_01/skills/0.md": []byte("# member doc"),
	}}
	svc := New(store, blobs)

	content, err := svc.SkillMarkdown(context.Background(), testCaller, "inline-1")
	if err != nil || content != "# inline doc" {
		t.Fatalf("inline = %q err=%v", content, err)
	}
	content, err = svc.SkillMarkdown(context.Background(), testCaller, "legacy-1")
	if err != nil || content != "# legacy doc" {
		t.Fatalf("legacy = %q err=%v", content, err)
	}
	content, err = svc.SkillMarkdown(context.Background(), testCaller, "squad-1")
	if err != nil || content != "# member doc" {
		t.Fatalf("squad = %q err=%v", content, err)
	}

	download, err := svc.OpenSkillPackage(context.Background(), testCaller, "legacy-1")
	if err != nil {
		t.Fatal(err)
	}
	if download.FileName != "pack.zip" {
		t.Fatalf("download = %#v", download)
	}
	var zipBuf bytes.Buffer
	if err := download.Write(&zipBuf); err != nil {
		t.Fatal(err)
	}
	if zipBuf.String() != "zip-bytes" {
		t.Fatalf("streamed legacy zip = %q", zipBuf.String())
	}
}

func TestOpenSkillPackageRejectsUnsafeLegacyKeys(t *testing.T) {
	space := "space-a"
	pkg := packageWith(rawAtt("skill/ref.json", `{"zip_object_key":"../secrets/root.zip"}`))
	store := &fakeStore{plugins: map[string]*model.Plugin{"bad-1": {ID: "bad-1", Name: "Bad", Type: model.PluginTypeSkill, SpaceID: &space, Package: pkg}}}
	svc := New(store, &importStorage{objects: map[string][]byte{}})
	if _, err := svc.OpenSkillPackage(context.Background(), testCaller, "bad-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestSkillArtifactsRejectForgedCrossSpacePointers(t *testing.T) {
	space := "space-a"
	// A caller-writable ref.json pointing into ANOTHER Space's managed prefix
	// must never be fetched, even though the key shape itself is clean.
	forged := packageWith(rawAtt("skill/ref.json", `{"object_key":"plugins/space-b/attachments/loot.md","zip_object_key":"plugins/space-b/attachments/loot.zip"}`))
	store := &fakeStore{plugins: map[string]*model.Plugin{
		"forged-1": {ID: "forged-1", Name: "Forged", Type: model.PluginTypeSkill, SpaceID: &space, Package: forged},
	}}
	blobs := &importStorage{objects: map[string][]byte{
		"plugins/space-b/attachments/loot.md":  []byte("secret"),
		"plugins/space-b/attachments/loot.zip": []byte("secret-zip"),
	}}
	svc := New(store, blobs)
	if _, err := svc.SkillMarkdown(context.Background(), testCaller, "forged-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skill_md err = %v", err)
	}
	if _, err := svc.OpenSkillPackage(context.Background(), testCaller, "forged-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download err = %v", err)
	}
}

func TestSkillArtifactsServeOwnSpaceManagedPointers(t *testing.T) {
	space := "space-a"
	pkg := packageWith(rawAtt("skill/ref.json", `{"file_name":"pack.zip","object_key":"plugins/space-a/attachments/doc.md","zip_object_key":"plugins/space-a/attachments/pack.zip"}`))
	store := &fakeStore{plugins: map[string]*model.Plugin{
		"own-1": {ID: "own-1", Name: "Own", Type: model.PluginTypeSkill, SpaceID: &space, Package: pkg},
	}}
	blobs := &importStorage{objects: map[string][]byte{
		"plugins/space-a/attachments/doc.md":   []byte("# own doc"),
		"plugins/space-a/attachments/pack.zip": []byte("own-zip"),
	}}
	svc := New(store, blobs)
	content, err := svc.SkillMarkdown(context.Background(), testCaller, "own-1")
	if err != nil || content != "# own doc" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	download, err := svc.OpenSkillPackage(context.Background(), testCaller, "own-1")
	if err != nil {
		t.Fatal(err)
	}
	var ownZip bytes.Buffer
	if err := download.Write(&ownZip); err != nil {
		t.Fatal(err)
	}
	if ownZip.String() != "own-zip" {
		t.Fatalf("streamed own zip = %q", ownZip.String())
	}
}

func TestResolveIconOnlyPresignsIconNamespaces(t *testing.T) {
	svc := New(&fakeStore{}, &importStorage{objects: map[string][]byte{}})
	ctx := context.Background()
	if got := svc.resolveIcon(ctx, "icons/abc/logo.png"); got != "https://cdn.invalid/icons/abc/logo.png" {
		t.Fatalf("icon namespace not presigned: %q", got)
	}
	// A caller-chosen key outside the icon namespaces must never yield a
	// signed URL for an unrelated object.
	if got := svc.resolveIcon(ctx, "plugins/space-b/attachments/loot.zip"); got != "plugins/space-b/attachments/loot.zip" {
		t.Fatalf("non-icon key was presigned: %q", got)
	}
	if got := svc.resolveIcon(ctx, "🐙"); got != "🐙" {
		t.Fatalf("glyph mangled: %q", got)
	}
}

func TestImportUpdatePublishFailureKeepsCommittedObjects(t *testing.T) {
	store, blobs, tasks, svc := importFixtures(t)
	space := "space-a"
	store.plugins["existing-1"] = &model.Plugin{ID: "existing-1", Name: "Old", Type: model.PluginTypeSkill, OwnerUID: "user-1", SpaceID: &space, Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{"attachments":[]}`)}
	store.publishErr = errors.New("version conflict")

	_, err := svc.Import(context.Background(), testCaller, ImportParams{ParseTaskID: "task-1", PluginID: "existing-1", Version: "9.9.9"})
	if err == nil {
		t.Fatal("expected publish failure")
	}
	// The document update committed and references the fresh objects — they
	// must survive the failed publish.
	for _, key := range blobs.deletes {
		if strings.HasPrefix(key, "plugins/space-a/attachments/") {
			t.Fatalf("committed object deleted: %q", key)
		}
	}
	if len(tasks.released) != 1 {
		t.Fatalf("task released = %#v", tasks.released)
	}
}

func TestSkillMarkdownRefObjectWinsOverInlineStub(t *testing.T) {
	space := "space-a"
	// Snapshot layout: inline SKILL.md is a stub; the authoritative document
	// lives behind the ref pointer and must win.
	pkg := packageWith(
		rawAtt("SKILL.md", "# stub"),
		rawAtt("skill/ref.json", `{"object_key":"squads/team-1/members/member_01/skills/0.md"}`),
	)
	store := &fakeStore{plugins: map[string]*model.Plugin{
		"snap-1": {ID: "snap-1", Name: "Snap", Type: model.PluginTypeSkill, SpaceID: &space, Package: pkg},
	}}
	blobs := &importStorage{objects: map[string][]byte{
		"squads/team-1/members/member_01/skills/0.md": []byte("# full doc"),
	}}
	content, err := New(store, blobs).SkillMarkdown(context.Background(), testCaller, "snap-1")
	if err != nil || content != "# full doc" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	// A trusted pointer whose object is missing falls back to the inline text
	// instead of failing the read.
	blobs.objects = map[string][]byte{}
	content, err = New(store, blobs).SkillMarkdown(context.Background(), testCaller, "snap-1")
	if err != nil || content != "# stub" {
		t.Fatalf("fallback content=%q err=%v", content, err)
	}
}

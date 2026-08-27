package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func zipWith(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type treeAttachment struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	MIMEType    string `json:"mime_type"`
	RawContent  string `json:"raw_content"`
	StorageURI  string `json:"storage_uri"`
	ContentSize int64  `json:"content_size"`
	ContentHash string `json:"content_hash"`
}

func decodeTree(t *testing.T, atts []map[string]any) map[string]treeAttachment {
	t.Helper()
	out := map[string]treeAttachment{}
	for _, a := range atts {
		raw, _ := json.Marshal(a)
		var ta treeAttachment
		if err := json.Unmarshal(raw, &ta); err != nil {
			t.Fatal(err)
		}
		out[ta.Path] = ta
	}
	return out
}

func TestBuildSkillAttachmentTreeClassifiesAndRoots(t *testing.T) {
	binary := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0xff, 0xfe} // non-UTF-8
	zipData := zipWith(t, map[string][]byte{
		"pkg/SKILL.md":            []byte("# Rooted Skill\nBody."),
		"pkg/references/notes.md": []byte("checklist text"),
		"pkg/assets/logo.png":     binary,
	})
	svc := New(&fakeStore{}, &importStorage{objects: map[string][]byte{}})

	atts, uploaded, spilled, err := svc.buildSkillAttachmentTree(context.Background(), "space-a", "plug-1", zipData, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tree := decodeTree(t, atts)

	// Rooted at the SKILL.md directory: the "pkg/" prefix is stripped everywhere.
	md, ok := tree["SKILL.md"]
	if !ok || md.ContentType != "raw" || md.MIMEType != "text/markdown" || md.RawContent != "# Rooted Skill\nBody." {
		t.Fatalf("SKILL.md attachment = %#v", md)
	}
	notes, ok := tree["references/notes.md"]
	if !ok || notes.ContentType != "raw" || notes.RawContent != "checklist text" {
		t.Fatalf("text file should inline as raw: %#v", notes)
	}
	logo, ok := tree["assets/logo.png"]
	if !ok || logo.ContentType != "storage" || logo.MIMEType != "image/png" {
		t.Fatalf("binary file should be storage: %#v", logo)
	}
	wantKey := deterministicSkillObjectKey("space-a", "plug-1", "assets/logo.png", binary)
	if logo.StorageURI != wantKey {
		t.Fatalf("storage_uri = %q want %q", logo.StorageURI, wantKey)
	}
	if !validReferencedObjectKey(logo.StorageURI, "space-a") {
		t.Fatalf("storage key not in controlled prefix: %q", logo.StorageURI)
	}
	sum := sha256.Sum256(binary)
	if logo.ContentHash != "sha256:"+hex.EncodeToString(sum[:]) || logo.ContentSize != int64(len(binary)) {
		t.Fatalf("binary hash/size = %s/%d", logo.ContentHash, logo.ContentSize)
	}
	if len(uploaded) != 1 || uploaded[0] != wantKey {
		t.Fatalf("uploaded = %#v", uploaded)
	}
	if len(spilled) != 0 {
		t.Fatalf("no text should spill here: %#v", spilled)
	}
}

func TestBuildSkillAttachmentTreeDeterministicAndOverride(t *testing.T) {
	zipData := zipWith(t, map[string][]byte{
		"SKILL.md":      []byte("# original"),
		"docs/guide.md": []byte("guide"),
	})
	svc := New(&fakeStore{}, &importStorage{objects: map[string][]byte{}})

	// skillMDOverride replaces the SKILL.md bytes (import injects frontmatter).
	override := []byte("---\nname: x\n---\n# rewritten")
	a1, _, _, err := svc.buildSkillAttachmentTree(context.Background(), "space-a", "plug-1", zipData, override, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decodeTree(t, a1)["SKILL.md"].RawContent != string(override) {
		t.Fatalf("override not applied: %#v", decodeTree(t, a1)["SKILL.md"])
	}
	// Same inputs → byte-identical attachment maps (idempotent re-run).
	a2, _, _, err := svc.buildSkillAttachmentTree(context.Background(), "space-a", "plug-1", zipData, override, nil)
	if err != nil {
		t.Fatal(err)
	}
	j1, _ := json.Marshal(a1)
	j2, _ := json.Marshal(a2)
	if !bytes.Equal(j1, j2) {
		t.Fatalf("non-deterministic output:\n%s\n%s", j1, j2)
	}
}

func TestBuildSkillAttachmentTreeSpillsOversizeText(t *testing.T) {
	big := bytes.Repeat([]byte("a"), skillInlineMaxBytes+1)
	zipData := zipWith(t, map[string][]byte{
		"SKILL.md":           []byte("# s"),
		"references/big.txt": big,
	})
	svc := New(&fakeStore{}, &importStorage{objects: map[string][]byte{}})
	atts, uploaded, spilled, err := svc.buildSkillAttachmentTree(context.Background(), "space-a", "plug-1", zipData, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	big2 := decodeTree(t, atts)["references/big.txt"]
	if big2.ContentType != "storage" {
		t.Fatalf("oversize text should spill to storage: %#v", big2)
	}
	if len(uploaded) != 1 || len(spilled) != 1 || spilled[0] != "references/big.txt" {
		t.Fatalf("spill bookkeeping: uploaded=%#v spilled=%#v", uploaded, spilled)
	}
}

// TestSkillPackageRoundTrip builds a tree, persists it as a skill plugin, then
// reconstructs the download zip and checks every original file is byte-identical
// (inline text and spilled binary both survive the round trip).
func TestSkillPackageRoundTrip(t *testing.T) {
	space := "space-a"
	binary := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	files := map[string][]byte{
		"SKILL.md":          []byte("# Round Trip\nbody"),
		"scripts/deploy.sh": []byte("echo deploy"),
		"assets/logo.png":   binary,
	}
	zipData := zipWith(t, files)
	blobs := &importStorage{objects: map[string][]byte{}}
	svc := New(&fakeStore{}, blobs)

	atts, _, _, err := svc.buildSkillAttachmentTree(context.Background(), space, "plug-1", zipData, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pkgRaw, err := json.Marshal(map[string]any{"$schema": packageSchema, "attachments": atts})
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the write chokepoint: storage object keys live in the row's sidecar,
	// not inline in the 2.0 package.
	pkg, keys, err := splitStorageKeys(pkgRaw, space)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{plugins: map[string]*model.Plugin{
		"plug-1": {ID: "plug-1", Name: "Round Trip", Type: model.PluginTypeSkill, SpaceID: &space, Package: pkg, AttachmentKeys: keys},
	}}
	svc.repo = store

	stream, err := svc.OpenSkillPackage(context.Background(), testCaller, "plug-1")
	if err != nil {
		t.Fatal(err)
	}
	if stream.FileName != "Round Trip.zip" {
		t.Fatalf("filename = %q", stream.FileName)
	}
	var out bytes.Buffer
	if err := stream.Write(&out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = data
	}
	for name, want := range files {
		if !bytes.Equal(got[name], want) {
			t.Fatalf("entry %s = %q want %q", name, got[name], want)
		}
	}
	if len(got) != len(files) {
		t.Fatalf("reconstructed entries = %v", got)
	}
}

func TestExpandSkillPackageFromLegacyZip(t *testing.T) {
	space := "space-a"
	zipData := zipWith(t, map[string][]byte{
		"SKILL.md":           []byte("# Real Doc\nbody"),
		"references/note.md": []byte("note text"),
	})
	// Legacy shape: SKILL.md stub + ref.json pointing at a legacy-root zip object.
	pkg := json.RawMessage(`{"$schema":"` + packageSchema + `","attachments":[` +
		`{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# stub"},` +
		`{"path":"skill/ref.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"zip_object_key\":\"experts/x/skill.zip\"}"}` +
		`]}`)
	blobs := &importStorage{objects: map[string][]byte{"experts/x/skill.zip": zipData}}
	svc := New(&fakeStore{}, blobs)

	out, _, changed, err := svc.ExpandSkillPackage(context.Background(), space, "plug-1", pkg)
	if err != nil || !changed {
		t.Fatalf("expand err=%v changed=%v", err, changed)
	}
	tree := decodeTree(t, mapsFromPackage(t, out))
	if _, ok := tree["skill/ref.json"]; ok {
		t.Fatalf("ref.json should be gone: %s", out)
	}
	if tree["SKILL.md"].RawContent != "# Real Doc\nbody" {
		t.Fatalf("SKILL.md not expanded from zip: %#v", tree["SKILL.md"])
	}
	if tree["references/note.md"].RawContent != "note text" {
		t.Fatalf("supporting file missing: %#v", tree)
	}
	// Idempotent: expanding the result again is a no-op.
	if _, _, changed2, err := svc.ExpandSkillPackage(context.Background(), space, "plug-1", out); err != nil || changed2 {
		t.Fatalf("second expand changed=%v err=%v", changed2, err)
	}
}

// TestExpandSkillPackageSplitsStorageKeys locks the 2.0 storage contract: a
// legacy zip carrying a binary expands to a package with NO storage_uri inline
// (the strict decoder would reject it) and returns the path->object-key sidecar
// so the caller can persist attachment_keys_json.
func TestExpandSkillPackageSplitsStorageKeys(t *testing.T) {
	space := "space-a"
	binary := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	zipData := zipWith(t, map[string][]byte{
		"SKILL.md":        []byte("# Doc\nbody"),
		"assets/logo.png": binary,
	})
	pkg := json.RawMessage(`{"$schema":"` + packageSchema + `","attachments":[` +
		`{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# stub"},` +
		`{"path":"skill/ref.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"zip_object_key\":\"experts/x/skill.zip\"}"}` +
		`]}`)
	blobs := &importStorage{objects: map[string][]byte{"experts/x/skill.zip": zipData}}
	svc := New(&fakeStore{}, blobs)

	out, keys, changed, err := svc.ExpandSkillPackage(context.Background(), space, "plug-1", pkg)
	if err != nil || !changed {
		t.Fatalf("expand err=%v changed=%v", err, changed)
	}
	// ExpandSkillPackage already validated `out` through the 2.0 decoder; assert it
	// carries no host storage_uri (the strict decoder would have rejected it).
	if bytes.Contains(out, []byte("storage_uri")) {
		t.Fatalf("expanded package leaked storage_uri: %s", out)
	}
	// The binary's object key is carried in the sidecar, scoped to this Space.
	var m map[string]string
	if err := json.Unmarshal(keys, &m); err != nil {
		t.Fatalf("sidecar keys not JSON: %v (%s)", err, keys)
	}
	wantKey := deterministicSkillObjectKey(space, "plug-1", "assets/logo.png", binary)
	if m["assets/logo.png"] != wantKey {
		t.Fatalf("sidecar key = %q want %q", m["assets/logo.png"], wantKey)
	}
}

func TestExpandSkillPackageEmptyRefKeepsInlineDoc(t *testing.T) {
	space := "space-a"
	// Empty ref (no zip, no object_key): collapse to the single inline SKILL.md.
	pkg := json.RawMessage(`{"$schema":"` + packageSchema + `","attachments":[` +
		`{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# just a title\n"},` +
		`{"path":"skill/ref.json","content_type":"raw","mime_type":"application/json","raw_content":"{}"}` +
		`]}`)
	svc := New(&fakeStore{}, &importStorage{objects: map[string][]byte{}})
	out, _, changed, err := svc.ExpandSkillPackage(context.Background(), space, "plug-2", pkg)
	if err != nil || !changed {
		t.Fatalf("expand err=%v changed=%v", err, changed)
	}
	tree := decodeTree(t, mapsFromPackage(t, out))
	if len(tree) != 1 || tree["SKILL.md"].RawContent != "# just a title\n" {
		t.Fatalf("empty-ref should keep single inline SKILL.md: %s", out)
	}
}

// TestExpandSkillPackagePrefersRefObjectOverStub is the Q1 fix: a snapshot skill
// inlines only a SKILL.md stub while its real document lives behind the
// ref.json object_key. Expansion must keep the real document, not the stub.
func TestExpandSkillPackagePrefersRefObjectOverStub(t *testing.T) {
	space := "space-a"
	pkg := json.RawMessage(`{"$schema":"` + packageSchema + `","attachments":[` +
		`{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# name"},` +
		`{"path":"skill/ref.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"object_key\":\"skills/x/SKILL.md\"}"}` +
		`]}`)
	blobs := &importStorage{objects: map[string][]byte{"skills/x/SKILL.md": []byte("# Real Doc\nfull body")}}
	svc := New(&fakeStore{}, blobs)

	out, _, changed, err := svc.ExpandSkillPackage(context.Background(), space, "plug-q1", pkg)
	if err != nil || !changed {
		t.Fatalf("expand err=%v changed=%v", err, changed)
	}
	tree := decodeTree(t, mapsFromPackage(t, out))
	if got := tree["SKILL.md"].RawContent; got != "# Real Doc\nfull body" {
		t.Fatalf("expansion kept the stub instead of the ref object: %q", got)
	}
}

func TestExpandSkillPackageTreeUnchanged(t *testing.T) {
	space := "space-a"
	// Already a tree (no legacy pointer): unchanged.
	pkg := json.RawMessage(`{"$schema":"` + packageSchema + `","attachments":[` +
		`{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# doc"}` +
		`]}`)
	svc := New(&fakeStore{}, &importStorage{objects: map[string][]byte{}})
	out, _, changed, err := svc.ExpandSkillPackage(context.Background(), space, "plug-3", pkg)
	if err != nil || changed {
		t.Fatalf("tree should be unchanged: changed=%v err=%v", changed, err)
	}
	if string(out) != string(pkg) {
		t.Fatalf("unchanged package should be returned verbatim")
	}
}

func mapsFromPackage(t *testing.T, pkg json.RawMessage) []map[string]any {
	t.Helper()
	var doc struct {
		Attachments []map[string]any `json:"attachments"`
	}
	if err := json.Unmarshal(pkg, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Attachments
}

package plugin

import "testing"

// The archive/attachment endpoints are gone, but these helpers still guard the
// upsert package validation (schema.go) and the skill artifact reads
// (skill_artifact.go), so their traversal rejections keep dedicated coverage.

func TestValidReferencedObjectKeyEnforcesManagedSpacePrefix(t *testing.T) {
	valid := []string{
		"plugins/space-a/attachments/file.zip",
		"plugins/space-a/attachments/nested/file.bin",
	}
	for _, key := range valid {
		if !validReferencedObjectKey(key, "space-a") {
			t.Fatalf("key %q rejected", key)
		}
	}
	invalid := []string{
		"",
		"plugins/space-b/attachments/file.zip", // foreign Space
		"plugins/space-a/attachments/",         // empty remainder
		"plugins/space-a/attachments/../../secret", // traversal
		"plugins/space-a/attachments/a/../b",       // non-clean path
		"/plugins/space-a/attachments/file.zip",    // absolute
		"s3://bucket/plugins/space-a/attachments/x",
		"plugins/space-a/attachments/file?x=1",
		"plugins/space-a/attachments/file#frag",
		"plugins/space-a/attachments/file\\name",
	}
	for _, key := range invalid {
		if validReferencedObjectKey(key, "space-a") {
			t.Fatalf("key %q accepted", key)
		}
	}
}

func TestNormalizedArchivePathRejectsTraversalAndNonCleanNames(t *testing.T) {
	if got, ok := normalizedArchivePath("docs/readme.md"); !ok || got != "docs/readme.md" {
		t.Fatalf("clean path = %q ok=%v", got, ok)
	}
	invalid := []string{
		"", "/abs", "trailing/", "a//b", "./a", "../a", "a/../b", "a/./b", "a\\b", "a\x00b",
	}
	for _, name := range invalid {
		if _, ok := normalizedArchivePath(name); ok {
			t.Fatalf("path %q accepted", name)
		}
	}
}

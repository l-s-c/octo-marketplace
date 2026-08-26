package plugin

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxAttachmentBytes = int64(20 << 20)
	defaultMaxArchiveBytes    = int64(100 << 20)
)

var safeObjectSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

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

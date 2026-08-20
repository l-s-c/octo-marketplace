package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const secretPlaceholder = "__OCTO_SECRET_PLACEHOLDER__"

var secretFragments = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "access_key", "private_key", "authorization", "cookie"}

// DeterministicID creates stable family-specific IDs for globally shared tables.
func DeterministicID(family, sourceID string) string {
	sum := sha256.Sum256([]byte(family + "\x00" + sourceID))
	return family + "_" + hex.EncodeToString(sum[:])[:32]
}

// PluginID returns the opaque unified-table storage ID. Cowork external IDs are
// prefixed at the service boundary (for example skill:<storage-id>); storing that
// prefix here would violate the existing VARCHAR(64) contract and double-prefix
// API responses. Colliding legacy IDs are deterministically namespaced.
func PluginID(family, sourceID string, globalCount int) string {
	if globalCount == 1 && len(sourceID) <= 64 {
		return sourceID
	}
	return DeterministicID(family, sourceID)
}

func hashJSON(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func canonical(v any) ([]byte, error) { return json.Marshal(v) }

type manifestExample struct {
	Title string `json:"title"`
	Input string `json:"input"`
}

type pluginManifest struct {
	Schema      string            `json:"$schema"`
	Description string            `json:"description"`
	Examples    []manifestExample `json:"examples"`
	Labels      []string          `json:"labels"`
	Name        string            `json:"name"`
	PluginName  string            `json:"plugin_name"`
	PluginType  string            `json:"plugin_type"`
}

type packageAttachment struct {
	ContentHash string `json:"content_hash"`
	ContentSize int    `json:"content_size"`
	ContentType string `json:"content_type"`
	MIMEType    string `json:"mime_type"`
	Path        string `json:"path"`
	RawContent  string `json:"raw_content"`
}

type pluginPackage struct {
	Schema      string              `json:"$schema"`
	Attachments []packageAttachment `json:"attachments"`
}

type rawAttachment struct {
	path, mimeType, content string
}

func newPluginManifest(pluginName, pluginType, name, description string, labels, examples []string) pluginManifest {
	if labels == nil {
		labels = []string{}
	}
	mappedExamples := make([]manifestExample, 0, len(examples))
	for i, input := range examples {
		mappedExamples = append(mappedExamples, manifestExample{
			Title: fmt.Sprintf("使用示例 %d", i+1),
			Input: input,
		})
	}
	return pluginManifest{
		Schema:      "cowork-plugin-manifest-1.0.json",
		PluginName:  pluginName,
		PluginType:  pluginType,
		Name:        name,
		Description: description,
		Labels:      labels,
		Examples:    mappedExamples,
	}
}

func packageJSON(manifestJSON []byte, extras ...rawAttachment) ([]byte, error) {
	attachments := make([]packageAttachment, 0, len(extras)+1)
	add := func(attachmentPath, mimeType, content string) error {
		if !validAttachmentPath(attachmentPath) {
			return fmt.Errorf("invalid package attachment path %q", attachmentPath)
		}
		raw := []byte(content)
		attachments = append(attachments, packageAttachment{
			Path:        attachmentPath,
			ContentType: "raw",
			MIMEType:    mimeType,
			RawContent:  content,
			ContentSize: len(raw),
			ContentHash: hashJSON(raw),
		})
		return nil
	}
	if err := add("manifest.json", "application/json", string(manifestJSON)); err != nil {
		return nil, err
	}
	for _, extra := range extras {
		if err := add(extra.path, extra.mimeType, extra.content); err != nil {
			return nil, err
		}
	}
	sort.Slice(attachments, func(i, j int) bool { return attachments[i].Path < attachments[j].Path })
	unique := attachments[:0]
	for _, attachment := range attachments {
		if len(unique) == 0 || unique[len(unique)-1].Path != attachment.Path {
			unique = append(unique, attachment)
			continue
		}
		if unique[len(unique)-1] != attachment {
			return nil, fmt.Errorf("conflicting package attachment path %q", attachment.Path)
		}
	}
	return canonical(pluginPackage{Schema: "cowork-plugin-package-1.0.json", Attachments: unique})
}

func validAttachmentPath(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return false
	}
	clean := path.Clean(value)
	return clean == value && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func jsonAttachment(path string, value any) (rawAttachment, error) {
	raw, err := canonical(value)
	if err != nil {
		return rawAttachment{}, err
	}
	return rawAttachment{path: path, mimeType: "application/json", content: string(raw)}, nil
}

func secretShaped(key string) bool {
	key = strings.TrimSpace(key)
	var normalized strings.Builder
	for i, r := range key {
		if r == '-' || r == '.' || r == ' ' {
			normalized.WriteByte('_')
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := rune(key[i-1])
			if prev != '_' && !(prev >= 'A' && prev <= 'Z') {
				normalized.WriteByte('_')
			}
		}
		normalized.WriteRune(r)
	}
	key = strings.ToLower(normalized.String())
	for _, fragment := range secretFragments {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

// SanitizeConnectorJSON enforces a deliberately stricter policy than legacy
// writes: env/header values are blanked, and any non-empty value beneath a
// secret-shaped key rejects the source record. It never returns secret values.
func SanitizeConnectorJSON(raw []byte) ([]byte, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte(`{}`)
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("connector config must be an object")
	}
	if servers, present := root["mcpServers"]; present {
		if _, ok := servers.(map[string]any); !ok {
			return nil, fmt.Errorf("/mcpServers must be an object")
		}
	}
	if err := sanitizeNode(value, ""); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return fmt.Errorf("multiple JSON values are not allowed")
}

func sanitizeNode(value any, path string) error {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			childPath := path + "/" + key
			normalizedKey := strings.ToLower(strings.TrimSpace(key))
			if normalizedKey == "env" || normalizedKey == "headers" {
				values, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("%s must be an object", childPath)
				}
				for name, raw := range values {
					if _, ok := raw.(string); !ok {
						return fmt.Errorf("%s/%s must be a string", childPath, name)
					}
					values[name] = ""
				}
				continue
			}
			if secretShaped(key) {
				switch typed := child.(type) {
				case nil:
				case string:
					if typed != "" && typed != secretPlaceholder {
						return fmt.Errorf("secret-shaped value rejected at %s", childPath)
					}
				default:
					return fmt.Errorf("secret-shaped value rejected at %s", childPath)
				}
				node[key] = ""
				continue
			}
			if err := sanitizeNode(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range node {
			if err := sanitizeNode(child, fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func namesFromTagIDs(raw []byte, dictionary map[int64]string) ([]string, error) {
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		name, ok := dictionary[id]
		if !ok {
			return nil, fmt.Errorf("unknown tag id %d", id)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func digestLines(lines []string) string {
	sort.Strings(lines)
	return hashJSON([]byte(strings.Join(lines, "\n")))
}

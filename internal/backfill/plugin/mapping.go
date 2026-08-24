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

	"github.com/Mininglamp-OSS/octo-marketplace/internal/id"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

const secretPlaceholder = "__OCTO_SECRET_PLACEHOLDER__"

var secretFragments = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "access_key", "private_key", "authorization", "cookie"}

// DeterministicID creates stable IDs for backfilled rows, rendered as UUIDs
// (version 8 = derived per RFC 9562, RFC variant) from sha256(family NUL
// sourceID) so re-runs stay idempotent and the legacy→unified mapping remains
// recomputable.
func DeterministicID(family, sourceID string) string {
	sum := sha256.Sum256([]byte(family + "\x00" + sourceID))
	var raw [16]byte
	copy(raw[:], sum[:16])
	raw[6] = (raw[6] & 0x0f) | 0x80 // version 8: name-derived, not random or time-based
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 9562 variant
	return id.Format(raw)
}

// PluginID returns the opaque unified-table storage ID for a legacy record.
// Every backfilled plugin gets a deterministic UUID: legacy source IDs are not
// preserved, keeping the unified tables on a single UUID format. Cowork
// external IDs are prefixed at the service boundary (for example
// skill:<storage-id>); storing that prefix here would double-prefix API
// responses.
func PluginID(family, sourceID string) string {
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
	Connector   *packageConnector   `json:"connector,omitempty"`
}

// packageConnector is the top-level connector descriptor of connector plugin
// packages (plugin-lib schema evolution): the connector kind plus an optional
// source identifier.
type packageConnector struct {
	Source string `json:"source,omitempty"`
	Type   string `json:"type"`
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

func packageJSON(extras ...rawAttachment) ([]byte, error) {
	return connectorPackageJSON(nil, extras...)
}

func connectorPackageJSON(connector *packageConnector, extras ...rawAttachment) ([]byte, error) {
	attachments := make([]packageAttachment, 0, len(extras))
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
	return canonical(pluginPackage{Schema: "cowork-plugin-package-1.0.json", Attachments: unique, Connector: connector})
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

// canonicalDocs runs draft documents through the service write validator so
// backfilled rows can never bypass or drift from the API contract: the same
// manifest/package schema rules, secret scan, canonicalization, and hash
// formulas apply. The package embeds the service-canonical manifest bytes.
func canonicalDocs(outerName, pluginType string, tags []string, draftManifest []byte, extras []rawAttachment, spaceID string) (*pluginsvc.CanonicalDocuments, error) {
	return canonicalConnectorDocs(outerName, pluginType, tags, draftManifest, extras, spaceID, nil)
}

func canonicalConnectorDocs(outerName, pluginType string, tags []string, draftManifest []byte, extras []rawAttachment, spaceID string, connector *packageConnector) (*pluginsvc.CanonicalDocuments, error) {
	if tags == nil {
		tags = []string{}
	}
	tagsJSON, err := canonical(tags)
	if err != nil {
		return nil, err
	}
	manifest, canonicalTags, err := pluginsvc.CanonicalizeManifest(outerName, model.PluginType(pluginType), tagsJSON, draftManifest)
	if err != nil {
		return nil, fmt.Errorf("manifest rejected by service validator: %w", err)
	}
	pkg, err := connectorPackageJSON(connector, extras...)
	if err != nil {
		return nil, err
	}
	docs, err := pluginsvc.CanonicalizeDocuments(outerName, model.PluginType(pluginType), canonicalTags, manifest, pkg, spaceID)
	if err != nil {
		return nil, fmt.Errorf("package rejected by service validator: %w", err)
	}
	return docs, nil
}

// teamAgentsMarkdown renders the deterministic AGENTS.md entry document of an
// expert_team package from its legacy squad fields: collaboration prose,
// leader, ordered dispatch strategies, dependencies, and permission. It is the
// team package's O\nY attachment under the contract layout, and backfill and
// repackage must render byte-identical output.
func teamAgentsMarkdown(name, summary, leader string, strategies, dependencies any, permission string) string {
	var b strings.Builder
	b.WriteString("# " + strings.TrimSpace(name) + "\n")
	if trimmed := strings.TrimSpace(summary); trimmed != "" {
		b.WriteString("\n" + trimmed + "\n")
	}
	b.WriteString("\n## 协作方式\n")
	if trimmed := strings.TrimSpace(leader); trimmed != "" {
		b.WriteString("\n- Leader: " + trimmed + "\n")
	}
	if lines := stringItems(strategies); len(lines) > 0 {
		b.WriteString("\n### 策略\n")
		for i, line := range lines {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, line))
		}
	}
	if deps, ok := dependencies.(map[string]any); ok {
		blocking := stringItems(deps["blocking"])
		recommended := stringItems(deps["recommended"])
		if len(blocking) > 0 || len(recommended) > 0 {
			b.WriteString("\n### 依赖\n")
			for _, line := range blocking {
				b.WriteString("- 阻塞: " + line + "\n")
			}
			for _, line := range recommended {
				b.WriteString("- 推荐: " + line + "\n")
			}
		}
	}
	if trimmed := strings.TrimSpace(permission); trimmed != "" {
		b.WriteString("\n### 权限\n" + trimmed + "\n")
	}
	return b.String()
}

// entryMarkdown renders the minimal deterministic entry document (used when a
// package must carry its contract entry file but the legacy source has no
// authored text).
func entryMarkdown(name, summary string) string {
	var b strings.Builder
	b.WriteString("# " + strings.TrimSpace(name) + "\n")
	if trimmed := strings.TrimSpace(summary); trimmed != "" {
		b.WriteString("\n" + trimmed + "\n")
	}
	return b.String()
}

// expertAgentsMarkdown picks the expert package's AGENTS.md entry document:
// the legacy instruction verbatim when present, else a minimal deterministic
// document from the display fields (the contract requires the entry file).
func expertAgentsMarkdown(name, summary, instruction string) string {
	if trimmed := strings.TrimSpace(instruction); trimmed != "" {
		return instruction
	}
	return entryMarkdown(name, summary)
}

// stringItems extracts the non-blank string entries of a decoded JSON array.
func stringItems(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
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

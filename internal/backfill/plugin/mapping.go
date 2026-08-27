package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/id"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/plugindoc"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

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
// manifest/package schema rules, canonicalization, and hash
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
	docs, err := pluginsvc.CanonicalizeMigratedDocuments(outerName, model.PluginType(pluginType), canonicalTags, manifest, pkg, spaceID)
	if err != nil {
		return nil, fmt.Errorf("package rejected by service validator: %w", err)
	}
	return docs, nil
}

// teamAgentsMarkdown delegates to the shared renderer so backfill, repackage,
// and the live admin import all emit byte-identical expert_team AGENTS.md.
func teamAgentsMarkdown(name, summary, leader string, strategies, dependencies any, permission string) string {
	return plugindoc.TeamAgentsMarkdown(name, summary, leader, strategies, dependencies, permission)
}

// entryMarkdown delegates to the shared minimal-entry renderer.
func entryMarkdown(name, summary string) string {
	return plugindoc.EntryMarkdown(name, summary)
}

// expertAgentsMarkdown delegates to the shared expert AGENTS.md renderer.
func expertAgentsMarkdown(name, summary, instruction string) string {
	return plugindoc.ExpertAgentsMarkdown(name, summary, instruction)
}

// stringItems delegates to the shared decoded-JSON-array extractor.
func stringItems(value any) []string {
	return plugindoc.StringItems(value)
}

// SanitizeConnectorJSON delegates to the shared connector-config sanitizer so
// backfill and the live admin import blank env/header and secret-shaped values
// identically. It never returns secret values.
func SanitizeConnectorJSON(raw []byte) ([]byte, error) {
	return plugindoc.SanitizeConnectorJSON(raw)
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

package plugincontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	uuidPattern             = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hashPattern             = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	skillPathExpr           = regexp.MustCompile(`^skills/[^/]+/SKILL\.md$`)
	windowsDrivePathPattern = regexp.MustCompile(`^[A-Za-z]:`)
)

func ValidatePlugin(item Plugin) error {
	if !uuidPattern.MatchString(item.PluginID) {
		return invalid(CodeInvalidField, "plugin_id", "must be a lowercase UUID string")
	}
	if err := validateText("plugin_name", item.PluginName, 160, true); err != nil {
		return err
	}
	if !item.PluginType.Valid() {
		return invalid(CodeInvalidPluginType, "plugin_type", "must be expert, skill, expert_team, or connector")
	}
	manifest, err := DecodeManifest(item.ManifestJSON)
	if err != nil {
		return err
	}
	if manifest.PluginName != item.PluginName {
		return invalid(CodeInvalidField, "manifest_json.plugin_name", "must match plugin_name")
	}
	if manifest.PluginType != item.PluginType {
		return invalid(CodeInvalidField, "manifest_json.plugin_type", "must match plugin_type")
	}
	if !isJSONNull(item.PluginJSON) {
		if _, err := DecodePackage(item.PluginType, item.PluginJSON); err != nil {
			return err
		}
	}
	if item.PluginHash != "" {
		if !hashPattern.MatchString(item.PluginHash) {
			return invalid(CodeInvalidField, "plugin_hash", "must use sha256:<64 lowercase hex>")
		}
		expected, err := ComputePluginHash(item.ManifestJSON, item.PluginJSON)
		if err != nil {
			return err
		}
		if item.PluginHash != expected {
			return invalid(CodeHashMismatch, "plugin_hash", "does not match manifest_json and plugin_json")
		}
	}
	if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() || item.UpdatedAt.Before(item.CreatedAt) {
		return invalid(CodeInvalidField, "updated_at", "created_at and updated_at are required and updated_at must not precede created_at")
	}
	return nil
}

// NormalizePlugin canonicalizes both JSON documents and calculates the hash.
// It does not assign identity, status, or timestamps; those are host-owned.
func NormalizePlugin(item Plugin) (Plugin, error) {
	manifest, err := CanonicalJSON(item.ManifestJSON)
	if err != nil {
		return Plugin{}, withPath(err, "manifest_json")
	}
	packageJSON := []byte("null")
	if !isJSONNull(item.PluginJSON) {
		packageJSON, err = CanonicalJSON(item.PluginJSON)
		if err != nil {
			return Plugin{}, withPath(err, "plugin_json")
		}
	}
	item.ManifestJSON = manifest
	item.PluginJSON = packageJSON
	item.PluginHash, err = ComputePluginHash(manifest, packageJSON)
	if err != nil {
		return Plugin{}, err
	}
	if err := ValidatePlugin(item); err != nil {
		return Plugin{}, err
	}
	return item, nil
}

func ValidateRelation(item Relation) error {
	for _, identifier := range []struct {
		field string
		value string
	}{
		{field: "relation_id", value: item.RelationID},
		{field: "source_plugin_id", value: item.SourcePluginID},
		{field: "target_plugin_id", value: item.TargetPluginID},
	} {
		if !uuidPattern.MatchString(identifier.value) {
			return invalid(CodeInvalidField, identifier.field, "must be a lowercase UUID string")
		}
	}
	if item.SourcePluginID == item.TargetPluginID {
		return invalid(CodeInvalidRelation, "target_plugin_id", "self-reference is not allowed")
	}
	if !item.RelationType.Valid() {
		return invalid(CodeInvalidRelationType, "relation_type", "must be expert_team_expert, expert_skill, or expert_connector")
	}
	if item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() || item.UpdatedAt.Before(item.CreatedAt) {
		return invalid(CodeInvalidField, "updated_at", "created_at and updated_at are required and updated_at must not precede created_at")
	}
	return nil
}

func ValidateRelationEndpoints(item Relation, sourceType, targetType Type) error {
	if err := ValidateRelation(item); err != nil {
		return err
	}
	wantSource, wantTarget := TypeExpert, TypeSkill
	switch item.RelationType {
	case RelationExpertTeamExpert:
		wantSource, wantTarget = TypeExpertTeam, TypeExpert
	case RelationExpertConnector:
		wantTarget = TypeConnector
	}
	if sourceType != wantSource || targetType != wantTarget {
		return invalid(CodeInvalidRelation, "relation_type", fmt.Sprintf("%s requires %s -> %s", item.RelationType, wantSource, wantTarget))
	}
	return nil
}

func DecodeManifest(raw json.RawMessage) (Manifest, error) {
	value, err := decodeJSON(raw)
	if err != nil {
		return Manifest{}, withPath(err, "manifest_json")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Manifest{}, invalid(CodeInvalidJSON, "manifest_json", "must be a JSON object")
	}
	if _, ok := object["description"].(string); !ok {
		return Manifest{}, invalid(CodeInvalidField, "manifest_json.description", "is required")
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, invalid(CodeInvalidJSON, "manifest_json", err.Error())
	}
	if manifest.Schema != ManifestSchemaID {
		return Manifest{}, invalid(CodeInvalidField, "manifest_json.$schema", "must be "+ManifestSchemaID)
	}
	if err := validateText("manifest_json.plugin_name", manifest.PluginName, 160, true); err != nil {
		return Manifest{}, err
	}
	if !manifest.PluginType.Valid() {
		return Manifest{}, invalid(CodeInvalidPluginType, "manifest_json.plugin_type", "unsupported plugin type")
	}
	if err := validateText("manifest_json.name", manifest.Name, 160, true); err != nil {
		return Manifest{}, err
	}
	if err := validateText("manifest_json.description", manifest.Description, 0, false); err != nil {
		return Manifest{}, err
	}
	if rawLabels, ok := object["labels"]; ok {
		labels, ok := rawLabels.([]any)
		if !ok {
			return Manifest{}, invalid(CodeInvalidField, "manifest_json.labels", "must be an array")
		}
		seen := map[string]struct{}{}
		for index, rawLabel := range labels {
			label, ok := rawLabel.(string)
			if !ok {
				return Manifest{}, invalid(CodeInvalidField, fmt.Sprintf("manifest_json.labels[%d]", index), "must be a string")
			}
			if err := validateText(fmt.Sprintf("manifest_json.labels[%d]", index), label, 0, false); err != nil {
				return Manifest{}, err
			}
			if _, exists := seen[label]; exists {
				return Manifest{}, invalid(CodeInvalidField, "manifest_json.labels", "labels must be unique")
			}
			seen[label] = struct{}{}
		}
	}
	if rawExamples, ok := object["examples"]; ok {
		examples, ok := rawExamples.([]any)
		if !ok {
			return Manifest{}, invalid(CodeInvalidField, "manifest_json.examples", "must be an array")
		}
		for index, rawExample := range examples {
			example, ok := rawExample.(map[string]any)
			if !ok || len(example) != 2 {
				return Manifest{}, invalid(CodeInvalidField, fmt.Sprintf("manifest_json.examples[%d]", index), "must contain only title and input")
			}
			for _, field := range []string{"title", "input"} {
				text, ok := example[field].(string)
				if !ok {
					return Manifest{}, invalid(CodeInvalidField, fmt.Sprintf("manifest_json.examples[%d].%s", index, field), "must be a string")
				}
				if err := validateText(fmt.Sprintf("manifest_json.examples[%d].%s", index, field), text, 0, false); err != nil {
					return Manifest{}, err
				}
			}
		}
	}
	return manifest, nil
}

func DecodePackage(pluginType Type, raw json.RawMessage) (Package, error) {
	value, err := decodeJSON(raw)
	if err != nil {
		return Package{}, withPath(err, "plugin_json")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return Package{}, invalid(CodeInvalidJSON, "plugin_json", "must be a JSON object")
	}
	for key := range object {
		if key != "$schema" && key != "connector" && key != "attachments" {
			return Package{}, invalid(CodeInvalidField, "plugin_json."+key, "unknown property")
		}
	}
	var packageValue Package
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&packageValue); err != nil {
		return Package{}, invalid(CodeInvalidJSON, "plugin_json", err.Error())
	}
	if packageValue.Schema != PackageSchemaID {
		return Package{}, invalid(CodeInvalidField, "plugin_json.$schema", "must be "+PackageSchemaID)
	}
	if rawConnector, exists := object["connector"]; exists && rawConnector == nil {
		return Package{}, invalid(CodeInvalidField, "plugin_json.connector", "must be an object")
	}
	rawAttachments, _ := object["attachments"].([]any)
	paths := make(map[string]struct{}, len(packageValue.Attachments))
	for index := range packageValue.Attachments {
		attachment := &packageValue.Attachments[index]
		prefix := fmt.Sprintf("plugin_json.attachments[%d]", index)
		if index < len(rawAttachments) {
			if rawAttachment, ok := rawAttachments[index].(map[string]any); ok {
				_, hasRaw := rawAttachment["raw_content"]
				_, hasStorage := rawAttachment["storage_uri"]
				if attachment.ContentType == ContentRaw && hasStorage || attachment.ContentType == ContentStorage && hasRaw {
					return Package{}, invalid(CodeInvalidAttachmentContent, prefix, "content fields must match content_type")
				}
				for _, field := range []string{"content_size", "content_hash"} {
					if value, exists := rawAttachment[field]; exists && value == nil {
						return Package{}, invalid(CodeInvalidField, prefix+"."+field, "must not be null")
					}
				}
			}
		}
		if err := validateAttachment(*attachment, prefix); err != nil {
			return Package{}, err
		}
		if _, exists := paths[attachment.Path]; exists {
			return Package{}, invalid(CodeDuplicateAttachmentPath, prefix+".path", "attachment path must be unique")
		}
		paths[attachment.Path] = struct{}{}
	}
	if err := validatePackageShape(pluginType, packageValue, paths); err != nil {
		return Package{}, err
	}
	return packageValue, nil
}

func validateAttachment(item Attachment, prefix string) error {
	if item.Path == "" || strings.Contains(item.Path, "\\") || strings.HasPrefix(item.Path, "/") || windowsDrivePathPattern.MatchString(item.Path) || path.Clean(item.Path) != item.Path || item.Path == "." || strings.IndexFunc(item.Path, func(value rune) bool { return value <= 0x1f || value == 0x7f }) >= 0 {
		return invalid(CodeInvalidAttachmentPath, prefix+".path", "must be a normalized relative slash path")
	}
	for _, segment := range strings.Split(item.Path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return invalid(CodeInvalidAttachmentPath, prefix+".path", "must not contain empty, dot, or parent segments")
		}
	}
	mimeParts := strings.Split(item.MIMEType, "/")
	if err := validateText(prefix+".mime_type", item.MIMEType, 0, true); err != nil || len(mimeParts) != 2 || mimeParts[0] == "" || mimeParts[1] == "" || strings.IndexFunc(item.MIMEType, unicode.IsSpace) >= 0 {
		if err != nil {
			return err
		}
		return invalid(CodeInvalidField, prefix+".mime_type", "must be a media type")
	}
	switch item.ContentType {
	case ContentRaw:
		if item.RawContent == nil || item.StorageURI != "" {
			return invalid(CodeInvalidAttachmentContent, prefix, "raw content requires raw_content and forbids storage_uri")
		}
		if !utf8.ValidString(*item.RawContent) {
			return invalid(CodeInvalidAttachmentContent, prefix+".raw_content", "must be valid UTF-8 text")
		}
		if item.ContentHash != "" && !hashPattern.MatchString(item.ContentHash) {
			return invalid(CodeInvalidField, prefix+".content_hash", "must use sha256:<64 lowercase hex>")
		}
		size := int64(len([]byte(*item.RawContent)))
		if item.ContentSize != nil && *item.ContentSize != size {
			return invalid(CodeInvalidAttachmentContent, prefix+".content_size", "does not match raw_content bytes")
		}
		if item.ContentHash != "" && item.ContentHash != digestBytes([]byte(*item.RawContent)) {
			return invalid(CodeHashMismatch, prefix+".content_hash", "does not match raw_content")
		}
	case ContentStorage:
		if item.RawContent != nil || strings.TrimSpace(item.StorageURI) == "" {
			return invalid(CodeInvalidAttachmentContent, prefix, "storage content requires storage_uri and forbids raw_content")
		}
		if item.ContentSize != nil && *item.ContentSize < 0 {
			return invalid(CodeInvalidAttachmentContent, prefix+".content_size", "must not be negative")
		}
		if item.ContentHash != "" && !hashPattern.MatchString(item.ContentHash) {
			return invalid(CodeInvalidField, prefix+".content_hash", "must use sha256:<64 lowercase hex>")
		}
	default:
		return invalid(CodeInvalidField, prefix+".content_type", "must be raw or storage")
	}
	return nil
}

func validatePackageShape(pluginType Type, packageValue Package, paths map[string]struct{}) error {
	has := func(name string) bool { _, ok := paths[name]; return ok }
	hasSkill := false
	for item := range paths {
		if skillPathExpr.MatchString(item) {
			hasSkill = true
			break
		}
	}
	switch pluginType {
	case TypeExpert:
		if packageValue.Connector != nil || !has("AGENTS.md") {
			return invalid(CodeInvalidField, "plugin_json.attachments", "expert requires AGENTS.md and no connector descriptor")
		}
	case TypeSkill:
		if packageValue.Connector != nil || !has("SKILL.md") {
			return invalid(CodeInvalidField, "plugin_json.attachments", "skill requires SKILL.md and no connector descriptor")
		}
	case TypeExpertTeam:
		if packageValue.Connector != nil || len(paths) != 1 || !has("AGENTS.md") {
			return invalid(CodeInvalidField, "plugin_json.attachments", "expert_team must contain only AGENTS.md")
		}
	case TypeConnector:
		if packageValue.Connector == nil {
			return invalid(CodeInvalidField, "plugin_json.connector", "connector package requires connector descriptor")
		}
		if err := validateText("plugin_json.connector.source", packageValue.Connector.Source, 0, true); err != nil {
			return err
		}
		switch packageValue.Connector.Type {
		case ConnectorMCP:
			if !has("mcp.json") {
				return invalid(CodeInvalidField, "plugin_json.attachments", "mcp requires mcp.json")
			}
		case ConnectorOpenConnector:
			if !has("mcp.json") {
				return invalid(CodeInvalidField, "plugin_json.attachments", "openconnector requires mcp.json")
			}
		case ConnectorCLI:
			if !has("cli.json") || !hasSkill {
				return invalid(CodeInvalidField, "plugin_json.attachments", "cli requires cli.json and skills/<name>/SKILL.md")
			}
		case ConnectorSkillOnly:
			if len(packageValue.Attachments) == 0 || packageValue.Attachments[0].Path != "token-schema.json" || !hasSkill {
				return invalid(CodeInvalidField, "plugin_json.attachments", "skill-only requires token-schema.json first and skills/<name>/SKILL.md")
			}
		default:
			return invalid(CodeInvalidField, "plugin_json.connector.type", "unsupported connector type")
		}
	default:
		return invalid(CodeInvalidPluginType, "plugin_type", "unsupported plugin type")
	}
	return nil
}

func (value Type) Valid() bool {
	return value == TypeExpert || value == TypeSkill || value == TypeExpertTeam || value == TypeConnector
}

func (value RelationType) Valid() bool {
	return value == RelationExpertTeamExpert || value == RelationExpertSkill || value == RelationExpertConnector
}

func validateText(field, value string, max int, required bool) error {
	if !utf8.ValidString(value) || (max > 0 && len([]rune(value)) > max) || (required && strings.TrimSpace(value) == "") {
		message := "must be valid UTF-8"
		if max > 0 {
			message = fmt.Sprintf("must be valid UTF-8 with at most %d characters", max)
		}
		return invalid(CodeInvalidField, field, message)
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func withPath(err error, prefix string) error {
	if current, ok := err.(*ValidationError); ok {
		path := prefix
		if current.Path != "" {
			path += "." + current.Path
		}
		return invalid(current.Code, path, current.Message)
	}
	return invalid(CodeInvalidJSON, prefix, err.Error())
}

func decodeJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, invalid(CodeInvalidJSON, "", "must be valid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, invalid(CodeInvalidJSON, "", err.Error())
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, invalid(CodeInvalidJSON, "", err.Error())
	}
	return value, nil
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			value, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated object")
		}
		return object, nil
	case '[':
		var values []any
		for decoder.More() {
			value, err := decodeValue(decoder)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated array")
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delimiter)
	}
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

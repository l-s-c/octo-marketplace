package plugin

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

const (
	manifestSchema      = "cowork-plugin-manifest-1.0.json"
	packageSchema       = "cowork-plugin-package-1.0.json"
	pluginPackageSchema = packageSchema
)

func normalizeManifest(raw json.RawMessage, outerName string, outerType model.PluginType, outerTags json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	value, err := decodeJSONObject(raw)
	if err != nil {
		return nil, nil, ErrInvalidRequest
	}

	schema, ok := requiredString(value, "$schema")
	if !ok || schema != manifestSchema {
		return nil, nil, ErrInvalidRequest
	}
	pluginName, ok := requiredString(value, "plugin_name")
	if !ok || !validName(pluginName) || pluginName != outerName {
		return nil, nil, ErrInvalidRequest
	}
	pluginType, ok := requiredString(value, "plugin_type")
	if !ok || model.PluginType(pluginType) != outerType || !validPluginType(model.PluginType(pluginType)) {
		return nil, nil, ErrInvalidRequest
	}
	name, ok := requiredString(value, "name")
	if !ok || name == "" {
		return nil, nil, ErrInvalidRequest
	}
	if _, ok := requiredString(value, "description"); !ok {
		return nil, nil, ErrInvalidRequest
	}

	labels, err := normalizeStringList(value["labels"], true)
	if err != nil {
		return nil, nil, err
	}
	value["labels"] = labels
	tags, err := normalizeTags(outerTags)
	if err != nil {
		return nil, nil, err
	}
	canonicalLabels, err := canonicalJSONValue(labels)
	if err != nil || !bytes.Equal(canonicalLabels, tags) {
		return nil, nil, ErrInvalidRequest
	}

	examples, err := normalizeExamples(value["examples"])
	if err != nil {
		return nil, nil, err
	}
	value["examples"] = examples
	canonical, err := canonicalJSONValue(value)
	if err != nil {
		return nil, nil, err
	}
	return canonical, tags, nil
}

// normalizePackage validates and canonicalizes a plugin package. spaceID scopes
// storage attachments: a storage_uri must already be an approved object key for
// the owning Space (the same rule the archive/download readers enforce), so a
// package can never be written in a shape its own archive endpoint rejects.
func normalizePackage(raw, canonicalManifest json.RawMessage, spaceID string) (json.RawMessage, error) {
	value, err := decodeJSONObject(raw)
	if err != nil || !onlyKeys(value, "$schema", "attachments") {
		return nil, ErrInvalidRequest
	}
	schema, ok := requiredString(value, "$schema")
	if !ok || schema != packageSchema {
		return nil, ErrInvalidRequest
	}
	attachmentsValue, ok := value["attachments"]
	if !ok {
		return nil, ErrInvalidRequest
	}
	attachments, ok := attachmentsValue.([]any)
	if !ok {
		return nil, ErrInvalidRequest
	}

	seen := make(map[string]struct{}, len(attachments))
	manifestCount := 0
	for _, item := range attachments {
		attachment, ok := item.(map[string]any)
		if !ok || !onlyKeys(attachment, "path", "content_type", "mime_type", "raw_content", "storage_uri", "content_size", "content_hash") {
			return nil, ErrInvalidRequest
		}
		attachmentPath, ok := requiredString(attachment, "path")
		if !ok {
			return nil, ErrInvalidRequest
		}
		if normalized, valid := normalizedArchivePath(attachmentPath); !valid || normalized != attachmentPath {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := seen[attachmentPath]; duplicate {
			return nil, ErrInvalidRequest
		}
		seen[attachmentPath] = struct{}{}

		contentType, ok := requiredString(attachment, "content_type")
		if !ok || (contentType != "raw" && contentType != "storage") {
			return nil, ErrInvalidRequest
		}
		mimeType, ok := requiredString(attachment, "mime_type")
		if !ok {
			return nil, ErrInvalidRequest
		}
		if _, exists := attachment["content_size"]; exists {
			number, ok := attachment["content_size"].(json.Number)
			if !ok {
				return nil, ErrInvalidRequest
			}
			size, err := number.Int64()
			if err != nil || size < 0 {
				return nil, ErrInvalidRequest
			}
		}
		if hash, exists := attachment["content_hash"]; exists {
			if _, ok := hash.(string); !ok {
				return nil, ErrInvalidRequest
			}
		}

		rawContent, hasRaw := attachment["raw_content"]
		storageURI, hasStorage := attachment["storage_uri"]
		if hasRaw == hasStorage {
			return nil, ErrInvalidRequest
		}
		switch contentType {
		case "raw":
			if !hasRaw {
				return nil, ErrInvalidRequest
			}
			if _, ok := rawContent.(string); !ok {
				return nil, ErrInvalidRequest
			}
		case "storage":
			if !hasStorage {
				return nil, ErrInvalidRequest
			}
			uri, ok := storageURI.(string)
			if !ok || !safeObjectSegment.MatchString(spaceID) || !validReferencedObjectKey(uri, spaceID) {
				return nil, ErrInvalidRequest
			}
		}

		if attachmentPath == "manifest.json" {
			manifestCount++
			content, ok := rawContent.(string)
			if contentType != "raw" || !ok || mimeType != "application/json" || content != string(canonicalManifest) {
				return nil, ErrInvalidRequest
			}
			if sizeValue, exists := attachment["content_size"]; exists {
				size, _ := sizeValue.(json.Number).Int64()
				if size != int64(len(canonicalManifest)) {
					return nil, ErrInvalidRequest
				}
			}
			if hashValue, exists := attachment["content_hash"]; exists && hashValue.(string) != hashJSON(canonicalManifest) {
				return nil, ErrInvalidRequest
			}
		}
	}
	if manifestCount != 1 {
		return nil, ErrInvalidRequest
	}

	sort.Slice(attachments, func(i, j int) bool {
		return attachments[i].(map[string]any)["path"].(string) < attachments[j].(map[string]any)["path"].(string)
	})
	value["attachments"] = attachments
	canonical, err := canonicalJSONValue(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func decodeJSONObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > maxJSONBytes {
		return nil, ErrInvalidRequest
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil || value == nil || ensureJSONEOF(dec) != nil {
		return nil, ErrInvalidRequest
	}
	return value, nil
}

func requiredString(value map[string]any, key string) (string, bool) {
	raw, exists := value[key]
	text, ok := raw.(string)
	return text, exists && ok
}

func onlyKeys(value map[string]any, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range value {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}

func normalizeStringList(value any, optional bool) ([]string, error) {
	if value == nil && optional {
		return []string{}, nil
	}
	items, ok := value.([]any)
	if !ok || len(items) > maxTags {
		return nil, ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, ErrInvalidRequest
		}
		text = strings.TrimSpace(text)
		if text == "" || len(text) > maxTagBytes {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := seen[text]; duplicate {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
	}
	return out, nil
}

func normalizeExamples(value any) ([]any, error) {
	if value == nil {
		return []any{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, ErrInvalidRequest
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		example, ok := item.(map[string]any)
		if !ok || !onlyKeys(example, "title", "input") {
			return nil, ErrInvalidRequest
		}
		if _, ok := requiredString(example, "title"); !ok {
			return nil, ErrInvalidRequest
		}
		if _, ok := requiredString(example, "input"); !ok {
			return nil, ErrInvalidRequest
		}
		out = append(out, example)
	}
	return out, nil
}

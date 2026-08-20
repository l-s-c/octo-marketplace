package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

const (
	maxJSONBytes  = 1 << 20 // current-state and snapshot JSON are metadata, not artifacts
	maxNameBytes  = 160
	maxTags       = 100
	maxTagBytes   = 128
	maxRelations  = 200
	maxPlacements = 100
	maxListLimit  = 100
)

var (
	versionPattern    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	placementPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)
	relationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

func validPluginType(v model.PluginType) bool {
	switch v {
	case model.PluginTypeExpert, model.PluginTypeExpertTeam, model.PluginTypeSkill, model.PluginTypeConnector:
		return true
	default:
		return false
	}
}

func validVisibility(v model.PluginVisibility, systemAdmin bool) bool {
	switch v {
	case model.PluginVisibilityPublic, model.PluginVisibilitySpace, model.PluginVisibilityPrivate:
		return true
	case model.PluginVisibilitySystem:
		return systemAdmin
	default:
		return false
	}
}

func validName(v string) bool {
	return v != "" && utf8.ValidString(v) && len(v) <= maxNameBytes && !strings.ContainsRune(v, '\x00')
}

func validVersion(v string) bool       { return versionPattern.MatchString(strings.TrimSpace(v)) }
func validPlacementCode(v string) bool { return placementPattern.MatchString(v) }
func validRelationID(v string) bool    { return relationIDPattern.MatchString(v) }

func validRelationSource(relation string, source model.PluginType) bool {
	switch relation {
	case "expert_team_member":
		return source == model.PluginTypeExpertTeam
	case "expert_skill":
		return source == model.PluginTypeExpert || source == model.PluginTypeExpertTeam
	case "plugin_dependency":
		return source == model.PluginTypeExpert || source == model.PluginTypeExpertTeam || source == model.PluginTypeConnector
	default:
		return false
	}
}

func validRelationTarget(relation string, target model.PluginType) bool {
	switch relation {
	case "expert_team_member":
		return target == model.PluginTypeExpert
	case "expert_skill":
		return target == model.PluginTypeSkill
	case "plugin_dependency":
		return target == model.PluginTypeSkill || target == model.PluginTypeConnector
	default:
		return false
	}
}

func normalizeObject(raw json.RawMessage) (json.RawMessage, string, error) {
	canonical, err := canonicalJSONObject(raw)
	if err != nil {
		return nil, "", ErrInvalidRequest
	}
	return canonical, hashJSON(canonical), nil
}

func normalizeOptionalObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	canonical, err := canonicalJSONObject(raw)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	return canonical, nil
}

func canonicalJSONObject(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > maxJSONBytes {
		return nil, ErrInvalidRequest
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, ErrInvalidRequest
	}
	canonical, err := json.Marshal(value)
	if err != nil || len(canonical) > maxJSONBytes {
		return nil, ErrInvalidRequest
	}
	return canonical, nil
}

func canonicalJSONValue(value any) (json.RawMessage, error) {
	canonical, err := json.Marshal(value)
	if err != nil || len(canonical) > maxJSONBytes {
		return nil, ErrInvalidRequest
	}
	return canonical, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == nil {
		return ErrInvalidRequest
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func normalizeTags(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("[]"), nil
	}
	if len(raw) > maxJSONBytes {
		return nil, ErrInvalidRequest
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tags []string
	if err := dec.Decode(&tags); err != nil || ensureJSONEOF(dec) != nil || len(tags) > maxTags {
		return nil, ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || !utf8.ValidString(tag) || len(tag) > maxTagBytes {
			return nil, ErrInvalidRequest
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return canonicalJSONValue(out)
}

func hashJSON(canonical json.RawMessage) string {
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// maxEmbeddedSecretScanDepth bounds re-parsing of JSON documents carried as
// string values (attachment raw_content). Legitimate packages nest at most one
// document level; deeper nesting fails closed.
const maxEmbeddedSecretScanDepth = 5

// rejectSecretValues rejects values likely to be credentials while allowing
// declarations (secret_names/required_secrets) and references (${NAME}, env://,
// secret://, vault://). It applies to every plugin type — experts and teams
// carry MCP config too — and examines both manifest and package so the same
// invariant protects current state, immutable versions, and audits. String
// values that are themselves JSON documents (attachment raw_content, where
// connector config canonically lives) are re-parsed and scanned.
func rejectSecretValues(values ...json.RawMessage) error {
	for _, raw := range values {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return ErrInvalidRequest
		}
		if secretValuePresent(value, nil, 0) {
			return ErrSecretValue
		}
	}
	return nil
}

func secretValuePresent(value any, path []string, depth int) bool {
	switch x := value.(type) {
	case map[string]any:
		for key, child := range x {
			normalizedKey := normalizeSecretKey(key)
			next := append(path, normalizedKey)
			insideSecretValues := len(path) > 0 && (path[len(path)-1] == "secrets" || path[len(path)-1] == "credentials")
			if isSecretDeclaration(normalizedKey) && !insideSecretValues {
				if secretDeclarationSafe(normalizedKey, child) {
					continue
				}
				return true
			}
			if isSecretContainer(next) {
				if secretContainerHasValue(child, normalizedKey) {
					return true
				}
				continue
			}
			if isSecretField(normalizedKey) && (nonEmptyLiteral(child) || secretContainerHasValue(child, "secrets")) {
				return true
			}
			if secretValuePresent(child, next, depth) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if secretValuePresent(child, path, depth) {
				return true
			}
		}
	case string:
		return embeddedJSONHasSecret(x, path, depth)
	}
	return false
}

// embeddedJSONHasSecret re-parses string values that carry whole JSON documents
// so secrets cannot be smuggled past the walk inside attachment raw_content or
// further string-encoded layers.
func embeddedJSONHasSecret(text string, path []string, depth int) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	if depth >= maxEmbeddedSecretScanDepth {
		return true
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil || ensureJSONEOF(dec) != nil {
		return false
	}
	switch value.(type) {
	case map[string]any, []any:
		return secretValuePresent(value, path, depth+1)
	}
	return false
}

func isSecretContainer(path []string) bool {
	if len(path) == 0 {
		return false
	}
	leaf := path[len(path)-1]
	return leaf == "env" || leaf == "headers" || leaf == "secrets" || leaf == "credentials"
}

func secretContainerHasValue(value any, container string) bool {
	switch x := value.(type) {
	case map[string]any:
		if isSecretDeclarationObject(x) {
			return false
		}
		for key, child := range x {
			normalizedKey := normalizeSecretKey(key)
			// In value-mapping containers, caller-chosen keys may look like
			// declarations (for example CUSTOM_NAME) but their values are secrets.
			if (container == "secrets" || container == "credentials") && nonEmptyLiteral(child) {
				return true
			}
			if isSecretDeclaration(normalizedKey) {
				if secretDeclarationSafe(normalizedKey, child) {
					continue
				}
				return true
			}
			if isSecretField(normalizedKey) && nonEmptyLiteral(child) {
				return true
			}
			if nestedContainer := isSecretContainer([]string{normalizedKey}); nestedContainer {
				if secretContainerHasValue(child, normalizedKey) {
					return true
				}
				continue
			}
			// A secrets container maps caller-chosen secret names to values. Env and
			// header containers, by contrast, routinely contain harmless literals.
			if (container == "secrets" || container == "credentials") && nonEmptyLiteral(child) {
				return true
			}
			if secretContainerHasValue(child, container) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if (container == "secrets" || container == "credentials") && nonEmptyLiteral(child) {
				return true
			}
			if secretContainerHasValue(child, container) {
				return true
			}
		}
	}
	return false
}

func secretDeclarationSafe(key string, value any) bool {
	if strings.HasSuffix(key, "_ref") || strings.HasSuffix(key, "_refs") {
		switch x := value.(type) {
		case string:
			return isSecretReference(x)
		case []any:
			for _, item := range x {
				ref, ok := item.(string)
				if !ok || !isSecretReference(ref) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	if strings.HasSuffix(key, "_name") || strings.HasSuffix(key, "_names") || strings.HasPrefix(key, "required_") {
		switch x := value.(type) {
		case string:
			return strings.TrimSpace(x) != ""
		case []any:
			for _, item := range x {
				name, ok := item.(string)
				if !ok || strings.TrimSpace(name) == "" {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return false
}

func isSecretDeclarationObject(value map[string]any) bool {
	if len(value) == 0 {
		return false
	}
	hasNameOrRef := false
	for key, child := range value {
		switch normalizeSecretKey(key) {
		case "name", "key", "env", "description", "required", "optional", "type":
			// Declaration metadata must not itself contain nested configuration.
			if _, object := child.(map[string]any); object {
				return false
			}
			if _, array := child.([]any); array {
				return false
			}
			if normalizeSecretKey(key) == "name" || normalizeSecretKey(key) == "key" || normalizeSecretKey(key) == "env" {
				hasNameOrRef = true
			}
		case "ref", "reference", "source":
			s, ok := child.(string)
			if !ok || !isSecretReference(s) {
				return false
			}
			hasNameOrRef = true
		default:
			return false
		}
	}
	return hasNameOrRef
}

func nonEmptyLiteral(value any) bool {
	switch x := value.(type) {
	case string:
		x = strings.TrimSpace(x)
		return x != "" && !isSecretReference(x)
	case nil:
		return false
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func isSecretReference(v string) bool {
	lower := strings.ToLower(strings.TrimSpace(v))
	return (strings.HasPrefix(lower, "${") && strings.HasSuffix(lower, "}")) ||
		strings.HasPrefix(lower, "env://") || strings.HasPrefix(lower, "secret://") ||
		strings.HasPrefix(lower, "vault://") || strings.HasPrefix(lower, "ref://")
}

func normalizeSecretKey(key string) string {
	key = strings.TrimSpace(key)
	var out strings.Builder
	out.Grow(len(key) + 4)
	for i, r := range key {
		if r == '-' || r == '.' || r == ' ' {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "_") {
				out.WriteByte('_')
			}
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := rune(key[i-1])
			if prev != '_' && prev != '-' && prev != '.' && prev != ' ' && !(prev >= 'A' && prev <= 'Z') {
				out.WriteByte('_')
			}
		}
		out.WriteRune(r)
	}
	return strings.ToLower(out.String())
}

// secretDescriptorSuffixes are trailing words that describe an auth mechanism
// rather than carry it (auth_type: "none", token_format: "jwt"); such keys hold
// metadata, not credential values.
var secretDescriptorSuffixes = map[string]struct{}{
	"type": {}, "mode": {}, "method": {}, "scheme": {}, "kind": {},
	"enabled": {}, "required": {}, "format": {}, "provider": {}, "status": {}, "version": {},
}

func isSecretField(key string) bool {
	key = normalizeSecretKey(key)
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '_' })
	if len(parts) > 1 {
		if _, descriptor := secretDescriptorSuffixes[parts[len(parts)-1]]; descriptor {
			return false
		}
	}
	for _, part := range parts {
		switch part {
		case "password", "passwd", "secret", "token", "bearer", "auth", "authorization", "credential", "credentials", "apikey":
			return true
		}
	}
	return strings.Contains(key, "api_key") || strings.Contains(key, "private_key") ||
		strings.Contains(key, "access_key")
}

// isSecretDeclaration matches keys that declare a secret by name or reference
// (secret_name, token_refs, required_*). The stem must itself be secret-shaped:
// generic keys like file_name or member_ref are ordinary data, not declarations.
func isSecretDeclaration(key string) bool {
	key = normalizeSecretKey(key)
	if strings.HasPrefix(key, "required_") {
		return true
	}
	for _, suffix := range []string{"_name", "_names", "_ref", "_refs"} {
		if strings.HasSuffix(key, suffix) {
			return isSecretField(strings.TrimSuffix(key, suffix))
		}
	}
	return false
}

func hashForTest(raw json.RawMessage) (string, error) {
	canonical, err := canonicalJSONObject(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	return hashJSON(canonical), nil
}

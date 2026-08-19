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
	versionPattern   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	placementPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)
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

// rejectConnectorSecrets rejects values likely to be credentials while allowing
// declarations (secret_names/required_secrets) and references (${NAME}, env://,
// secret://, vault://). It deliberately examines both manifest and package so
// the same invariant protects current state, immutable versions, and audits.
func rejectConnectorSecrets(values ...json.RawMessage) error {
	for _, raw := range values {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return ErrInvalidRequest
		}
		if secretValuePresent(value, nil) {
			return ErrSecretValue
		}
	}
	return nil
}

func secretValuePresent(value any, path []string) bool {
	switch x := value.(type) {
	case map[string]any:
		for key, child := range x {
			next := append(path, strings.ToLower(key))
			if isSecretContainer(next) {
				if secretContainerHasValue(child) {
					return true
				}
				continue
			}
			if isSecretField(key) && !isSecretDeclaration(key) && nonEmptyLiteral(child) {
				return true
			}
			if secretValuePresent(child, next) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if secretValuePresent(child, path) {
				return true
			}
		}
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

func secretContainerHasValue(value any) bool {
	switch x := value.(type) {
	case map[string]any:
		for _, child := range x {
			if nonEmptyLiteral(child) || secretContainerHasValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if nonEmptyLiteral(child) || secretContainerHasValue(child) {
				return true
			}
		}
	}
	return false
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

func isSecretField(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
	for _, marker := range []string{"password", "passwd", "secret", "token", "api_key", "apikey", "private_key", "authorization", "credential"} {
		if key == marker || strings.HasSuffix(key, "_"+marker) {
			return true
		}
	}
	return false
}

func isSecretDeclaration(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
	return strings.HasSuffix(key, "_name") || strings.HasSuffix(key, "_names") ||
		strings.HasSuffix(key, "_ref") || strings.HasSuffix(key, "_refs") ||
		strings.HasPrefix(key, "required_")
}

func hashForTest(raw json.RawMessage) (string, error) {
	canonical, err := canonicalJSONObject(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize: %w", err)
	}
	return hashJSON(canonical), nil
}

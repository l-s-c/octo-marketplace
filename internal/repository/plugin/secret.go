package plugin

import (
	"bytes"
	"encoding/json"
	"strings"
)

// maxPersistedSecretScanDepth bounds re-parsing of JSON documents carried as
// string values (attachment raw_content); deeper nesting fails closed.
const maxPersistedSecretScanDepth = 5

func rejectPersistedSecretValues(values ...json.RawMessage) error {
	for _, raw := range values {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return ErrUnsafeConnectorData
		}
		if persistedSecretValuePresent(value, 0) {
			return ErrUnsafeConnectorData
		}
	}
	return nil
}

func persistedSecretValuePresent(value any, depth int) bool {
	switch x := value.(type) {
	case map[string]any:
		for key, child := range x {
			normalized := normalizePersistedSecretKey(key)
			if isPersistedSecretContainer(normalized) && persistedContainerHasLiteral(child) {
				return true
			}
			if isPersistedSecretField(normalized) && !isPersistedSecretDeclaration(normalized) && persistedNonEmptyLiteral(child) {
				return true
			}
			if persistedSecretValuePresent(child, depth) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if persistedSecretValuePresent(child, depth) {
				return true
			}
		}
	case string:
		return persistedEmbeddedJSONHasSecret(x, depth)
	}
	return false
}

// persistedEmbeddedJSONHasSecret re-parses string values that carry whole JSON
// documents so secrets in attachment raw_content cannot bypass the walk.
func persistedEmbeddedJSONHasSecret(text string, depth int) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	if depth >= maxPersistedSecretScanDepth {
		return true
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return false
	}
	var extra any
	if dec.Decode(&extra) == nil {
		return false
	}
	switch value.(type) {
	case map[string]any, []any:
		return persistedSecretValuePresent(value, depth+1)
	}
	return false
}

func isPersistedSecretContainer(key string) bool {
	return key == "env" || key == "headers" || key == "secrets" || key == "credentials"
}

func persistedContainerHasLiteral(value any) bool {
	switch x := value.(type) {
	case map[string]any:
		for _, child := range x {
			if persistedNonEmptyLiteral(child) || persistedContainerHasLiteral(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if persistedNonEmptyLiteral(child) || persistedContainerHasLiteral(child) {
				return true
			}
		}
	}
	return false
}

func persistedNonEmptyLiteral(value any) bool {
	switch x := value.(type) {
	case string:
		x = strings.TrimSpace(x)
		return x != "" && !isPersistedSecretReference(x)
	case nil, map[string]any, []any:
		return false
	default:
		return true
	}
}

func isPersistedSecretReference(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return (strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}")) ||
		strings.HasPrefix(value, "env://") || strings.HasPrefix(value, "secret://") ||
		strings.HasPrefix(value, "vault://") || strings.HasPrefix(value, "ref://")
}

func normalizePersistedSecretKey(key string) string {
	key = strings.TrimSpace(key)
	var out strings.Builder
	for i, r := range key {
		if r == '-' || r == '.' || r == ' ' {
			out.WriteByte('_')
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' {
			prev := rune(key[i-1])
			if prev != '_' && !(prev >= 'A' && prev <= 'Z') {
				out.WriteByte('_')
			}
		}
		out.WriteRune(r)
	}
	return strings.ToLower(out.String())
}

func isPersistedSecretField(key string) bool {
	key = normalizePersistedSecretKey(key)
	for _, marker := range []string{"password", "passwd", "secret", "token", "api_key", "apikey", "private_key", "authorization", "credential"} {
		if key == marker || strings.HasSuffix(key, "_"+marker) {
			return true
		}
	}
	return false
}

func isPersistedSecretDeclaration(key string) bool {
	return strings.HasSuffix(key, "_name") || strings.HasSuffix(key, "_names") ||
		strings.HasSuffix(key, "_ref") || strings.HasSuffix(key, "_refs") || strings.HasPrefix(key, "required_")
}

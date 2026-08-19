package plugin

import (
	"bytes"
	"encoding/json"
	"strings"
)

func rejectPersistedConnectorSecrets(values ...json.RawMessage) error {
	for _, raw := range values {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return ErrUnsafeConnectorData
		}
		if persistedSecretValuePresent(value) {
			return ErrUnsafeConnectorData
		}
	}
	return nil
}

func persistedSecretValuePresent(value any) bool {
	switch x := value.(type) {
	case map[string]any:
		for key, child := range x {
			normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
			if isPersistedSecretContainer(normalized) && persistedContainerHasLiteral(child) {
				return true
			}
			if isPersistedSecretField(normalized) && !isPersistedSecretDeclaration(normalized) && persistedNonEmptyLiteral(child) {
				return true
			}
			if persistedSecretValuePresent(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if persistedSecretValuePresent(child) {
				return true
			}
		}
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

func isPersistedSecretField(key string) bool {
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

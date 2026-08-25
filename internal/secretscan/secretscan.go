// Package secretscan is the single source of truth for the connector
// secret-value policy shared by the service write path and the repository
// persistence guard. Both layers decode a document and call Present, so they can
// never diverge on what counts as an unsafe persisted secret.
//
// Policy: store secret references or required env-var NAMES, never secret
// VALUES. env/headers containers routinely carry harmless literals (REGION,
// Accept), so only secret-shaped values, secret-named keys with literal values,
// or secrets/credentials containers (name->value maps) are rejected. A string
// that looks like a JSON document (leading { or [) inside these documents is
// re-scanned; if it is malformed or carries trailing content the scan fails
// CLOSED, so a truncated payload cannot smuggle a credential past the walk.
package secretscan

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// maxEmbeddedScanDepth bounds re-parsing of JSON documents carried as string
// values (attachment raw_content); deeper nesting fails closed.
const maxEmbeddedScanDepth = 5

// Present reports whether a decoded JSON value carries a persisted secret value
// under the policy above.
func Present(value any) bool { return present(value, nil, 0) }

func present(value any, path []string, depth int) bool {
	switch x := value.(type) {
	case map[string]any:
		for key, child := range x {
			normalizedKey := normalizeKey(key)
			next := append(path, normalizedKey)
			insideSecretValues := len(path) > 0 && (path[len(path)-1] == "secrets" || path[len(path)-1] == "credentials")
			if isDeclaration(normalizedKey) && !insideSecretValues {
				if declarationSafe(normalizedKey, child) {
					continue
				}
				return true
			}
			if isContainer(next) {
				if containerHasValue(child, normalizedKey) {
					return true
				}
				continue
			}
			if isField(normalizedKey) && (nonEmptyLiteral(child) || containerHasValue(child, "secrets")) {
				return true
			}
			if present(child, next, depth) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if present(child, path, depth) {
				return true
			}
		}
	case string:
		return embeddedJSONHasSecret(x, path, depth)
	}
	return false
}

// embeddedJSONHasSecret re-parses string values that carry whole JSON documents
// so secrets cannot be smuggled past the walk inside attachment raw_content. A
// JSON-shaped string (leading { or [) that fails to parse cleanly or carries
// trailing content fails CLOSED — a dropped brace must not skip the scan.
func embeddedJSONHasSecret(text string, path []string, depth int) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return false
	}
	if depth >= maxEmbeddedScanDepth {
		return true
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil || ensureEOF(dec) != nil {
		// Looks like JSON but is malformed or has trailing content: fail closed.
		return true
	}
	switch value.(type) {
	case map[string]any, []any:
		return present(value, path, depth+1)
	}
	return false
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return errors.New("trailing content")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func isContainer(path []string) bool {
	if len(path) == 0 {
		return false
	}
	leaf := path[len(path)-1]
	return leaf == "env" || leaf == "headers" || leaf == "secrets" || leaf == "credentials"
}

func containerHasValue(value any, container string) bool {
	switch x := value.(type) {
	case map[string]any:
		if isDeclarationObject(x) {
			return false
		}
		for key, child := range x {
			normalizedKey := normalizeKey(key)
			// In value-mapping containers, caller-chosen keys may look like
			// declarations (for example CUSTOM_NAME) but their values are secrets.
			if (container == "secrets" || container == "credentials") && nonEmptyLiteral(child) {
				return true
			}
			if isDeclaration(normalizedKey) {
				if declarationSafe(normalizedKey, child) {
					continue
				}
				return true
			}
			if isField(normalizedKey) && nonEmptyLiteral(child) {
				return true
			}
			if isContainer([]string{normalizedKey}) {
				if containerHasValue(child, normalizedKey) {
					return true
				}
				continue
			}
			// A secrets container maps caller-chosen secret names to values. Env and
			// header containers, by contrast, routinely contain harmless literals.
			if (container == "secrets" || container == "credentials") && nonEmptyLiteral(child) {
				return true
			}
			if containerHasValue(child, container) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if (container == "secrets" || container == "credentials") && nonEmptyLiteral(child) {
				return true
			}
			if containerHasValue(child, container) {
				return true
			}
		}
	}
	return false
}

func declarationSafe(key string, value any) bool {
	if strings.HasSuffix(key, "_ref") || strings.HasSuffix(key, "_refs") {
		switch x := value.(type) {
		case string:
			return isReference(x)
		case []any:
			for _, item := range x {
				ref, ok := item.(string)
				if !ok || !isReference(ref) {
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

func isDeclarationObject(value map[string]any) bool {
	if len(value) == 0 {
		return false
	}
	hasNameOrRef := false
	for key, child := range value {
		switch normalizeKey(key) {
		case "name", "key", "env", "description", "required", "optional", "type":
			if _, object := child.(map[string]any); object {
				return false
			}
			if _, array := child.([]any); array {
				return false
			}
			if normalizeKey(key) == "name" || normalizeKey(key) == "key" || normalizeKey(key) == "env" {
				hasNameOrRef = true
			}
		case "ref", "reference", "source":
			s, ok := child.(string)
			if !ok || !isReference(s) {
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
		return x != "" && !isReference(x)
	case nil:
		return false
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func isReference(v string) bool {
	lower := strings.ToLower(strings.TrimSpace(v))
	return (strings.HasPrefix(lower, "${") && strings.HasSuffix(lower, "}")) ||
		strings.HasPrefix(lower, "env://") || strings.HasPrefix(lower, "secret://") ||
		strings.HasPrefix(lower, "vault://") || strings.HasPrefix(lower, "ref://")
}

func normalizeKey(key string) string {
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

// descriptorSuffixes are trailing words that describe an auth mechanism rather
// than carry it (auth_type: "none", token_format: "jwt"); such keys hold
// metadata, not credential values.
var descriptorSuffixes = map[string]struct{}{
	"type": {}, "mode": {}, "method": {}, "scheme": {}, "kind": {},
	"enabled": {}, "required": {}, "format": {}, "provider": {}, "status": {}, "version": {},
}

func isField(key string) bool {
	key = normalizeKey(key)
	parts := strings.FieldsFunc(key, func(r rune) bool { return r == '_' })
	if len(parts) > 1 {
		if _, descriptor := descriptorSuffixes[parts[len(parts)-1]]; descriptor {
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

// isDeclaration matches keys that declare a secret by name or reference
// (secret_name, token_refs, required_*). The stem must itself be secret-shaped:
// generic keys like file_name or member_ref are ordinary data.
func isDeclaration(key string) bool {
	key = normalizeKey(key)
	if strings.HasPrefix(key, "required_") {
		return true
	}
	for _, suffix := range []string{"_name", "_names", "_ref", "_refs"} {
		if strings.HasSuffix(key, suffix) {
			return isField(strings.TrimSuffix(key, suffix))
		}
	}
	return false
}

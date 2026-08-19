package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const secretPlaceholder = "__OCTO_SECRET_PLACEHOLDER__"

var secretFragments = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "access_key", "private_key", "authorization", "cookie"}

// DeterministicID creates stable family-specific IDs for globally shared tables.
func DeterministicID(family, sourceID string) string {
	sum := sha256.Sum256([]byte(family + "\x00" + sourceID))
	return family + "_" + hex.EncodeToString(sum[:])[:32]
}

// PluginID preserves a source ID only when it is globally unique and fits the
// unified column. Colliding IDs are deterministically namespaced by family.
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

func secretShaped(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(key, "-", "_"))
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
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := sanitizeNode(value, ""); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func sanitizeNode(value any, path string) error {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			childPath := path + "/" + key
			if key == "env" || key == "headers" {
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
				if text, ok := child.(string); ok && text != "" && text != secretPlaceholder {
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

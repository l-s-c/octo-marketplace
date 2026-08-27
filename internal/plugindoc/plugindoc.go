// Package plugindoc holds the pure, dependency-free renderers and sanitizers
// that produce the canonical expert/expert_team package documents (AGENTS.md
// entry text, connector MCP config sanitization). Both the deterministic
// backfill (internal/backfill/plugin) and the live admin container import
// (internal/service/plugin) render through this one package so an imported
// expert/team is byte-identical to a backfilled one.
//
// It deliberately depends only on the standard library: the service package
// cannot import the backfill package (backfill already imports the service, so
// that would be an import cycle), and the backfill package cannot be the shared
// home for the same reason. A neutral leaf package is the only seam that lets
// both sides share the exact renderers.
package plugindoc

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const secretPlaceholder = "__OCTO_SECRET_PLACEHOLDER__"

var secretFragments = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "access_key", "private_key", "authorization", "cookie"}

// TeamAgentsMarkdown renders the deterministic AGENTS.md entry document of an
// expert_team package from its squad fields: collaboration prose, leader,
// ordered dispatch strategies, dependencies, and permission. It is the team
// package's only attachment under the contract layout, and every writer
// (backfill, repackage, admin import) must render byte-identical output.
//
// strategies is a decoded JSON array ([]any of strings) and dependencies a
// decoded JSON object (map[string]any with "blocking"/"recommended" arrays);
// callers holding typed slices convert them to those shapes so the rendered
// bytes match the backfill, which decodes them from JSON.
func TeamAgentsMarkdown(name, summary, leader string, strategies, dependencies any, permission string) string {
	var b strings.Builder
	b.WriteString("# " + strings.TrimSpace(name) + "\n")
	if trimmed := strings.TrimSpace(summary); trimmed != "" {
		b.WriteString("\n" + trimmed + "\n")
	}
	b.WriteString("\n## 协作方式\n")
	if trimmed := strings.TrimSpace(leader); trimmed != "" {
		b.WriteString("\n- Leader: " + trimmed + "\n")
	}
	if lines := StringItems(strategies); len(lines) > 0 {
		b.WriteString("\n### 策略\n")
		for i, line := range lines {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, line))
		}
	}
	if deps, ok := dependencies.(map[string]any); ok {
		blocking := StringItems(deps["blocking"])
		recommended := StringItems(deps["recommended"])
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

// EntryMarkdown renders the minimal deterministic entry document (used when a
// package must carry its contract entry file but the source has no authored
// text).
func EntryMarkdown(name, summary string) string {
	var b strings.Builder
	b.WriteString("# " + strings.TrimSpace(name) + "\n")
	if trimmed := strings.TrimSpace(summary); trimmed != "" {
		b.WriteString("\n" + trimmed + "\n")
	}
	return b.String()
}

// ExpertAgentsMarkdown picks the expert package's AGENTS.md entry document: the
// instruction verbatim when present, else a minimal deterministic document from
// the display fields (the contract requires the entry file).
func ExpertAgentsMarkdown(name, summary, instruction string) string {
	if trimmed := strings.TrimSpace(instruction); trimmed != "" {
		return instruction
	}
	return EntryMarkdown(name, summary)
}

// StringItems extracts the non-blank string entries of a decoded JSON array.
func StringItems(value any) []string {
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

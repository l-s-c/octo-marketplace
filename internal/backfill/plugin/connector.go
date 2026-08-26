package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
)

// legacyConnection is the camelCase MCP connection shape stored by the legacy
// mcp_servers.config_json column (and inside the retired connector/config.json
// attachment as its "config" member).
type legacyConnection struct {
	URL                 string            `json:"url"`
	Command             string            `json:"command"`
	Args                []string          `json:"args"`
	Env                 map[string]string `json:"env"`
	EnvUserSupplied     []string          `json:"envUserSupplied"`
	Headers             map[string]string `json:"headers"`
	HeadersUserSupplied []string          `json:"headersUserSupplied"`
	AuthType            string            `json:"authType"`
	ServerName          string            `json:"serverName"`
}

// placeholderFor renders the install-time injection marker for one
// user-supplied env/header key: ${KEY}. The scanners on both write paths treat
// ${...} as a reference, never a stored secret.
func placeholderFor(key string) string { return "${" + envPlaceholderName(key) + "}" }

// envPlaceholderName normalizes a key into the environment-variable style name
// used inside the ${...} placeholder (Authorization -> AUTHORIZATION,
// X-API-Key -> X_API_KEY).
func envPlaceholderName(key string) string {
	var out strings.Builder
	out.Grow(len(key))
	for _, r := range strings.TrimSpace(key) {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r - 'a' + 'A')
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "VALUE"
	}
	return out.String()
}

// connectorMCPDocument converts one sanitized legacy connection into the
// standard mcp.json document: {"mcpServers": {<serverName>: {type, url,
// command, args, env, headers}}}. User-supplied keys carry ${KEY}
// placeholders; a legacy bearer authType folds into an Authorization header
// placeholder. If the sanitized config already IS an mcpServers document it is
// passed through untouched.
func connectorMCPDocument(sanitizedConfig []byte, transport, fallbackName string) (map[string]any, error) {
	var probe map[string]any
	if err := json.Unmarshal(sanitizedConfig, &probe); err != nil {
		return nil, fmt.Errorf("invalid connector config: %w", err)
	}
	if _, alreadyStandard := probe["mcpServers"]; alreadyStandard {
		return probe, nil
	}
	var conn legacyConnection
	if err := json.Unmarshal(sanitizedConfig, &conn); err != nil {
		return nil, fmt.Errorf("invalid connector connection: %w", err)
	}
	entry := map[string]any{}
	if strings.TrimSpace(transport) != "" {
		entry["type"] = strings.TrimSpace(transport)
	}
	if conn.URL != "" {
		entry["url"] = conn.URL
	}
	if conn.Command != "" {
		entry["command"] = conn.Command
	}
	if len(conn.Args) > 0 {
		entry["args"] = conn.Args
	}
	env := valueMapWithPlaceholders(conn.Env, conn.EnvUserSupplied)
	if len(env) > 0 {
		entry["env"] = env
	}
	headers := valueMapWithPlaceholders(conn.Headers, conn.HeadersUserSupplied)
	if strings.EqualFold(strings.TrimSpace(conn.AuthType), "bearer") {
		if headers == nil {
			headers = map[string]any{}
		}
		if _, exists := headers["Authorization"]; !exists {
			headers["Authorization"] = placeholderFor("Authorization")
		}
	}
	if len(headers) > 0 {
		entry["headers"] = headers
	}
	serverName := strings.TrimSpace(conn.ServerName)
	if serverName == "" {
		serverName = strings.TrimSpace(fallbackName)
	}
	if serverName == "" {
		serverName = "server"
	}
	return map[string]any{"mcpServers": map[string]any{serverName: entry}}, nil
}

// valueMapWithPlaceholders copies an env/header map, substituting ${KEY}
// placeholders for the keys the consumer must fill locally.
func valueMapWithPlaceholders(values map[string]string, userSupplied []string) map[string]any {
	if len(values) == 0 && len(userSupplied) == 0 {
		return nil
	}
	out := make(map[string]any, len(values)+len(userSupplied))
	for key, value := range values {
		out[key] = value
	}
	for _, key := range userSupplied {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = placeholderFor(key)
	}
	return out
}

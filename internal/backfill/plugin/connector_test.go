package plugin

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestConnectorMCPDocumentConvertsLegacyConnection(t *testing.T) {
	sanitized := []byte(`{"url":"https://mcp.example.com/mcp","args":["--fast"],"command":"","env":{"REGION":""},"envUserSupplied":["WORKSPACE_ID"],"headers":{"X-Trace":""},"headersUserSupplied":["X-API-Key"],"authType":"bearer","serverName":"jira"}`)
	doc, err := connectorMCPDocument(sanitized, "streamable-http", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"mcpServers": map[string]any{
			"jira": map[string]any{
				"type": "streamable-http",
				"url":  "https://mcp.example.com/mcp",
				"args": []string{"--fast"},
				"env":  map[string]any{"REGION": "", "WORKSPACE_ID": "${WORKSPACE_ID}"},
				"headers": map[string]any{
					"X-Trace":       "",
					"X-API-Key":     "${X_API_KEY}",
					"Authorization": "${AUTHORIZATION}",
				},
			},
		},
	}
	// Compare via canonical JSON so []string vs []any shapes don't matter.
	gotJSON, _ := json.Marshal(doc)
	wantJSON, _ := json.Marshal(want)
	var gotValue, wantValue any
	_ = json.Unmarshal(gotJSON, &gotValue)
	_ = json.Unmarshal(wantJSON, &wantValue)
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("doc = %s, want %s", gotJSON, wantJSON)
	}
}

func TestConnectorMCPDocumentFallbacksAndPassthrough(t *testing.T) {
	// A config that already carries mcpServers is passed through untouched.
	standard := []byte(`{"mcpServers":{"octo":{"command":"octo-mcp"}}}`)
	doc, err := connectorMCPDocument(standard, "stdio", "fallback")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	if string(raw) != `{"mcpServers":{"octo":{"command":"octo-mcp"}}}` {
		t.Fatalf("passthrough mangled: %s", raw)
	}
	// Empty serverName falls back to the provided name.
	doc, err = connectorMCPDocument([]byte(`{"command":"run"}`), "stdio", "my-connector")
	if err != nil {
		t.Fatal(err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if _, ok := servers["my-connector"]; !ok {
		t.Fatalf("fallback server key missing: %v", servers)
	}
}

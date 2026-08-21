package plugin

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestRejectPersistedSecretValuesScansEmbeddedJSON(t *testing.T) {
	bad := []string{
		`{"attachments":[{"path":"connector/config.json","content_type":"raw","mime_type":"application/json","raw_content":` + strconv.Quote(`{"env":{"API_KEY":"sk-live-value"}}`) + `}]}`,
		`{"attachments":[{"raw_content":` + strconv.Quote(`{"wrapper":`+strconv.Quote(`{"headers":{"Authorization":"Bearer abc"}}`)+`}`) + `}]}`,
		`{"config":{"credentials":{"CUSTOM":"plain-token"}}}`,
	}
	for _, doc := range bad {
		if err := rejectPersistedSecretValues(json.RawMessage(doc)); err == nil {
			t.Fatalf("doc %s accepted, want ErrUnsafeConnectorData", doc)
		}
	}
	good := []string{
		`{"attachments":[{"path":"skill/README.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# not json {"}]}`,
		`{"attachments":[{"raw_content":` + strconv.Quote(`{"env":{"API_KEY":"${API_KEY}"},"region":"us-east-1"}`) + `}]}`,
		`{"required_secret_names":["API_TOKEN"]}`,
		// The mcp.json shape: ${KEY} placeholders mark user-supplied env/header
		// values injected at install time — references, not secret literals.
		`{"attachments":[{"path":"mcp.json","content_type":"raw","mime_type":"application/json","raw_content":` + strconv.Quote(`{"mcpServers":{"jira":{"type":"streamable-http","url":"https://mcp.example.com/mcp","headers":{"Authorization":"${TOKEN}","X-API-Key":"${JIRA_API_KEY}"},"env":{"WORKSPACE":"${WORKSPACE_ID}"}}}}`) + `}]}`,
	}
	for _, doc := range good {
		if err := rejectPersistedSecretValues(json.RawMessage(doc)); err != nil {
			t.Fatalf("doc %s rejected: %v", doc, err)
		}
	}
}

func TestRejectPersistedSecretValuesFailsClosedOnDeepNesting(t *testing.T) {
	payload := `{"note":"harmless"}`
	for i := 0; i < maxPersistedSecretScanDepth+1; i++ {
		payload = `{"nested":` + strconv.Quote(payload) + `}`
	}
	if err := rejectPersistedSecretValues(json.RawMessage(payload)); err == nil {
		t.Fatal("pathological nesting accepted, want ErrUnsafeConnectorData")
	}
}

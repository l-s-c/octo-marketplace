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
	for i := 0; i < 7; i++ {
		payload = `{"nested":` + strconv.Quote(payload) + `}`
	}
	if err := rejectPersistedSecretValues(json.RawMessage(payload)); err == nil {
		t.Fatal("pathological nesting accepted, want ErrUnsafeConnectorData")
	}
}

// TestRejectPersistedSecretValuesAcceptsServiceGoodCorpus pins the repo scanner
// to the service scanner: documents the service write path locks as VALID must
// not be rejected at the persistence layer. These two are the exact documents
// the service contract test defines as good; before the scanners were unified
// the repo layer 400'd them (an ordinary env/header literal like REGION/Accept).
func TestRejectPersistedSecretValuesAcceptsServiceGoodCorpus(t *testing.T) {
	good := []string{
		`{"required_secret_names":["API_TOKEN"],"config":{"env":{"API_TOKEN":"","REGION":"us-east-1"}}}`,
		`{"config":{"env":{"API_TOKEN":"${API_TOKEN}"},"headers":{"Authorization":"secret://auth-header","Accept":"application/json"}}}`,
	}
	for _, doc := range good {
		if err := rejectPersistedSecretValues(json.RawMessage(doc)); err != nil {
			t.Fatalf("service-valid doc rejected by repo scanner: %s\n%v", doc, err)
		}
	}
}

// TestRejectPersistedSecretValuesFailsClosedOnMalformedEmbeddedJSON is the ④
// fix: a JSON-shaped attachment string that is truncated (dropped brace) must
// NOT skip the scan — the malformed document fails closed so a credential
// hidden behind a parse error cannot be persisted.
func TestRejectPersistedSecretValuesFailsClosedOnMalformedEmbeddedJSON(t *testing.T) {
	// A well-formed version of this would be scanned and rejected (secret literal
	// in env). Truncated, the old scanner skipped it and persisted the secret.
	doc := `{"attachments":[{"path":"mcp.json","content_type":"raw","mime_type":"application/json","raw_content":` +
		strconv.Quote(`{"env":{"API_KEY":"sk-live-value"`) + `}]}`
	if err := rejectPersistedSecretValues(json.RawMessage(doc)); err == nil {
		t.Fatal("malformed embedded JSON accepted, want fail-closed ErrUnsafeConnectorData")
	}
}

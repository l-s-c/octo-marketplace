package secretscan

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, doc string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	return v
}

// TestPresentAllowsFreeTextStartingWithBracket is the P0-2 regression: a manifest
// description or a markdown attachment that merely starts with [ or { (a "[Draft]"
// tag, a badge link, a mustache template) is not JSON and must not be rejected.
func TestPresentAllowsFreeTextStartingWithBracket(t *testing.T) {
	good := []string{
		`{"description":"[Draft] An example plugin."}`,
		`{"description":"{beta} example"}`,
		`{"attachments":[{"path":"SKILL.md","mime_type":"text/markdown","raw_content":"[![Build](https://x/y.svg)](https://x)"}]}`,
		`{"attachments":[{"path":"prompt.md","mime_type":"text/markdown","raw_content":"{{ user.name }} hello"}]}`,
		`{"attachments":[{"path":"data.jsonl","mime_type":"application/jsonl","raw_content":"{\"a\":1}\n{\"b\":2}"}]}`,
		`{"attachments":[{"path":"cfg.jsonc","mime_type":"text/plain","raw_content":"{ // comment\n\"a\":1}"}]}`,
	}
	for _, doc := range good {
		if Present(decode(t, doc)) {
			t.Fatalf("free text rejected as secret: %s", doc)
		}
	}
}

// TestPresentFailsClosedOnTruncatedDeclaredJSON keeps the fail-closed intent: a
// declared-JSON payload (mime application/json or a .json path) that is truncated
// must not skip the scan.
func TestPresentFailsClosedOnTruncatedDeclaredJSON(t *testing.T) {
	bad := []string{
		`{"attachments":[{"path":"mcp.json","mime_type":"application/json","raw_content":"{\"env\":{\"API_KEY\":\"sk-live-value\""}]}`,
		`{"attachments":[{"path":"connector/config.json","mime_type":"application/json","raw_content":"[{\"token\":\"x\""}]}`,
	}
	for _, doc := range bad {
		if !Present(decode(t, doc)) {
			t.Fatalf("truncated declared-JSON accepted (should fail closed): %s", doc)
		}
	}
}

// TestPresentScansValidEmbeddedJSONRegardlessOfDeclaration: a raw_content that
// genuinely parses as JSON is scanned even without a JSON mime, so an embedded
// secret cannot hide behind a missing declaration.
func TestPresentScansValidEmbeddedJSON(t *testing.T) {
	doc := `{"attachments":[{"raw_content":"{\"env\":{\"API_KEY\":\"sk-live-value\"}}"}]}`
	if !Present(decode(t, doc)) {
		t.Fatalf("valid embedded JSON secret not caught: %s", doc)
	}
}

// TestPresentEnvHeadersLenientButSecretShaped: env/headers allow harmless
// literals (Accept, REGION) but reject secret-NAMED keys carrying literals.
func TestPresentEnvHeadersLenient(t *testing.T) {
	good := []string{
		`{"env":{"REGION":"us-east-1","NODE_ENV":"production"}}`,
		`{"headers":{"Accept":"application/json","Content-Type":"application/json"}}`,
		`{"env":{"API_TOKEN":"${API_TOKEN}"},"headers":{"Authorization":"secret://auth"}}`,
	}
	for _, doc := range good {
		if Present(decode(t, doc)) {
			t.Fatalf("harmless env/header literal rejected: %s", doc)
		}
	}
	bad := []string{
		`{"env":{"API_TOKEN":"sk-live-actual-value"}}`,
		`{"headers":{"Authorization":"Bearer realtoken"}}`,
	}
	for _, doc := range bad {
		if !Present(decode(t, doc)) {
			t.Fatalf("secret-named env/header literal accepted: %s", doc)
		}
	}
}

// TestPresentEnvHeadersCatchesPrefixedValueUnderOrdinaryKey is the r8/r9
// regression: a credential-shaped VALUE (known prefix) must be caught even when
// the KEY name is ordinary and would otherwise slip the key-name check —
// OPENAI_KEY / GH_PAT / X-Custom carrying sk-/ghp_/xoxb- tokens. The
// prefix-only heuristic must NOT re-flag ordinary long values (UUIDs, region
// strings), which option A deliberately allows.
func TestPresentEnvHeadersCatchesPrefixedValueUnderOrdinaryKey(t *testing.T) {
	bad := []string{
		`{"env":{"OPENAI_KEY":"sk-proj-abcdef0123456789"}}`,
		`{"env":{"GH_PAT":"ghp_abcdef0123456789"}}`,
		`{"env":{"ANTHROPIC":"sk-ant-abcdef0123456789"}}`,
		`{"headers":{"X-Custom":"xoxb-1111-2222-abcdef"}}`,
	}
	for _, doc := range bad {
		if !Present(decode(t, doc)) {
			t.Fatalf("prefixed credential under ordinary key accepted: %s", doc)
		}
	}
	good := []string{
		`{"env":{"BUILD_ID":"a1b2c3d4-e5f6-7890-abcd-ef1234567890"}}`, // UUID, not a credential prefix
		`{"env":{"REGION":"us-east-1","IMAGE_TAG":"v2.0.0-rc1-longbuildhash1234"}}`,
		`{"headers":{"User-Agent":"octo-marketplace/1.0 (+https://example.com)"}}`,
	}
	for _, doc := range good {
		if Present(decode(t, doc)) {
			t.Fatalf("ordinary long env/header value over-flagged: %s", doc)
		}
	}
}

// TestPresentStrictContainers: secrets/credentials reject any non-reference
// literal, including string-valued containers (finding 2) and a {name,key}
// declaration whose key value is itself a secret (P1-2). A genuine declaration
// naming a secret still passes.
func TestPresentStrictContainers(t *testing.T) {
	bad := []string{
		`{"secrets":"sk-live-value"}`,
		`{"credentials":"plain-token"}`,
		`{"config":{"credentials":{"CUSTOM":"plain-token"}}}`,
		`{"credentials":{"key":"sk-live-actual-secret"}}`,
		`{"credentials":{"name":"db","key":"AKIAIOSFODNN7EXAMPLE"}}`,
	}
	for _, doc := range bad {
		if !Present(decode(t, doc)) {
			t.Fatalf("strict-container secret accepted: %s", doc)
		}
	}
	good := []string{
		`{"secrets":[{"name":"API_TOKEN","description":"token","required":true},{"ref":"secret://API_TOKEN"}]}`,
		`{"credentials":{"name":"API_TOKEN","ref":"secret://API_TOKEN"}}`,
	}
	for _, doc := range good {
		if Present(decode(t, doc)) {
			t.Fatalf("legitimate secret declaration rejected: %s", doc)
		}
	}
}

// TestPresentDeclarationValueSmuggling: a required_*/_name declaration whose value
// is a credential (not a name) is rejected (finding 4).
func TestPresentDeclarationValueSmuggling(t *testing.T) {
	bad := []string{
		`{"required_token":"sk-live-abcdefghijklmnop"}`,
		`{"secret_names":["ghp_abcdef0123456789ABCDEF"]}`,
	}
	for _, doc := range bad {
		if !Present(decode(t, doc)) {
			t.Fatalf("declaration smuggling a secret value accepted: %s", doc)
		}
	}
	good := []string{
		`{"required_secret_names":["API_TOKEN","JIRA_KEY"]}`,
		`{"secret_name":"DATABASE_PASSWORD"}`,
	}
	for _, doc := range good {
		if Present(decode(t, doc)) {
			t.Fatalf("legitimate name declaration rejected: %s", doc)
		}
	}
}

// TestPresentReferenceGrammar: only a well-formed ${KEY} (or scheme:// with a
// non-empty tail) is a reference; a degenerate ${} or a spaced ${Bearer …} is not
// and stays subject to the scan (finding 5).
func TestPresentReferenceGrammar(t *testing.T) {
	// ${KEY} placeholders (first char may be a digit) are references — accepted
	// even under a secret-named key.
	good := []string{
		`{"env":{"TOKEN":"${TOKEN}"}}`,
		`{"env":{"TOKEN":"${1ST_TOKEN}"}}`,
	}
	for _, doc := range good {
		if Present(decode(t, doc)) {
			t.Fatalf("valid ${KEY} reference rejected: %s", doc)
		}
	}
	// A spaced/degenerate "reference" under a secret-named key is a literal value.
	bad := []string{
		`{"env":{"AUTH_TOKEN":"${Bearer eyJhbGciOi}"}}`,
		`{"headers":{"Authorization":"secret://"}}`,
	}
	for _, doc := range bad {
		if !Present(decode(t, doc)) {
			t.Fatalf("degenerate reference accepted under secret key: %s", doc)
		}
	}
}

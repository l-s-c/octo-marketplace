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
	"regexp"
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
		// An attachment object (mime_type application/json or a .json path)
		// declares its raw_content as JSON; a truncated declared-JSON payload
		// fails closed, while free-text raw_content (markdown, prose) does not.
		declaredJSON := declaresJSON(x)
		for key, child := range x {
			normalizedKey := normalizeKey(key)
			// raw_content is the attachment payload: scan it as embedded JSON, but
			// pass whether the attachment declared JSON so a "[Draft]" description
			// or a "{{template}}" markdown body (parse failure, not declared JSON)
			// is not rejected (P0-2).
			if key == "raw_content" {
				if s, ok := child.(string); ok {
					if embeddedJSONHasSecret(s, declaredJSON, append(path, normalizedKey), depth) {
						return true
					}
					continue
				}
			}
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
		// A naked string that is not attachment raw_content carries no JSON
		// declaration (a manifest description, a nested field). Only scan it when
		// it genuinely parses as a JSON document; a parse failure is treated as
		// free text and accepted.
		return embeddedJSONHasSecret(x, false, path, depth)
	}
	return false
}

// declaresJSON reports whether an attachment-like object declares JSON content,
// so a truncated/trailing raw_content fails closed (a dropped brace in mcp.json
// must not skip the scan) while free-text markdown/prose does not.
func declaresJSON(x map[string]any) bool {
	if mt, ok := x["mime_type"].(string); ok {
		mt = strings.ToLower(strings.TrimSpace(mt))
		if mt == "application/json" || strings.HasSuffix(mt, "+json") {
			return true
		}
	}
	if p, ok := x["path"].(string); ok {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(p)), ".json") {
			return true
		}
	}
	return false
}

// embeddedJSONHasSecret re-parses string values that carry whole JSON documents
// so secrets cannot be smuggled past the walk inside attachment raw_content. A
// string that parses cleanly as a JSON object/array is scanned recursively. A
// JSON-shaped string (leading { or [) that fails to parse or carries trailing
// content fails CLOSED only when declaredJSON is set — a truncated mcp.json must
// not skip the scan, but free text that merely starts with [ or { (a markdown
// "[Draft]", a "{{template}}") is not JSON and is accepted.
func embeddedJSONHasSecret(text string, declaredJSON bool, path []string, depth int) bool {
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
		return declaredJSON
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
	strictContainer := container == "secrets" || container == "credentials"
	switch x := value.(type) {
	case map[string]any:
		// The declaration-object exemption ({name, ref, ...}) applies in any
		// container, but isDeclarationObject refuses it when a name/key value is
		// itself secret-shaped — so {"credentials":{"key":"sk-live-…"}} is not
		// waved through as a harmless declaration (P1-2).
		if isDeclarationObject(x) {
			return false
		}
		for key, child := range x {
			normalizedKey := normalizeKey(key)
			// In value-mapping containers, caller-chosen keys may look like
			// declarations (for example CUSTOM_NAME) but their values are secrets.
			if strictContainer && nonEmptyLiteral(child) {
				return true
			}
			// A scalar VALUE carrying a known credential prefix is a secret under
			// ANY key name — env/headers allow ordinary literals but not a leaked
			// "sk-live-…"/"ghp_…" the key name (OPENAI_KEY, GH_PAT, DB_PASS) would
			// otherwise let slip past the key-name check below.
			if s, ok := child.(string); ok && looksLikeCredentialValue(s) {
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
			if strictContainer && nonEmptyLiteral(child) {
				return true
			}
			if containerHasValue(child, container) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if strictContainer && nonEmptyLiteral(child) {
				return true
			}
			if s, ok := child.(string); ok && looksLikeCredentialValue(s) {
				return true
			}
			if containerHasValue(child, container) {
				return true
			}
		}
	case string:
		// A container mapping directly to a scalar string (e.g. {"secrets":"..."})
		// — a string child is never a declaration object, so scan it directly. For
		// the strict containers any non-reference literal is a secret value; env
		// and header string values stay lenient EXCEPT a known credential prefix.
		return (strictContainer && nonEmptyLiteral(x)) || looksLikeCredentialValue(x)
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
			// A declaration names a secret; its value must be a name/identifier,
			// not the secret itself. A value that looks like a credential
			// (required_token: "sk-live-…") is a smuggled literal, not a name.
			return strings.TrimSpace(x) != "" && !looksLikeSecretValue(x)
		case []any:
			for _, item := range x {
				name, ok := item.(string)
				if !ok || strings.TrimSpace(name) == "" || looksLikeSecretValue(name) {
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

// secretValuePrefixes are well-known credential prefixes; a declaration value
// carrying one is a smuggled secret, not a name/identifier.
var secretValuePrefixes = []string{
	"sk-", "sk_", "pk_live", "rk_live", "ghp_", "gho_", "ghs_", "ghu_", "github_pat_",
	"xoxb-", "xoxp-", "xapp-", "xoxa-", "akia", "asia", "aiza", "ya29.", "eyj", "bearer ",
	"glpat-", "npm_", "dop_v1_", "shpat_", "shppa_", "sq0atp-", "sk-live", "sk-proj-",
}

// looksLikeSecretValue is a conservative heuristic: a value carrying a known
// credential prefix, or a long opaque high-entropy token, is a secret value
// rather than a name/reference. Short identifiers like API_TOKEN pass.
func looksLikeSecretValue(v string) bool {
	s := strings.TrimSpace(v)
	if s == "" || isReference(s) {
		return false
	}
	if hasCredentialPrefix(s) {
		return true
	}
	// A long single token with mixed character classes is opaque credential
	// material, not a human-readable name (which stays short or spaced).
	if len(s) >= 24 && !strings.ContainsAny(s, " \t\r\n") && mixedCharClasses(s) {
		return true
	}
	return false
}

// hasCredentialPrefix reports whether a value begins with a well-known
// credential prefix. It is used ONLY on the declaration-NAME side (via
// looksLikeSecretValue), where the surface is narrow and a short stem is safe.
// For arbitrary config VALUES use looksLikeCredentialValue instead — a short
// stem there collides with ordinary config ("Asia/Shanghai", "sk-SK",
// "npm_config_registry").
func hasCredentialPrefix(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" || isReference(s) {
		return false
	}
	for _, p := range secretValuePrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// credentialValueShapes are anchored, high-confidence credential SHAPES for
// scanning arbitrary config VALUES under env/headers. Unlike the name-side
// short-stem list, each requires enough structure (a fixed prefix plus a
// minimum body of the right character class) that it does not collide with
// ordinary configuration such as "Asia/Shanghai", "sk-SK",
// "npm_config_registry", locale codes, UUIDs, or version strings.
var credentialValueShapes = []*regexp.Regexp{
	regexp.MustCompile(`^(AKIA|ASIA)[A-Z0-9]{16}$`),                                 // AWS access key id
	regexp.MustCompile(`^AIza[0-9A-Za-z_-]{35}$`),                                   // Google API key
	regexp.MustCompile(`^gh[pousr]_[A-Za-z0-9]{20,255}$`),                           // GitHub token
	regexp.MustCompile(`^github_pat_[A-Za-z0-9_]{30,255}$`),                         // GitHub fine-grained PAT
	regexp.MustCompile(`^glpat-[A-Za-z0-9_-]{20,}$`),                                // GitLab PAT
	regexp.MustCompile(`^xox[baprs]-[A-Za-z0-9-]{10,}$`),                            // Slack
	regexp.MustCompile(`^xapp-[A-Za-z0-9-]{10,}$`),                                  // Slack app-level
	regexp.MustCompile(`^(sk|pk|rk)_live_[A-Za-z0-9]{16,}$`),                        // Stripe live
	regexp.MustCompile(`^sk-[A-Za-z0-9_-]{20,}$`),                                   // OpenAI / Anthropic sk-
	regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+$`), // JWT (three base64url segments)
	regexp.MustCompile(`^ya29\.[A-Za-z0-9_-]{20,}$`),                                // Google OAuth access token
	regexp.MustCompile(`^dop_v1_[a-f0-9]{64}$`),                                     // DigitalOcean
	regexp.MustCompile(`^shp(at|pa)_[a-f0-9]{32}$`),                                 // Shopify
	regexp.MustCompile(`^sq0atp-[A-Za-z0-9_-]{22,}$`),                               // Square
	regexp.MustCompile(`^npm_[A-Za-z0-9]{36}$`),                                     // npm automation token
}

// looksLikeCredentialValue reports whether a config VALUE matches a full,
// anchored credential shape. It deliberately requires the whole token to match
// (not just a prefix), so it catches a real "sk-…"/"ghp_…"/AWS/JWT credential
// leaked under an ordinary env/header key while leaving ordinary literals
// (regions, timezones, locale codes, UUIDs, version tags) writable under
// option A. It does NOT use the entropy branch, which would over-flag UUIDs.
func looksLikeCredentialValue(v string) bool {
	s := strings.TrimSpace(v)
	if s == "" || isReference(s) {
		return false
	}
	for _, re := range credentialValueShapes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func mixedCharClasses(s string) bool {
	var hasUpper, hasLower, hasDigit bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	classes := 0
	for _, ok := range []bool{hasUpper, hasLower, hasDigit} {
		if ok {
			classes++
		}
	}
	return classes >= 2
}

var envRefPattern = regexp.MustCompile(`^\$\{[A-Za-z0-9_]+\}$`)

func isReference(v string) bool {
	s := strings.TrimSpace(v)
	// ${KEY}: the frontend placeholder grammar (first char may be a digit, to
	// match placeholderFor/splitUserSupplied). An empty ${} or a spaced
	// "${Bearer eyJ…}" is NOT a reference and stays subject to the scan.
	if envRefPattern.MatchString(s) {
		return true
	}
	lower := strings.ToLower(s)
	for _, scheme := range []string{"env://", "secret://", "vault://", "ref://"} {
		if strings.HasPrefix(lower, scheme) && strings.TrimSpace(s[len(scheme):]) != "" {
			return true
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
			// A declaration names a secret; a name/key/env whose value is itself a
			// credential (sk-live-…) is smuggling the secret, not declaring it.
			if s, ok := child.(string); ok && looksLikeSecretValue(s) {
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

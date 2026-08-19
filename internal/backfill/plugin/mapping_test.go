package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPluginIDPreservesOnlyGloballyUnique(t *testing.T) {
	if got := PluginID("skill", "same", 1); got != "same" {
		t.Fatalf("unique ID = %q", got)
	}
	a := PluginID("skill", "same", 2)
	b := PluginID("expert", "same", 2)
	if a == "same" || a == b || a != PluginID("skill", "same", 2) {
		t.Fatalf("prefixed IDs not stable/distinct: %q %q", a, b)
	}
}

func TestSanitizeConnectorJSONBlanksEnvAndHeaders(t *testing.T) {
	got, err := SanitizeConnectorJSON([]byte(`{"url":"https://example.invalid","env":{"TOKEN":"actual"},"headers":{"Authorization":"Bearer actual"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(got, &value); err != nil {
		t.Fatal(err)
	}
	if value["env"].(map[string]any)["TOKEN"] != "" || value["headers"].(map[string]any)["Authorization"] != "" {
		t.Fatalf("secret maps were not blanked: %s", got)
	}
	if strings.Contains(string(got), "actual") {
		t.Fatalf("secret leaked: %s", got)
	}
}

func TestSanitizeConnectorJSONRejectsSecretShapedValue(t *testing.T) {
	if _, err := SanitizeConnectorJSON([]byte(`{"nested":{"api_key":"actual"}}`)); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestNamesFromTagIDs(t *testing.T) {
	got, err := namesFromTagIDs([]byte(`[2,1,2]`), map[int64]string{1: "one", 2: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "two,one" {
		t.Fatalf("names = %#v", got)
	}
	if _, err := namesFromTagIDs([]byte(`[3]`), map[int64]string{}); err == nil {
		t.Fatal("unknown ID accepted")
	}
}

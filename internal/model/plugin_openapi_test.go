package model

import (
	"os"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGeneratedOpenAPIPluginEnumsAreUnique(t *testing.T) {
	raw, err := os.ReadFile("../../docs/openapi/swagger.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	const prefix = "github_com_Mininglamp-OSS_octo-marketplace_internal_model."
	for name, want := range map[string][]string{
		"PluginType":       {"expert", "expert_team", "skill", "connector"},
		"PluginVisibility": {"public", "space", "private", "system"},
	} {
		schema, ok := spec.Components.Schemas[prefix+name]
		if !ok {
			t.Fatalf("generated OpenAPI schema %s is missing", name)
		}
		if !reflect.DeepEqual(schema.Enum, want) {
			t.Errorf("generated OpenAPI schema %s enum = %v, want unique wire values %v", name, schema.Enum, want)
		}
	}
}

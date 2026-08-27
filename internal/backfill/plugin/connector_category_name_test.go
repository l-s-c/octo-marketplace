package plugin

import "testing"

// The legacy MCP category slugs must localize to the display names stored
// directly in plugin_categories (the web renders the stored name as-is); an
// unmapped slug passes through unchanged.
func TestConnectorCategoryNameLocalizesKnownSlugs(t *testing.T) {
	cases := map[string]string{
		"dev":          "开发工具",
		"data":         "数据服务",
		"search":       "搜索检索",
		"productivity": "效率协作",
		"ai":           "AI能力",
		"unmapped":     "unmapped",
		"":             "",
	}
	for key, want := range cases {
		if got := connectorCategoryName(key); got != want {
			t.Errorf("connectorCategoryName(%q) = %q, want %q", key, got, want)
		}
	}
}

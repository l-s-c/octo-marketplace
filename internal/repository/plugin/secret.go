package plugin

import (
	"bytes"
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/secretscan"
)

// rejectPersistedSecretValues is the repository persistence guard against
// storing connector secret VALUES. It shares the exact walk and policy with the
// service write path via internal/secretscan, so the two layers can never
// disagree on what is a persisted secret (a divergence would 400 legitimate
// connector writes or leak a credential past one layer).
func rejectPersistedSecretValues(values ...json.RawMessage) error {
	for _, raw := range values {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return ErrUnsafeConnectorData
		}
		if secretscan.Present(value) {
			return ErrUnsafeConnectorData
		}
	}
	return nil
}

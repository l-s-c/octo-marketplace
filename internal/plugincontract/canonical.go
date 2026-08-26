package plugincontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CanonicalJSON produces UTF-8 JSON with sorted object keys and exact decimal
// number normalization. Unlike float-based encoders it does not lose integer
// precision. Duplicate keys and impractically large numeric exponents fail.
func CanonicalJSON(raw []byte) ([]byte, error) {
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := appendCanonical(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// ComputePluginHash implements the frozen sha256(manifest_json + plugin_json)
// contract over canonical JSON bytes.
func ComputePluginHash(manifestJSON, pluginJSON []byte) (string, error) {
	manifest, err := CanonicalJSON(manifestJSON)
	if err != nil {
		return "", withPath(err, "manifest_json")
	}
	if isJSONNull(pluginJSON) {
		pluginJSON = []byte("null")
	}
	packageValue, err := CanonicalJSON(pluginJSON)
	if err != nil {
		return "", withPath(err, "plugin_json")
	}
	hash := sha256.New()

	hash.Write(manifest)
	hash.Write(packageValue)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func appendCanonical(output *bytes.Buffer, value any) error {
	switch current := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		output.WriteString(strconv.FormatBool(current))
	case string:
		encoded, _ := json.Marshal(current)
		output.Write(encoded)
	case json.Number:
		number, err := canonicalNumber(current.String())
		if err != nil {
			return invalid(CodeInvalidJSON, "", err.Error())
		}
		output.WriteString(number)
	case []any:
		output.WriteByte('[')
		for index, item := range current {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := appendCanonical(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			output.Write(encoded)
			output.WriteByte(':')
			if err := appendCanonical(output, current[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", value)
	}
	return nil
}

func canonicalNumber(value string) (string, error) {
	if len(value) > 128 {
		return "", fmt.Errorf("JSON number is too long")
	}
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}
	exponent := 0
	if position := strings.IndexAny(value, "eE"); position >= 0 {
		parsed, err := strconv.Atoi(value[position+1:])
		if err != nil || parsed < -10000 || parsed > 10000 {
			return "", fmt.Errorf("JSON number exponent is outside the supported range")
		}
		exponent = parsed
		value = value[:position]
	}
	fractionDigits := 0
	if position := strings.IndexByte(value, '.'); position >= 0 {
		fractionDigits = len(value) - position - 1
		value = value[:position] + value[position+1:]
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0", nil
	}
	scale := fractionDigits - exponent
	for scale > 0 && strings.HasSuffix(value, "0") {
		value = strings.TrimSuffix(value, "0")
		scale--
	}
	var normalized string
	switch {
	case scale <= 0:
		normalized = value + strings.Repeat("0", -scale)
	case scale >= len(value):
		normalized = "0." + strings.Repeat("0", scale-len(value)) + value
	default:
		normalized = value[:len(value)-scale] + "." + value[len(value)-scale:]
	}
	if negative {
		normalized = "-" + normalized
	}
	return normalized, nil
}

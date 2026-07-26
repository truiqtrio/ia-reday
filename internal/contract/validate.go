package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// ValidateChange performs the transaction engine's three content checks:
// parser round-trip, structured blacklist scanning, and secret-location
// validation. Error text never includes plaintext.
func ValidateChange(kind ParserKind, data []byte, blacklist []string, plaintext string, allowedSecretPaths [][]string) error {
	switch kind {
	case ParserJSON:
		value, err := decodeJSONDocument(data)
		if err != nil {
			return fmt.Errorf("contract: JSON parse: %w", err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("contract: JSON marshal: %w", err)
		}
		roundTripped, err := decodeJSONDocument(encoded)
		if err != nil || !reflect.DeepEqual(value, roundTripped) {
			return errors.New("contract: JSON semantic round-trip mismatch")
		}
		if field, ok := findBlacklistedJSONField(value, stringSet(blacklist)); ok {
			return fmt.Errorf("contract: blacklisted JSON field %q", field)
		}
		return validateJSONSecret(value, plaintext, allowedSecretPaths)
	case ParserTOML:
		values, err := parseManagedCodexTOML(data)
		if err != nil {
			return fmt.Errorf("contract: managed TOML parse: %w", err)
		}
		blacklisted := stringSet(blacklist)
		for path := range values {
			parts := strings.Split(path, ".")
			if _, ok := blacklisted[parts[len(parts)-1]]; ok {
				return fmt.Errorf("contract: blacklisted TOML field %q", parts[len(parts)-1])
			}
		}
		return validateTOMLSecret(values, plaintext, allowedSecretPaths)
	default:
		return fmt.Errorf("contract: unsupported parser kind %q", kind)
	}
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func decodeJSONDocument(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("trailing JSON value")
		}
		return nil, err
	}
	return value, nil
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil
	}
	switch delim {
	case '{':
		out := make(map[string]any)
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("JSON object key is not a string")
			}
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("duplicate JSON object key %q", key)
			}
			value, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		if end, err := dec.Token(); err != nil || end != json.Delim('}') {
			return nil, errors.New("unterminated JSON object")
		}
		return out, nil
	case '[':
		var out []any
		for dec.More() {
			value, err := decodeJSONValue(dec)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		if end, err := dec.Token(); err != nil || end != json.Delim(']') {
			return nil, errors.New("unterminated JSON array")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func findBlacklistedJSONField(value any, blacklist map[string]struct{}) (string, bool) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if _, ok := blacklist[key]; ok {
				return key, true
			}
			if field, ok := findBlacklistedJSONField(child, blacklist); ok {
				return field, true
			}
		}
	case []any:
		for _, child := range value {
			if field, ok := findBlacklistedJSONField(child, blacklist); ok {
				return field, true
			}
		}
	}
	return "", false
}

func validateJSONSecret(value any, plaintext string, allowed [][]string) error {
	if plaintext == "" {
		if len(allowed) != 0 {
			return errors.New("contract: secret policy has no plaintext")
		}
		return nil
	}
	count := countSecretInJSON(value, plaintext)
	if count != len(allowed) {
		return fmt.Errorf("contract: secret appears %d times, want %d permitted locations", count, len(allowed))
	}
	for _, path := range allowed {
		got, ok := lookupJSONPath(value, path)
		if !ok || got != plaintext {
			return fmt.Errorf("contract: permitted secret path %q does not contain the selected key", strings.Join(path, "."))
		}
	}
	return nil
}

func countSecretInJSON(value any, plaintext string) int {
	count := 0
	switch value := value.(type) {
	case string:
		return strings.Count(value, plaintext)
	case map[string]any:
		for key, child := range value {
			count += strings.Count(key, plaintext)
			count += countSecretInJSON(child, plaintext)
		}
	case []any:
		for _, child := range value {
			count += countSecretInJSON(child, plaintext)
		}
	}
	return count
}

func lookupJSONPath(value any, path []string) (string, bool) {
	current := value
	for _, part := range path {
		switch node := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = node[part]
			if !ok {
				return "", false
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				return "", false
			}
			current = node[index]
		default:
			return "", false
		}
	}
	result, ok := current.(string)
	return result, ok
}

func validateTOMLSecret(values map[string]string, plaintext string, allowed [][]string) error {
	if plaintext == "" {
		if len(allowed) != 0 {
			return errors.New("contract: secret policy has no plaintext")
		}
		return nil
	}
	count := 0
	for path, value := range values {
		count += strings.Count(path, plaintext)
		count += strings.Count(value, plaintext)
	}
	if count != len(allowed) {
		return fmt.Errorf("contract: secret appears %d times, want %d permitted locations", count, len(allowed))
	}
	for _, path := range allowed {
		joined := strings.Join(path, ".")
		if values[joined] != plaintext {
			return fmt.Errorf("contract: permitted secret path %q does not contain the selected key", joined)
		}
	}
	return nil
}

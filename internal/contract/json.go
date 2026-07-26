package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type orderedJSONMember struct {
	key string
	raw json.RawMessage
}

// OrderedJSONObject preserves object member order and raw values while a
// caller replaces an explicit allowlist of members.
type OrderedJSONObject struct {
	members []orderedJSONMember
}

// ParseOrderedJSONObject parses exactly one JSON object and rejects duplicate
// member names so merge semantics never depend on parser-specific behavior.
func ParseOrderedJSONObject(data []byte) (OrderedJSONObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return OrderedJSONObject{}, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return OrderedJSONObject{}, errors.New("expected JSON object")
	}
	var out OrderedJSONObject
	seen := make(map[string]struct{})
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return OrderedJSONObject{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return OrderedJSONObject{}, errors.New("expected object key")
		}
		if _, ok := seen[key]; ok {
			return OrderedJSONObject{}, fmt.Errorf("duplicate object key %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return OrderedJSONObject{}, err
		}
		out.members = append(out.members, orderedJSONMember{key: key, raw: raw})
	}
	if _, err := dec.Token(); err != nil {
		return OrderedJSONObject{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return OrderedJSONObject{}, errors.New("trailing JSON value")
		}
		return OrderedJSONObject{}, err
	}
	return out, nil
}

func (o OrderedJSONObject) Get(key string) ([]byte, bool) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].key == key {
			return append([]byte(nil), o.members[i].raw...), true
		}
	}
	return nil, false
}

func (o *OrderedJSONObject) Set(key string, raw []byte) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].key == key {
			o.members[i].raw = append(o.members[i].raw[:0], raw...)
			return
		}
	}
	o.members = append(o.members, orderedJSONMember{key: key, raw: append([]byte(nil), raw...)})
}

func (o OrderedJSONObject) Marshal() []byte {
	var b strings.Builder
	b.WriteString("{\n")
	for i, member := range o.members {
		if i > 0 {
			b.WriteString(",\n")
		}
		key, _ := json.Marshal(member.key)
		b.WriteString("  ")
		b.Write(key)
		b.WriteString(": ")
		b.Write(member.raw)
	}
	b.WriteString("\n}\n")
	return []byte(b.String())
}

// CountJSONStringsContaining counts occurrences in every JSON string token,
// including object member names. Escaped strings are decoded before scanning.
func CountJSONStringsContaining(data []byte, needle string) (int, error) {
	if needle == "" {
		return 0, errors.New("empty JSON canary")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	count := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
		if value, ok := tok.(string); ok {
			count += strings.Count(value, needle)
		}
	}
}

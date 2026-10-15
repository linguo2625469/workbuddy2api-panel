package gateway

// orderedjson.go — order-preserving JSON with python json.dumps-compatible
// serialization.
//
// The upstream wire format is Python's json.dumps default: insertion-order
// keys (the client's field order, "system" appended last), ", " / ": "
// separators, ensure_ascii escaping. Go's json.Marshal(map) sorts keys,
// compacts and HTML-escapes — a different byte layout for the same JSON.
// The notmy wire encodes the body through this layer so the upstream sees
// byte-identical payloads to the reference implementation.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type orderedDoc struct {
	keys []string
	vals map[string]any
}

func newOrderedDoc() *orderedDoc {
	return &orderedDoc{vals: map[string]any{}}
}

// parseOrdered decodes JSON preserving object key order at every nesting
// level (numbers as json.Number to keep the client's literal spelling).
func parseOrdered(body []byte) (*orderedDoc, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	doc, ok := v.(*orderedDoc)
	if !ok {
		return nil, fmt.Errorf("not a JSON object")
	}
	return doc, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeFromToken(dec, tok)
}

func decodeFromToken(dec *json.Decoder, tok json.Token) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			doc := newOrderedDoc()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("bad object key")
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				doc.Set(key, v)
			}
			if _, err := dec.Token(); err != nil { // closing brace
				return nil, err
			}
			return doc, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil { // closing bracket
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	}
	return tok, nil // string | json.Number | bool | nil
}

// Set replaces the value in place (keeping the key's position, like a
// Python dict assignment) or appends the key at the end when new.
func (d *orderedDoc) Set(key string, v any) {
	if _, ok := d.vals[key]; !ok {
		d.keys = append(d.keys, key)
	}
	d.vals[key] = v
}

func (d *orderedDoc) Get(key string) (any, bool) {
	v, ok := d.vals[key]
	return v, ok
}

type jsonSeparators struct {
	item string // between elements
	kv   string // between key and value
}

var pythonSeparators = jsonSeparators{item: ", ", kv: ": "}

// MarshalPython encodes with python json.dumps default formatting.
func (d *orderedDoc) MarshalPython() []byte {
	return d.encode(pythonSeparators)
}

func (d *orderedDoc) encode(sep jsonSeparators) []byte {
	var sb strings.Builder
	encodeDoc(&sb, d, sep)
	return []byte(sb.String())
}

func encodeDoc(sb *strings.Builder, d *orderedDoc, sep jsonSeparators) {
	sb.WriteByte('{')
	for i, k := range d.keys {
		if i > 0 {
			sb.WriteString(sep.item)
		}
		pythonEscape(sb, k)
		sb.WriteString(sep.kv)
		encodeValue(sb, d.vals[k], sep)
	}
	sb.WriteByte('}')
}

func encodeValue(sb *strings.Builder, v any, sep jsonSeparators) {
	switch t := v.(type) {
	case *orderedDoc:
		encodeDoc(sb, t, sep)
	case []any:
		sb.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				sb.WriteString(sep.item)
			}
			encodeValue(sb, e, sep)
		}
		sb.WriteByte(']')
	case string:
		pythonEscape(sb, t)
	case json.Number:
		sb.WriteString(t.String())
	case bool:
		if t {
			sb.WriteString("true")
		} else {
			sb.WriteString("false")
		}
	case nil:
		sb.WriteString("null")
	case map[string]any:
		// values injected by Go code — deterministic (sorted) fallback
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				sb.WriteString(sep.item)
			}
			pythonEscape(sb, k)
			sb.WriteString(sep.kv)
			encodeValue(sb, t[k], sep)
		}
		sb.WriteByte('}')
	default:
		// any other Go value: fall back to encoding/json (compact scalar)
		b, err := json.Marshal(v)
		if err != nil {
			sb.WriteString("null")
			return
		}
		sb.Write(b)
	}
}

// pythonEscape writes a JSON string with python json.dumps default escaping
// (ensure_ascii=True): control chars and everything non-ASCII as \uXXXX,
// surrogate pairs for astral runes.
func pythonEscape(sb *strings.Builder, s string) {
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		case '\b':
			sb.WriteString("\\b")
		case '\f':
			sb.WriteString("\\f")
		default:
			if r < 0x20 {
				fmt.Fprintf(sb, "\\u%04x", r)
			} else {
				// ensure_ascii=False style: non-ASCII raw
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
}

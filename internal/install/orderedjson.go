package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// object is a JSON object that keeps its key order, so rewriting a user's
// settings file changes only the entries the installer owns.
type object struct {
	keys []string
	vals map[string]any // values: *object, []any, string, json.Number, bool, nil
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *object) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			return
		}
	}
}

// parseObject decodes a JSON document whose top level is an object. Empty
// input is an empty object (a settings file that does not exist yet).
func parseObject(b []byte) (*object, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return newObject(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON document")
	}
	o, ok := v.(*object)
	if !ok {
		return nil, errors.New("top level is not a JSON object")
	}
	return o, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T", kt)
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	}
	return tok, nil
}

// marshal writes v with two-space indentation and no HTML escaping.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := write(&buf, v, ""); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func write(buf *bytes.Buffer, v any, indent string) error {
	inner := indent + "  "
	switch t := v.(type) {
	case *object:
		if len(t.keys) == 0 {
			buf.WriteString("{}")
			return nil
		}
		buf.WriteString("{\n")
		for i, k := range t.keys {
			buf.WriteString(inner)
			if err := scalar(buf, k); err != nil {
				return err
			}
			buf.WriteString(": ")
			if err := write(buf, t.vals[k], inner); err != nil {
				return err
			}
			if i < len(t.keys)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "}")
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteString("[\n")
		for i, e := range t {
			buf.WriteString(inner)
			if err := write(buf, e, inner); err != nil {
				return err
			}
			if i < len(t)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		buf.WriteString(indent + "]")
	default:
		return scalar(buf, t)
	}
	return nil
}

func scalar(buf *bytes.Buffer, v any) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	return nil
}

package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

var (
	ErrInvalid  = errors.New("invalid JSON object")
	ErrTooLarge = errors.New("JSON body exceeds size limit")
)

// Decode requires one bounded object and rejects duplicate keys at every depth.
func Decode(r io.Reader, dst any, limit int64) error {
	if limit < 1 || limit > 1<<20 {
		return ErrTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return ErrInvalid
	}
	if int64(len(b)) > limit {
		return ErrTooLarge
	}
	if !utf8.Valid(b) {
		return ErrInvalid
	}
	b = bytes.Trim(b, " \t\r\n")
	if len(b) == 0 || b[0] != '{' {
		return ErrInvalid
	}
	check := json.NewDecoder(bytes.NewReader(b))
	check.UseNumber()
	if err := value(check, 0); err != nil {
		return ErrInvalid
	}
	if _, err := check.Token(); err != io.EOF {
		return ErrInvalid
	}
	if err := canonicalFields(b, reflect.TypeOf(dst), 0); err != nil {
		return ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return ErrInvalid
	}
	return nil
}

// encoding/json accepts case-insensitive field aliases. Transport DTOs require
// the exact contract spelling so aliases cannot override an earlier field.
func canonicalFields(raw json.RawMessage, typ reflect.Type, depth int) error {
	if typ == nil || depth > 32 {
		return ErrInvalid
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.Trim(raw, " \t\r\n"), []byte("null")) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return ErrInvalid
		}
		fields := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field.Type
		}
		for name, value := range object {
			fieldType, ok := fields[name]
			if !ok {
				return ErrInvalid
			}
			if err := canonicalFields(value, fieldType, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return ErrInvalid
		}
		for _, item := range items {
			if err := canonicalFields(item, typ.Elem(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if typ.Key().Kind() != reflect.String {
			return ErrInvalid
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return ErrInvalid
		}
		for _, value := range object {
			if err := canonicalFields(value, typ.Elem(), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func value(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrInvalid
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := t.(string)
			if !ok || seen[key] {
				return ErrInvalid
			}
			seen[key] = true
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for d.More() {
			if err := value(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

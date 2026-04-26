package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Set updates a single configuration value identified by section and keyword.
// It returns an error if the section or keyword is invalid, or if the value
// cannot be converted to the field's type.
//
// Special case: If keyword is empty and the section is a slice, value is
// expected to be a JSON array of the slice's element type.
func (c *Config) Set(section, keyword, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.SetLocked(section, keyword, value)
}

// SetLocked updates a single configuration value like Set, but assumes the
// caller already holds the write lock (e.g. from within a With callback).
func (c *Config) SetLocked(section, keyword, value string) error {

	// 1. Find the section field
	v := reflect.ValueOf(c).Elem()
	sectionField := findFieldByTag(v, section)
	if !sectionField.IsValid() {
		return fmt.Errorf("config: invalid section %q", section)
	}

	// 2. Handle array-based sections (Servers, Categories) via JSON
	if keyword == "" && sectionField.Kind() == reflect.Slice {
		return setSliceValue(sectionField, value)
	}

	// 3. Handle flat sections (General, Downloads, PostProc)
	if sectionField.Kind() == reflect.Struct {
		field := findFieldByTag(sectionField, keyword)
		if !field.IsValid() {
			return fmt.Errorf("config: invalid keyword %q in section %q", keyword, section)
		}
		return setFieldValue(field, value)
	}

	return fmt.Errorf("config: section %q (kind %v) does not support Set with keyword %q", section, sectionField.Kind(), keyword)
}

func setSliceValue(f reflect.Value, val string) error {
	if !f.CanSet() {
		return fmt.Errorf("field cannot be set")
	}

	// Create a new slice of the correct type
	newSlice := reflect.New(f.Type()).Interface()
	if err := json.Unmarshal([]byte(val), newSlice); err != nil {
		return fmt.Errorf("invalid JSON array: %w", err)
	}

	f.Set(reflect.ValueOf(newSlice).Elem())
	return nil
}

func findFieldByTag(v reflect.Value, tagValue string) reflect.Value {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		// Check json tag first, then fallback to yaml
		tag := field.Tag.Get("json")
		if tag == "" {
			tag = field.Tag.Get("yaml")
		}
		parts := strings.Split(tag, ",")
		if parts[0] == tagValue {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

func setFieldValue(f reflect.Value, val string) error {
	if !f.CanSet() {
		return fmt.Errorf("field cannot be set")
	}

	// Handle custom types that wrap int/int64
	ft := f.Type()

	switch {
	case ft == reflect.TypeOf(ByteSize(0)):
		b, err := parseByteSize(val)
		if err != nil {
			return fmt.Errorf("invalid byte size: %w", err)
		}
		f.SetInt(int64(b))
	case ft == reflect.TypeOf(Percent(0)):
		i, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid percent: %w", err)
		}
		p := Percent(i)
		// We don't have a validate method on Percent but we can check range
		if i < 0 || i > 100 {
			return fmt.Errorf("percent: %d outside [0,100]", i)
		}
		f.SetInt(int64(p))
	case ft == reflect.TypeOf(SSLVerify(0)):
		i, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid ssl_verify: %w", err)
		}
		s := SSLVerify(i)
		if err := s.validate(); err != nil {
			return err
		}
		f.SetInt(int64(s))
	case f.Kind() == reflect.String:
		f.SetString(val)
	case f.Kind() == reflect.Int:
		i, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("invalid integer: %w", err)
		}
		f.SetInt(int64(i))
	case f.Kind() == reflect.Int64:
		i, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid int64: %w", err)
		}
		f.SetInt(i)
	case f.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("invalid boolean: %w", err)
		}
		f.SetBool(b)
	default:
		return fmt.Errorf("unsupported field type: %v (%s)", f.Kind(), ft)
	}
	return nil
}

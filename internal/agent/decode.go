package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/aidenpaleczny/navi/internal/domain"
)

// decode is Layer 1 (docs/06-agent-spec.md#validation): encoding/json with
// DisallowUnknownFields, then a reflection walk over the same jsonschema:
// tags schema.go's reflector reads for required/enum/range constraints - one
// tag namespace instead of two that can drift apart. Both failure modes
// return *domain.ValidationError, so a caller checks with errors.As exactly
// once regardless of which one fired.
func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, decodeError(err)
	}
	if err := validateArgs(v); err != nil {
		return v, err
	}
	return v, nil
}

// decodeError turns encoding/json's wording into the shape the escalation
// ladder wants, mirroring internal/schedule's decodeError. Duplicated rather
// than shared: the two packages format field paths differently (a bare
// argument name here, "schedule.x" there) and a shared helper would need a
// prefix parameter for no real savings.
func decodeError(err error) error {
	msg := err.Error()
	const unknown = "json: unknown field "
	if i := strings.Index(msg, unknown); i >= 0 {
		field := strings.Trim(msg[i+len(unknown):], `"`)
		return domain.Invalid("unknown_field", field, "%q is not a recognized argument", field)
	}
	return domain.Invalid("arguments_json", "", "arguments are not valid JSON: %s", msg)
}

// validateArgs walks v's fields, checking jsonschema:"required",
// jsonschema:"enum=..." and jsonschema:"minimum=...,maximum=..." - the same
// tag schema.go's invopop/jsonschema reflector reads. Returns the first
// violation: one precise retry beats an audit, the same policy
// schedule.Validate already uses.
func validateArgs(v any) error {
	return validateValue(reflect.ValueOf(v), "")
}

func validateValue(rv reflect.Value, prefix string) error {
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}

	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" { // unexported
			continue
		}
		fv := rv.Field(i)
		path := jsonFieldName(sf)
		if prefix != "" {
			path = prefix + "." + path
		}

		if tag := sf.Tag.Get("jsonschema"); tag != "" {
			if err := checkField(fv, path, tag); err != nil {
				return err
			}
		}

		// Recurse into nested structs (ItemChanges under UpdateItemArgs) so
		// their own enum/range tags are enforced too. schedule.Schedule
		// carries no jsonschema tags of its own - Layer 2 validates it - so
		// recursing into it is a harmless no-op.
		switch fv.Kind() {
		case reflect.Struct:
			if err := validateValue(fv, path); err != nil {
				return err
			}
		case reflect.Ptr:
			if !fv.IsNil() && fv.Elem().Kind() == reflect.Struct {
				if err := validateValue(fv, path); err != nil {
					return err
				}
			}
		case reflect.Slice:
			// Elements too, indexed in the path so a rejection names the row -
			// "resolutions[2].status" and not "status". bulk_resolve is the
			// first tool taking a list, and without this its per-element enum
			// tag would be decoration: Layer 1 would pass anything and the
			// state machine would reject it later with a worse message.
			for j := 0; j < fv.Len(); j++ {
				if err := validateValue(fv.Index(j), fmt.Sprintf("%s[%d]", path, j)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func jsonFieldName(sf reflect.StructField) string {
	name := strings.Split(sf.Tag.Get("json"), ",")[0]
	if name == "" || name == "-" {
		return sf.Name
	}
	return name
}

func checkField(fv reflect.Value, path, tag string) error {
	var required bool
	var enums []string
	var min, max *float64
	var minItems *int

	for _, part := range strings.Split(tag, ",") {
		switch {
		case part == "required":
			required = true
		case strings.HasPrefix(part, "enum="):
			enums = append(enums, strings.TrimPrefix(part, "enum="))
		case strings.HasPrefix(part, "minimum="):
			if f, err := strconv.ParseFloat(strings.TrimPrefix(part, "minimum="), 64); err == nil {
				min = &f
			}
		case strings.HasPrefix(part, "maximum="):
			if f, err := strconv.ParseFloat(strings.TrimPrefix(part, "maximum="), 64); err == nil {
				max = &f
			}
		case strings.HasPrefix(part, "minItems="):
			if n, err := strconv.Atoi(strings.TrimPrefix(part, "minItems=")); err == nil {
				minItems = &n
			}
		}
	}

	// Dereference for the value checks below. A nil pointer or an empty
	// string is "omitted" - only required objects to that; every other check
	// is skipped rather than applied to a value that was never provided.
	//
	// A nil slice is omitted; an explicit [] is present but empty, which is a
	// different mistake and gets minItems' message rather than "is required".
	present := true
	check := fv
	switch {
	case fv.Kind() == reflect.Ptr:
		if fv.IsNil() {
			present = false
		} else {
			check = fv.Elem()
		}
	case fv.Kind() == reflect.String:
		present = fv.String() != ""
	case fv.Kind() == reflect.Slice:
		present = !fv.IsNil()
	}

	if required && !present {
		return domain.Invalid("field_required", path, "%s is required", path)
	}
	if !present {
		return nil
	}

	if minItems != nil && check.Kind() == reflect.Slice && check.Len() < *minItems {
		return domain.Invalid("min_items", path, "%s needs at least %d item(s), got %d",
			path, *minItems, check.Len())
	}

	if len(enums) > 0 && check.Kind() == reflect.String {
		val := check.String()
		ok := false
		for _, e := range enums {
			if e == val {
				ok = true
				break
			}
		}
		if !ok {
			return domain.Invalid("enum", path, "%s %q is not one of %s", path, val, strings.Join(enums, ", "))
		}
	}

	if min != nil || max != nil {
		switch check.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n := float64(check.Int())
			if min != nil && n < *min {
				return domain.Invalid("range", path, "%s %v is below the minimum %v", path, n, *min)
			}
			if max != nil && n > *max {
				return domain.Invalid("range", path, "%s %v is above the maximum %v", path, n, *max)
			}
		}
	}

	return nil
}

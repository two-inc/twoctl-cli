package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// validatePayload checks a private schema copy: embedded schemas are shared by
// all commands and must retain their original describe output.
func validatePayload(w io.Writer, op *openapi3.Operation, raw []byte) error {
	if len(raw) == 0 {
		if op.RequestBody != nil && op.RequestBody.Value != nil && op.RequestBody.Value.Required {
			return payloadError("request body is required")
		}
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return payloadError("request body must contain one valid JSON value")
	}
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}
	mt := op.RequestBody.Value.Content.Get("application/json")
	if mt == nil || mt.Schema == nil || mt.Schema.Value == nil {
		return nil
	}
	schema := permissiveSchema(mt.Schema.Value, map[*openapi3.Schema]*openapi3.Schema{})
	if err := validatePermissiveSchema(mt.Schema.Value, schema, value); err != nil {
		// Do not render SchemaError.Error(): it includes the submitted payload.
		var schemaErr *openapi3.SchemaError
		if errors.As(err, &schemaErr) {
			return payloadError(fmt.Sprintf("request body violates %s at %q; use --describe to inspect the schema",
				schemaErr.SchemaField, "/"+strings.Join(schemaErr.JSONPointer(), "/")))
		}
		return payloadError("request body does not match the operation schema; use --describe")
	}
	warnings := map[string]bool{}
	unknownPayloadFields(schema, value, "", warnings)
	paths := make([]string, 0, len(warnings))
	for path := range warnings {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		fmt.Fprintf(w, "warning: unknown request property at %q; sending unchanged (the API schema may have drifted)\n", path)
	}
	return nil
}

func payloadError(message string) error {
	return &apiError{code: "invalid_request_body", message: message, exit: ExitUsage}
}

func permissiveSchema(s *openapi3.Schema, seen map[*openapi3.Schema]*openapi3.Schema) *openapi3.Schema {
	if s == nil {
		return nil
	}
	if copy := seen[s]; copy != nil {
		return copy
	}
	copy := *s
	seen[s] = &copy
	clone := func(ref *openapi3.SchemaRef) *openapi3.SchemaRef {
		if ref == nil {
			return nil
		}
		r := *ref
		r.Value = permissiveSchema(ref.Value, seen)
		return &r
	}
	copy.Properties = openapi3.Schemas{}
	for key, ref := range s.Properties {
		copy.Properties[key] = clone(ref)
	}
	copy.Items = clone(s.Items)
	copy.AdditionalProperties.Schema = clone(s.AdditionalProperties.Schema)
	if s.AdditionalProperties.Has != nil && !*s.AdditionalProperties.Has {
		// nil allows additional fields, while preserving explicitly open maps.
		copy.AdditionalProperties.Has = nil
	}
	copy.AllOf = cloneSchemaRefs(s.AllOf, clone)
	copy.AnyOf = cloneSchemaRefs(s.AnyOf, clone)
	copy.OneOf = cloneSchemaRefs(s.OneOf, clone)
	// Negation is a constraint, not an object receiving extra properties.
	// Retain it unchanged so relaxing a forbidden shape cannot reject more input.
	return &copy
}

func cloneSchemaRefs(refs openapi3.SchemaRefs, clone func(*openapi3.SchemaRef) *openapi3.SchemaRef) openapi3.SchemaRefs {
	out := make(openapi3.SchemaRefs, len(refs))
	for i, ref := range refs {
		out[i] = clone(ref)
	}
	return out
}

// payloadSchemas combines applicable branches before checking property names.
// A property declared by one allOf branch is not unknown to its siblings.
func payloadSchemas(s *openapi3.Schema, value any, seen map[*openapi3.Schema]bool) []*openapi3.Schema {
	if s == nil || seen[s] {
		return nil
	}
	seen[s] = true
	out := []*openapi3.Schema{s}
	refs := append(append(append(openapi3.SchemaRefs{}, s.AllOf...), s.AnyOf...), s.OneOf...)
	for _, ref := range refs {
		if ref != nil && ref.Value != nil && ref.Value.VisitJSON(value, openapi3.VisitAsRequest()) == nil {
			out = append(out, payloadSchemas(ref.Value, value, seen)...)
		}
	}
	return out
}

func unknownPayloadFields(s *openapi3.Schema, value any, path string, warnings map[string]bool) {
	schemas := payloadSchemas(s, value, map[*openapi3.Schema]bool{})
	switch v := value.(type) {
	case map[string]any:
		unknownObjectFields(schemas, v, path, warnings)
	case []any:
		for _, schema := range schemas {
			if schema.Items == nil || schema.Items.Value == nil {
				continue
			}
			for i, item := range v {
				unknownPayloadFields(schema.Items.Value, item, fmt.Sprintf("%s/%d", path, i), warnings)
			}
		}
	}
}

func unknownObjectFields(schemas []*openapi3.Schema, value map[string]any, path string, warnings map[string]bool) {
	for key, item := range value {
		known, declared, open := false, false, false
		childPath := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
		for _, schema := range schemas {
			declared = declared || len(schema.Properties) > 0
			if ref := schema.Properties[key]; ref != nil && ref.Value != nil {
				known = true
				unknownPayloadFields(ref.Value, item, childPath, warnings)
			}
			if ref := schema.AdditionalProperties.Schema; ref != nil && ref.Value != nil {
				open = true
				if schema.Properties[key] == nil {
					unknownPayloadFields(ref.Value, item, childPath, warnings)
				}
			}
			open = open || (schema.AdditionalProperties.Has != nil && *schema.AdditionalProperties.Has)
		}
		if declared && !known && !open {
			warnings[childPath] = true
		}
	}
}

func readValidatedRequestBody(w io.Writer, op *openapi3.Operation, b *opBindings, headers http.Header) (io.Reader, error) {
	if b.bodyFile == nil || b.bodyData == nil {
		return nil, nil
	}
	raw, err := readBody(*b.bodyFile, *b.bodyData)
	if err != nil {
		return nil, err
	}
	if err := validatePayload(w, op, raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	headers.Set("Content-Type", "application/json")
	return bytes.NewReader(raw), nil
}

func validatePermissiveSchema(original, relaxed *openapi3.Schema, value any) error {
	// Relaxing closed oneOf branches can make them overlap. Keep an original
	// match, or retry without fields unknown to every branch, before treating
	// that newly introduced ambiguity as a genuine schema violation.
	err := relaxed.VisitJSON(value, openapi3.VisitAsRequest())
	if !errors.Is(err, openapi3.ErrOneOfConflict) {
		return err
	}
	if original.VisitJSON(value, openapi3.VisitAsRequest()) == nil {
		return nil
	}
	return original.VisitJSON(pruneUnknownPayload(original, value), openapi3.VisitAsRequest())
}

func allPayloadSchemas(s *openapi3.Schema, seen map[*openapi3.Schema]bool) []*openapi3.Schema {
	if s == nil || seen[s] {
		return nil
	}
	seen[s] = true
	out := []*openapi3.Schema{s}
	refs := append(append(append(openapi3.SchemaRefs{}, s.AllOf...), s.AnyOf...), s.OneOf...)
	for _, ref := range refs {
		if ref != nil {
			out = append(out, allPayloadSchemas(ref.Value, seen)...)
		}
	}
	return out
}

func pruneUnknownPayload(s *openapi3.Schema, value any) any {
	schemas := allPayloadSchemas(s, map[*openapi3.Schema]bool{})
	switch v := value.(type) {
	case map[string]any:
		return pruneUnknownObject(schemas, v)
	case []any:
		out := append([]any(nil), v...)
		for _, schema := range schemas {
			if schema.Items != nil && schema.Items.Value != nil {
				for i, item := range out {
					out[i] = pruneUnknownPayload(schema.Items.Value, item)
				}
			}
		}
		return out
	default:
		return value
	}
}

func pruneUnknownObject(schemas []*openapi3.Schema, value map[string]any) map[string]any {
	props := openapi3.Schemas{}
	open, declared := false, false
	for _, schema := range schemas {
		open = open || schema.AdditionalProperties.Schema != nil ||
			(schema.AdditionalProperties.Has != nil && *schema.AdditionalProperties.Has)
		declared = declared || len(schema.Properties) > 0 ||
			(schema.AdditionalProperties.Has != nil && !*schema.AdditionalProperties.Has)
		for key, ref := range schema.Properties {
			if props[key] == nil {
				props[key] = &openapi3.SchemaRef{Value: &openapi3.Schema{}}
			}
			props[key].Value.AllOf = append(props[key].Value.AllOf, ref)
		}
	}
	out := map[string]any{}
	for key, item := range value {
		if ref := props[key]; ref != nil && ref.Value != nil {
			out[key] = pruneUnknownPayload(ref.Value, item)
		} else if open || !declared {
			out[key] = item
		}
	}
	return out
}

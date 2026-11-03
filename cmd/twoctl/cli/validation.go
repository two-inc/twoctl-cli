package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// validatePayload checks a private payload projection. Shared embedded schemas
// and the original bytes sent to the API are never modified.
func validatePayload(w io.Writer, op *openapi3.Operation, raw []byte) error {
	if len(raw) == 0 {
		if op.RequestBody != nil && op.RequestBody.Value != nil && op.RequestBody.Value.Required {
			return payloadError("request body is required")
		}
		return nil
	}
	value, err := decodePayload(raw)
	if err != nil {
		return err
	}
	schema := payloadSchema(op)
	if schema == nil {
		return nil
	}
	value, inexact := floatPayload(value, "")
	if inexact != "" {
		// The validator compares numbers as float64, so it could pass or reject
		// this one wrongly; let the API decide.
		fmt.Fprintf(w, "warning: number at %q cannot be checked exactly; skipping local schema validation (the API will validate it)\n", inexact)
		return nil
	}
	if err := schema.VisitJSON(pruneUnknownPayload(schema, value), openapi3.VisitAsRequest()); err != nil {
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

// decodePayload keeps numbers as json.Number so precision loss can be detected.
func decodePayload(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		return nil, payloadError("request body must contain one valid JSON value")
	}
	return value, nil
}

func payloadSchema(op *openapi3.Operation) *openapi3.Schema {
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}
	mt := op.RequestBody.Value.Content.Get("application/json")
	if mt == nil || mt.Schema == nil {
		return nil
	}
	return mt.Schema.Value
}

// floatPayload converts JSON numbers to float64 and returns the path of the
// first number that float64 cannot represent exactly.
func floatPayload(value any, path string) (any, string) {
	switch v := value.(type) {
	case json.Number:
		f, exact := exactFloat(v.String())
		if !exact {
			return value, pointerOrRoot(path)
		}
		return f, ""
	case map[string]any:
		out := make(map[string]any, len(v))
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item, inexact := floatPayload(v[key], path+"/"+strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"))
			if inexact != "" {
				return value, inexact
			}
			out[key] = item
		}
		return out, ""
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			converted, inexact := floatPayload(item, fmt.Sprintf("%s/%d", path, i))
			if inexact != "" {
				return value, inexact
			}
			out[i] = converted
		}
		return out, ""
	default:
		return value, ""
	}
}

// exactFloat reports whether the shortest float64 rendering of literal has the
// same value, so 0.1 is exact but 36.000000000000001 and 1e400 are not.
func exactFloat(literal string) (float64, bool) {
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		return 0, false
	}
	if f == 0 {
		mantissa := strings.TrimLeft(strings.SplitN(strings.ToLower(literal), "e", 2)[0], "-")
		return f, strings.Trim(mantissa, "0.") == ""
	}
	want, ok := new(big.Rat).SetString(literal)
	got, ok2 := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	return f, ok && ok2 && want.Cmp(got) == 0
}

func pointerOrRoot(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func payloadError(message string) error {
	return &apiError{code: "invalid_request_body", message: message, exit: ExitUsage}
}

func unknownPayloadFields(s *openapi3.Schema, value any, path string, warnings map[string]bool) {
	schemas := allPayloadSchemas(s, map[*openapi3.Schema]bool{})
	switch v := value.(type) {
	case map[string]any:
		unknownObjectFields(schemas, v, path, warnings)
	case []any:
		items := payloadItemSchema(schemas)
		for i, item := range v {
			unknownPayloadFields(items, item, fmt.Sprintf("%s/%d", path, i), warnings)
		}
	}
}

func unknownObjectFields(schemas []*openapi3.Schema, value map[string]any, path string, warnings map[string]bool) {
	shape := payloadObjectShape(schemas)
	for key, item := range value {
		childPath := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
		if ref := shape.props[key]; ref != nil {
			unknownPayloadFields(ref.Value, item, childPath, warnings)
		} else if shape.open {
			unknownPayloadFields(shape.extra, item, childPath, warnings)
		} else if shape.declared {
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
		items := payloadItemSchema(schemas)
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = pruneUnknownPayload(items, item)
		}
		return out
	default:
		return value
	}
}

type payloadShape struct {
	props          openapi3.Schemas
	extra          *openapi3.Schema
	open, declared bool
}

func payloadObjectShape(schemas []*openapi3.Schema) payloadShape {
	shape := payloadShape{props: openapi3.Schemas{}, extra: &openapi3.Schema{}}
	for _, schema := range schemas {
		if schema.AdditionalProperties.Schema != nil {
			shape.extra.AllOf = append(shape.extra.AllOf, schema.AdditionalProperties.Schema)
		}
		shape.open = shape.open || schema.AdditionalProperties.Schema != nil ||
			(schema.AdditionalProperties.Has != nil && *schema.AdditionalProperties.Has)
		shape.declared = shape.declared || len(schema.Properties) > 0 ||
			(schema.AdditionalProperties.Has != nil && !*schema.AdditionalProperties.Has)
		for key, ref := range schema.Properties {
			if shape.props[key] == nil {
				shape.props[key] = &openapi3.SchemaRef{Value: &openapi3.Schema{}}
			}
			shape.props[key].Value.AllOf = append(shape.props[key].Value.AllOf, ref)
		}
	}
	return shape
}

func pruneUnknownObject(schemas []*openapi3.Schema, value map[string]any) map[string]any {
	shape := payloadObjectShape(schemas)
	out := map[string]any{}
	for key, item := range value {
		if ref := shape.props[key]; ref != nil {
			out[key] = pruneUnknownPayload(ref.Value, item)
		} else if shape.open || !shape.declared {
			out[key] = pruneUnknownPayload(shape.extra, item)
		}
	}
	return out
}

func payloadItemSchema(schemas []*openapi3.Schema) *openapi3.Schema {
	items := &openapi3.Schema{}
	for _, schema := range schemas {
		if schema.Items != nil {
			items.AllOf = append(items.AllOf, schema.Items)
		}
	}
	return items
}

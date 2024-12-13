// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Traces represents the traces data that can be stored in a persistent storage,
// OR can be embedded by other protocols that transfer OTLP traces data but do
// not implement the OTLP protocol.
//
// The main difference between this message and collector protocol is that
// in this message there will not be any "control" or "metadata" specific to
// OTLP protocol.
//
// When new fields are added into this message, the OTLP request MUST be updated
// as well.
type Traces struct {
	// An array of ResourceSpans.
	// For data coming from a single resource this array will typically contain
	// one element. Intermediary nodes that receive data from multiple origins
	// typically batch the data before forwarding further and in that case this
	// array will contain multiple elements.
	ResourceSpans []*ResourceSpans `json:"resourceSpans,omitempty"`
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into td.
func (td *Traces) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid TracesData type")
	}

	for decoder.More() {
		keyIface, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Empty.
				return nil
			}
			return err
		}

		key, ok := keyIface.(string)
		if !ok {
			return fmt.Errorf("invalid TracesData field: %#v", keyIface)
		}

		switch key {
		case "resourceSpans", "resource_spans":
			err = decoder.Decode(&td.ResourceSpans)
		default:
			// Skip unknown.
		}

		if err != nil {
			return err
		}
	}
	return nil
}

// A collection of ScopeSpans from a Resource.
type ResourceSpans struct {
	// The resource for the spans in this message.
	// If this field is not set then no resource info is known.
	Resource Resource `json:"resource"`
	// A list of ScopeSpans that originate from a resource.
	ScopeSpans []*ScopeSpans `json:"scopeSpans,omitempty"`
	// This schema_url applies to the data in the "resource" field. It does not apply
	// to the data in the "scope_spans" field which have their own schema_url field.
	SchemaURL string `json:"schemaUrl,omitempty"`
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into rs.
func (rs *ResourceSpans) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid ResourceSpans type")
	}

	for decoder.More() {
		keyIface, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Empty.
				return nil
			}
			return err
		}

		key, ok := keyIface.(string)
		if !ok {
			return fmt.Errorf("invalid ResourceSpans field: %#v", keyIface)
		}

		switch key {
		case "resource":
			err = decoder.Decode(&rs.Resource)
		case "scopeSpans", "scope_spans":
			err = decoder.Decode(&rs.ScopeSpans)
		case "schemaUrl", "schema_url":
			err = decoder.Decode(&rs.SchemaURL)
		default:
			// Skip unknown.
		}

		if err != nil {
			return err
		}
	}
	return nil
}

// A collection of Spans produced by an InstrumentationScope.
type ScopeSpans struct {
	// The instrumentation scope information for the spans in this message.
	// Semantically when InstrumentationScope isn't set, it is equivalent with
	// an empty instrumentation scope name (unknown).
	Scope *Scope `json:"scope"`
	// A list of Spans that originate from an instrumentation scope.
	Spans []*Span `json:"spans,omitempty"`
	// The Schema URL, if known. This is the identifier of the Schema that the span data
	// is recorded in. To learn more about Schema URL see
	// https://opentelemetry.io/docs/specs/otel/schemas/#schema-url
	// This schema_url applies to all spans and span events in the "spans" field.
	SchemaURL string `json:"schemaUrl,omitempty"`
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into ss.
func (ss *ScopeSpans) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid ScopeSpans type")
	}

	for decoder.More() {
		keyIface, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Empty.
				return nil
			}
			return err
		}

		key, ok := keyIface.(string)
		if !ok {
			return fmt.Errorf("invalid ScopeSpans field: %#v", keyIface)
		}

		switch key {
		case "scope":
			err = decoder.Decode(&ss.Scope)
		case "spans":
			err = decoder.Decode(&ss.Spans)
		case "schemaUrl", "schema_url":
			err = decoder.Decode(&ss.SchemaURL)
		default:
			// Skip unknown.
		}

		if err != nil {
			return err
		}
	}
	return nil
}

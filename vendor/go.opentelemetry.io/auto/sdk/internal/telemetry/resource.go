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

// Resource information.
type Resource struct {
	// Attrs are the set of attributes that describe the resource. Attribute
	// keys MUST be unique (it is not allowed to have more than one attribute
	// with the same key).
	Attrs []Attr `json:"attributes,omitempty"`
	// DroppedAttrs is the number of dropped attributes. If the value
	// is 0, then no attributes were dropped.
	DroppedAttrs uint32 `json:"droppedAttributesCount,omitempty"`
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into r.
func (r *Resource) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid Resource type")
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
			return fmt.Errorf("invalid Resource field: %#v", keyIface)
		}

		switch key {
		case "attributes":
			err = decoder.Decode(&r.Attrs)
		case "droppedAttributesCount", "dropped_attributes_count":
			err = decoder.Decode(&r.DroppedAttrs)
		default:
			// Skip unknown.
		}

		if err != nil {
			return err
		}
	}
	return nil
}

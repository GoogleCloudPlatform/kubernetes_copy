// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"log/slog"
	"os"
	"strconv"
)

// maxSpan are the span limits resolved during startup.
var maxSpan = newSpanLimits()

type spanLimits struct {
	// Attrs is the number of allowed attributes for a span.
	//
	// This is resolved from the environment variable value for the
	// OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT key if it exists. Otherwise, the
	// environment variable value for OTEL_ATTRIBUTE_COUNT_LIMIT, or 128 if
	// that is not set, is used.
	Attrs int
	// AttrValueLen is the maximum attribute value length allowed for a span.
	//
	// This is resolved from the environment variable value for the
	// OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT key if it exists. Otherwise, the
	// environment variable value for OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT, or -1
	// if that is not set, is used.
	AttrValueLen int
	// Events is the number of allowed events for a span.
	//
	// This is resolved from the environment variable value for the
	// OTEL_SPAN_EVENT_COUNT_LIMIT key, or 128 is used if that is not set.
	Events int
	// EventAttrs is the number of allowed attributes for a span event.
	//
	// The is resolved from the environment variable value for the
	// OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT key, or 128 is used if that is not set.
	EventAttrs int
	// Links is the number of allowed Links for a span.
	//
	// This is resolved from the environment variable value for the
	// OTEL_SPAN_LINK_COUNT_LIMIT, or 128 is used if that is not set.
	Links int
	// LinkAttrs is the number of allowed attributes for a span link.
	//
	// This is resolved from the environment variable value for the
	// OTEL_LINK_ATTRIBUTE_COUNT_LIMIT, or 128 is used if that is not set.
	LinkAttrs int
}

func newSpanLimits() spanLimits {
	return spanLimits{
		Attrs: firstEnv(
			128,
			"OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT",
			"OTEL_ATTRIBUTE_COUNT_LIMIT",
		),
		AttrValueLen: firstEnv(
			-1, // Unlimited.
			"OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT",
			"OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT",
		),
		Events:     firstEnv(128, "OTEL_SPAN_EVENT_COUNT_LIMIT"),
		EventAttrs: firstEnv(128, "OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT"),
		Links:      firstEnv(128, "OTEL_SPAN_LINK_COUNT_LIMIT"),
		LinkAttrs:  firstEnv(128, "OTEL_LINK_ATTRIBUTE_COUNT_LIMIT"),
	}
}

// firstEnv returns the parsed integer value of the first matching environment
// variable from keys. The defaultVal is returned if the value is not an
// integer or no match is found.
func firstEnv(defaultVal int, keys ...string) int {
	for _, key := range keys {
		strV := os.Getenv(key)
		if strV == "" {
			continue
		}

		v, err := strconv.Atoi(strV)
		if err == nil {
			return v
		}
		slog.Warn(
			"invalid limit environment variable",
			"error", err,
			"key", key,
			"value", strV,
		)
	}

	return defaultVal
}

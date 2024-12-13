/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package stats provides internal stats related functionality.
package stats

import "context"

// Labels are the labels for metrics.
type Labels struct {
	// TelemetryLabels are the telemetry labels to record.
	TelemetryLabels map[string]string
}

type labelsKey struct{}

// GetLabels returns the Labels stored in the context, or nil if there is one.
func GetLabels(ctx context.Context) *Labels {
	labels, _ := ctx.Value(labelsKey{}).(*Labels)
	return labels
}

// SetLabels sets the Labels in the context.
func SetLabels(ctx context.Context, labels *Labels) context.Context {
	// could also append
	return context.WithValue(ctx, labelsKey{}, labels)
}

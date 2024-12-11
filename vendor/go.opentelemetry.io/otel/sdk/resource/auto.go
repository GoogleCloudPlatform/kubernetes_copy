// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package resource // import "go.opentelemetry.io/otel/sdk/resource"

import (
	"context"
	"errors"
	"fmt"
)

// ErrPartialResource is returned by a detector when complete source
// information for a Resource is unavailable or the source information
// contains invalid values that are omitted from the returned Resource.
var ErrPartialResource = errors.New("partial resource")

// Detector detects OpenTelemetry resource information.
type Detector interface {
	// DO NOT CHANGE: any modification will not be backwards compatible and
	// must never be done outside of a new major release.

	// Detect returns an initialized Resource based on gathered information.
	// If the source information to construct a Resource contains invalid
	// values, a Resource is returned with the valid parts of the source
	// information used for initialization along with an appropriately
	// wrapped ErrPartialResource error.
	Detect(ctx context.Context) (*Resource, error)
	// DO NOT CHANGE: any modification will not be backwards compatible and
	// must never be done outside of a new major release.
}

// Detect returns a new [Resource] merged from all the Resources each of the
// detectors produces. Each of the detectors are called sequentially, in the
// order they are passed, merging the produced resource into the previous.
//
// This may return a partial Resource along with an error containing
// [ErrPartialResource] if that error is returned from a detector. It may also
// return a merge-conflicting Resource along with an error containing
// [ErrSchemaURLConflict] if merging Resources from different detectors results
// in a schema URL conflict. It is up to the caller to determine if this
// returned Resource should be used or not.
//
// If one of the detectors returns an error that is not [ErrPartialResource],
// the resource produced by the detector will not be merged and the returned
// error will wrap that detector's error.
func Detect(ctx context.Context, detectors ...Detector) (*Resource, error) {
	r := new(Resource)
	return r, detect(ctx, r, detectors)
}

// detect runs all detectors using ctx and merges the result into res. This
// assumes res is allocated and not nil, it will panic otherwise.
//
// If the detectors or merging resources produces any errors (i.e.
// [ErrPartialResource] [ErrSchemaURLConflict]), a single error wrapping all of
// these errors will be returned. Otherwise, nil is returned.
func detect(ctx context.Context, res *Resource, detectors []Detector) error {
	var (
		r   *Resource
		err error
		e   error
	)

	for _, detector := range detectors {
		if detector == nil {
			continue
		}
		r, e = detector.Detect(ctx)
		if e != nil {
			err = errors.Join(err, e)
			if !errors.Is(e, ErrPartialResource) {
				continue
			}
		}
		r, e = Merge(res, r)
		if e != nil {
			err = errors.Join(err, e)
		}
		*res = *r
	}

	if err != nil {
		if errors.Is(err, ErrSchemaURLConflict) {
			// If there has been a merge conflict, ensure the resource has no
			// schema URL.
			res.schemaURL = ""
		}

		err = fmt.Errorf("error detecting resource: %w", err)
	}
	return err
}

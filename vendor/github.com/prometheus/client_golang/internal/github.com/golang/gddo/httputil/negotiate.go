// Copyright 2013 The Go Authors. All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file or at
// https://developers.google.com/open-source/licenses/bsd.

package httputil

import (
	"net/http"

	"github.com/prometheus/client_golang/internal/github.com/golang/gddo/httputil/header"
)

// NegotiateContentEncoding returns the best offered content encoding for the
// request's Accept-Encoding header. If two offers match with equal weight and
// then the offer earlier in the list is preferred. If no offers are
// acceptable, then "" is returned.
func NegotiateContentEncoding(r *http.Request, offers []string) string {
	bestOffer := "identity"
	bestQ := -1.0
	specs := header.ParseAccept(r.Header, "Accept-Encoding")
	for _, offer := range offers {
		for _, spec := range specs {
			if spec.Q > bestQ &&
				(spec.Value == "*" || spec.Value == offer) {
				bestQ = spec.Q
				bestOffer = offer
			}
		}
	}
	if bestQ == 0 {
		bestOffer = ""
	}
	return bestOffer
}

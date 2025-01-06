// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package semconvutil // import "go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful/internal/semconvutil"

// Generate semconvutil package:
//go:generate gotmpl --body=../../../../../../../internal/shared/semconvutil/httpconv_test.go.tmpl "--data={}" --out=httpconv_test.go
//go:generate gotmpl --body=../../../../../../../internal/shared/semconvutil/httpconv.go.tmpl "--data={}" --out=httpconv.go
//go:generate gotmpl --body=../../../../../../../internal/shared/semconvutil/netconv_test.go.tmpl "--data={}" --out=netconv_test.go
//go:generate gotmpl --body=../../../../../../../internal/shared/semconvutil/netconv.go.tmpl "--data={}" --out=netconv.go

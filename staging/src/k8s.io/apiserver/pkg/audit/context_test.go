/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package audit

import (
	"context"
	"fmt"
	"sync"
	"testing"

	auditinternal "k8s.io/apiserver/pkg/apis/audit"

	"github.com/stretchr/testify/assert"
)

func TestEnabled(t *testing.T) {
	tests := []struct {
		name          string
		ctx           *AuditContext
		expectEnabled bool
	}{{
		name:          "nil context",
		expectEnabled: false,
	}, {
		name:          "empty context",
		ctx:           &AuditContext{},
		expectEnabled: true, // An AuditContext should be considered enabled before the level is set
	}, {
		name:          "level None",
		ctx:           &AuditContext{RequestAuditConfig: RequestAuditConfig{Level: auditinternal.LevelNone}},
		expectEnabled: false,
	}, {
		name:          "level Metadata",
		ctx:           &AuditContext{RequestAuditConfig: RequestAuditConfig{Level: auditinternal.LevelMetadata}},
		expectEnabled: true,
	}, {
		name:          "level RequestResponse",
		ctx:           &AuditContext{RequestAuditConfig: RequestAuditConfig{Level: auditinternal.LevelRequestResponse}},
		expectEnabled: true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.expectEnabled, test.ctx.Enabled())
		})
	}
}

func TestAddAuditAnnotation(t *testing.T) {
	const (
		annotationKeyTemplate = "test-annotation-%d"
		annotationValue       = "test-annotation-value"
		annotationExtraValue  = "test-existing-annotation"
		numAnnotations        = 10
	)

	expectAnnotations := func(t *testing.T, annotations map[string]string) {
		assert.Len(t, annotations, numAnnotations)
	}

	ctxWithAnnotation := withAuditContextAndLevel(context.Background(), auditinternal.LevelMetadata)
	AddAuditAnnotation(ctxWithAnnotation, fmt.Sprintf(annotationKeyTemplate, 0), annotationExtraValue)

	tests := []struct {
		description string
		ctx         context.Context
		validator   func(t *testing.T, ctx context.Context)
	}{{
		description: "no audit",
		ctx:         context.Background(),
		validator:   func(_ *testing.T, _ context.Context) {},
	}, {
		description: "context initialized, policy not evaluated",
		// Audit context is initialized, but the policy has not yet been evaluated (no level).
		// Annotations should be retained.
		ctx: WithAuditContext(context.Background()),
		validator: func(t *testing.T, ctx context.Context) {
			ev := AuditContextFrom(ctx).event
			expectAnnotations(t, ev.Annotations)
		},
	}, {
		description: "with metadata level",
		ctx:         withAuditContextAndLevel(context.Background(), auditinternal.LevelMetadata),
		validator: func(t *testing.T, ctx context.Context) {
			ev := AuditContextFrom(ctx).event
			expectAnnotations(t, ev.Annotations)
		},
	}, {
		description: "with none level",
		ctx:         withAuditContextAndLevel(context.Background(), auditinternal.LevelNone),
		validator: func(t *testing.T, ctx context.Context) {
			ev := AuditContextFrom(ctx).event
			assert.Empty(t, ev.Annotations)
		},
	}, {
		description: "with overlapping annotations",
		ctx:         ctxWithAnnotation,
		validator: func(t *testing.T, ctx context.Context) {
			ev := AuditContextFrom(ctx).event
			expectAnnotations(t, ev.Annotations)
			// Verify that the pre-existing annotation is not overwritten.
			assert.Equal(t, annotationExtraValue, ev.Annotations[fmt.Sprintf(annotationKeyTemplate, 0)])
		},
	}}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			var wg sync.WaitGroup
			wg.Add(numAnnotations)
			for i := 0; i < numAnnotations; i++ {
				go func(i int) {
					AddAuditAnnotation(test.ctx, fmt.Sprintf(annotationKeyTemplate, i), annotationValue)
					wg.Done()
				}(i)
			}
			wg.Wait()

			test.validator(t, test.ctx)
		})
	}
}

func TestAuditAnnotationsWithAuditLoggingSetup(t *testing.T) {
	// No audit context data in the request context
	ctx := context.Background()
	AddAuditAnnotation(ctx, "nil", "0")

	// initialize audit context, policy not evaluated yet
	ctx = WithAuditContext(ctx)
	AddAuditAnnotation(ctx, "before-evaluation", "1")

	// policy evaluated, audit logging enabled
	if ac := AuditContextFrom(ctx); ac != nil {
		ac.RequestAuditConfig.Level = auditinternal.LevelMetadata
	}
	AddAuditAnnotation(ctx, "after-evaluation", "2")

	expected := map[string]string{
		"before-evaluation": "1",
		"after-evaluation":  "2",
	}
	actual := AuditContextFrom(ctx).event.Annotations
	assert.Equal(t, expected, actual)
}

func withAuditContextAndLevel(ctx context.Context, l auditinternal.Level) context.Context {
	ctx = WithAuditContext(ctx)
	ac := AuditContextFrom(ctx)
	ac.RequestAuditConfig.Level = l
	return ctx
}

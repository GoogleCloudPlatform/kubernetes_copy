/*
Copyright 2018 The Kubernetes Authors.

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

package options

import (
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/natefinch/lumberjack.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"k8s.io/apiserver/pkg/server"
	v1 "k8s.io/client-go/tools/clientcmd/api/v1"
)

func TestAuditValidOptions(t *testing.T) {
	tmpDir := t.TempDir()
	auditPath := filepath.Join(tmpDir, "audit")

	webhookConfig := makeTmpWebhookConfig(t)
	defer os.Remove(webhookConfig)

	policy := makeTmpPolicy(t)
	defer os.Remove(policy)

	testCases := []struct {
		name     string
		options  func() *AuditOptions
		expected string
	}{{
		name:    "default",
		options: NewAuditOptions,
	}, {
		name: "default log",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = auditPath
			o.PolicyFile = policy
			return o
		},
		expected: "ignoreErrors<log>",
	}, {
		name: "stdout log",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = "-"
			o.PolicyFile = policy
			return o
		},
		expected: "ignoreErrors<log>",
	}, {
		name: "create audit log path dir",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = filepath.Join(tmpDir, "non-existing-dir1", "non-existing-dir2", "audit")
			o.PolicyFile = policy
			return o
		},
		expected: "ignoreErrors<log>",
	}, {
		name: "default log no policy",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = auditPath
			return o
		},
		expected: "",
	}, {
		name: "default webhook",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = webhookConfig
			o.PolicyFile = policy
			return o
		},
		expected: "buffered<webhook>",
	}, {
		name: "default webhook no policy",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = webhookConfig
			return o
		},
		expected: "",
	}, {
		name: "strict webhook",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = webhookConfig
			o.WebhookOptions.BatchOptions.Mode = ModeBlockingStrict
			o.PolicyFile = policy
			return o
		},
		expected: "webhook",
	}, {
		name: "default union",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = auditPath
			o.WebhookOptions.ConfigFile = webhookConfig
			o.PolicyFile = policy
			return o
		},
		expected: "union[ignoreErrors<log>,buffered<webhook>]",
	}, {
		name: "custom",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.BatchOptions.Mode = ModeBatch
			o.LogOptions.Path = auditPath
			o.WebhookOptions.BatchOptions.Mode = ModeBlocking
			o.WebhookOptions.ConfigFile = webhookConfig
			o.PolicyFile = policy
			return o
		},
		expected: "union[buffered<log>,ignoreErrors<webhook>]",
	}, {
		name: "default webhook with truncating",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = webhookConfig
			o.WebhookOptions.TruncateOptions.Enabled = true
			o.PolicyFile = policy
			return o
		},
		expected: "truncate<buffered<webhook>>",
	},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := tc.options()
			require.NotNil(t, options)

			// Verify flags don't change defaults.
			fs := pflag.NewFlagSet("Test", pflag.PanicOnError)
			options.AddFlags(fs)
			require.NoError(t, fs.Parse(nil))
			assert.Equal(t, tc.options(), options, "Flag defaults should match default options.")

			assert.Empty(t, options.Validate(), "Options should be valid.")
			config := &server.Config{}
			require.NoError(t, options.ApplyTo(config))
			if tc.expected == "" {
				assert.Nil(t, config.AuditBackend)
			} else {
				assert.Equal(t, tc.expected, fmt.Sprintf("%s", config.AuditBackend))
			}

			w, err := options.LogOptions.getWriter()
			require.NoError(t, err, "Writer creation should not fail.")

			// Don't check writer if logging is disabled.
			if w == nil {
				return
			}

			if options.LogOptions.Path == "-" {
				assert.Equal(t, os.Stdout, w)
				assert.NoFileExists(t, options.LogOptions.Path)
			} else {
				assert.IsType(t, (*lumberjack.Logger)(nil), w)
				assert.FileExists(t, options.LogOptions.Path)
			}
		})
	}
}

func TestAuditInvalidOptions(t *testing.T) {
	tmpDir := t.TempDir()
	auditPath := filepath.Join(tmpDir, "audit")

	testCases := []struct {
		name    string
		options func() *AuditOptions
	}{{
		name: "invalid log format",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = auditPath
			o.LogOptions.Format = "foo"
			return o
		},
	}, {
		name: "invalid log mode",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = auditPath
			o.LogOptions.BatchOptions.Mode = "foo"
			return o
		},
	}, {
		name: "invalid log buffer size",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.LogOptions.Path = auditPath
			o.LogOptions.BatchOptions.Mode = "batch"
			o.LogOptions.BatchOptions.BatchConfig.BufferSize = -3
			return o
		},
	}, {
		name: "invalid webhook mode",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = auditPath
			o.WebhookOptions.BatchOptions.Mode = "foo"
			return o
		},
	}, {
		name: "invalid webhook buffer throttle qps",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = auditPath
			o.WebhookOptions.BatchOptions.Mode = "batch"
			o.WebhookOptions.BatchOptions.BatchConfig.ThrottleQPS = -1
			return o
		},
	}, {
		name: "invalid webhook truncate max event size",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = auditPath
			o.WebhookOptions.TruncateOptions.Enabled = true
			o.WebhookOptions.TruncateOptions.TruncateConfig.MaxEventSize = -1
			return o
		},
	}, {
		name: "invalid webhook truncate max batch size",
		options: func() *AuditOptions {
			o := NewAuditOptions()
			o.WebhookOptions.ConfigFile = auditPath
			o.WebhookOptions.TruncateOptions.Enabled = true
			o.WebhookOptions.TruncateOptions.TruncateConfig.MaxEventSize = 2
			o.WebhookOptions.TruncateOptions.TruncateConfig.MaxBatchSize = 1
			return o
		},
	},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := tc.options()
			require.NotNil(t, options)
			assert.NotEmpty(t, options.Validate(), "Options should be invalid.")
		})
	}
}

func makeTmpWebhookConfig(t *testing.T) string {
	config := v1.Config{
		Clusters: []v1.NamedCluster{
			{Cluster: v1.Cluster{Server: "localhost", InsecureSkipTLSVerify: true}},
		},
	}
	f, err := os.CreateTemp("", "k8s_audit_webhook_test_")
	require.NoError(t, err, "creating temp file")
	require.NoError(t, stdjson.NewEncoder(f).Encode(config), "writing webhook kubeconfig")
	require.NoError(t, f.Close())
	return f.Name()
}

func makeTmpPolicy(t *testing.T) string {
	pol := auditv1.Policy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "audit.k8s.io/v1",
		},
		Rules: []auditv1.PolicyRule{
			{
				Level: auditv1.LevelRequestResponse,
			},
		},
	}
	f, err := os.CreateTemp("", "k8s_audit_policy_test_")
	require.NoError(t, err, "creating temp file")
	require.NoError(t, stdjson.NewEncoder(f).Encode(pol), "writing policy file")
	require.NoError(t, f.Close())
	return f.Name()
}

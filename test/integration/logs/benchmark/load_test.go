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

package benchmark

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

func TestData(t *testing.T) {
	versionResponse := &runtimev1.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "containerd",
		RuntimeVersion:    "v1.6.18",
		RuntimeApiVersion: "v1",
	}

	testcases := map[string]struct {
		messages []logMessage
		// These are subsets of the full output and may be empty.
		// Prefix and variable stack traces therefore aren't compared.
		printf, structured, json string
		stats                    logStats
	}{
		"data/simple.log": {
			messages: []logMessage{
				{
					msg: "Pod status updated",
				},
			},
			printf:     `Pod status updated: []`,
			structured: `"Pod status updated"`,
			json:       `"msg":"Pod status updated","v":0`,
			stats: logStats{
				TotalLines: 1,
				JsonLines:  1,
				ArgCounts:  map[string]int{},
			},
		},
		"data/split.log": {
			messages: []logMessage{
				{
					msg: "Pod status updated",
				},
				{
					msg: "Pod status updated again",
				},
			},
			stats: logStats{
				TotalLines: 3,
				SplitLines: 1,
				JsonLines:  2,
				ArgCounts:  map[string]int{},
			},
		},
		"data/error.log": {
			messages: []logMessage{
				{
					msg:     "Pod status update",
					err:     errors.New("failed"),
					isError: true,
				},
			},
			printf:     `Pod status update: failed []`,
			structured: `"Pod status update" err="failed"`,
			json:       `"msg":"Pod status update","err":"failed"`,
			stats: logStats{
				TotalLines:    1,
				JsonLines:     1,
				ErrorMessages: 1,
				ArgCounts: map[string]int{
					stringArg: 1,
					totalArg:  1,
				},
			},
		},
		"data/error-value.log": {
			messages: []logMessage{
				{
					msg: "Pod status update",
					kvs: []interface{}{"err", errors.New("failed")},
				},
			},
			printf:     `Pod status update: [err failed]`,
			structured: `"Pod status update" err="failed"`,
			json:       `"msg":"Pod status update","v":0,"err":"failed"`,
			stats: logStats{
				TotalLines: 1,
				JsonLines:  1,
				ArgCounts: map[string]int{
					stringArg: 1,
					totalArg:  1,
				},
			},
		},
		"data/values.log": {
			messages: []logMessage{
				{
					msg: "Example",
					kvs: []interface{}{
						"pod", klog.KRef("system", "kube-scheduler"),
						"pv", klog.KRef("", "volume"),
						"someString", "hello world",
						"someValue", 1.0,
					},
				},
			},
			printf:     `Example: [pod system/kube-scheduler pv volume someString hello world someValue 1]`,
			structured: `"Example" pod="system/kube-scheduler" pv="volume" someString="hello world" someValue=1`,
			json:       `"msg":"Example","v":0,"pod":{"name":"kube-scheduler","namespace":"system"},"pv":{"name":"volume"},"someString":"hello world","someValue":1`,
			stats: logStats{
				TotalLines: 1,
				JsonLines:  1,
				ArgCounts: map[string]int{
					stringArg: 1,
					krefArg:   2,
					numberArg: 1,
					totalArg:  4,
				},
			},
		},
		"data/versionresponse.log": {
			messages: []logMessage{
				{
					msg:       "[RemoteRuntimeService] Version Response",
					verbosity: 10,
					kvs: []interface{}{
						"apiVersion", versionResponse,
					},
				},
			},
			printf:     `[RemoteRuntimeService] Version Response: [apiVersion version:"0.1.0"`,
			structured: `"[RemoteRuntimeService] Version Response" apiVersion="version:\"0.1.0\"`,
			// Because of
			// https://github.com/kubernetes/kubernetes/issues/106652
			// we get the string instead of a JSON struct.
			json: `"msg":"[RemoteRuntimeService] Version Response","v":0,"apiVersion":"version:\"0.1.0\"`,
			stats: logStats{
				TotalLines: 1,
				JsonLines:  1,
				ArgCounts: map[string]int{
					totalArg: 1,
					otherArg: 1,
				},
				OtherArgs: []interface{}{
					versionResponse,
				},
			},
		},
	}

	for filePath, expected := range testcases {
		t.Run(filePath, func(t *testing.T) {
			messages, stats, err := loadLog(filePath)
			if err != nil {
				t.Fatalf("unexpected load error: %v", err)
			}
			assert.Equal(t, expected.messages, messages)
			assert.Equal(t, expected.stats, stats)
			printAll := func(format func(item logMessage)) {
				for _, item := range expected.messages {
					format(item)
				}
			}
			testBuffered := func(t *testing.T, expected string, format string, print func(item logMessage)) {
				var buffer bytes.Buffer
				c := logsapi.NewLoggingConfiguration()
				c.Format = format
				o := logsapi.LoggingOptions{
					ErrorStream: &buffer,
					InfoStream:  &buffer,
				}
				klog.SetOutput(&buffer)
				defer func() {
					if err := logsapi.ResetForTest(nil); err != nil {
						t.Errorf("Unexpected error resetting the logging configuration: %v", err)
					}
				}()
				if err := logsapi.ValidateAndApplyWithOptions(c, &o, nil); err != nil {
					t.Fatalf("Unexpected error configuring logging: %v", err)
				}

				printAll(print)
				klog.Flush()

				if !strings.Contains(buffer.String(), expected) {
					t.Errorf("Expected log output to contain:\n%s\nActual log output:\n%s\n", expected, buffer.String())
				}
			}

			t.Run("printf", func(t *testing.T) {
				testBuffered(t, expected.printf, "text", printf)
			})
			t.Run("structured", func(t *testing.T) {
				testBuffered(t, expected.structured, "text", printLogger)
			})
			t.Run("json", func(t *testing.T) {
				testBuffered(t, expected.json, "json", printLogger)
			})
		})
	}
}

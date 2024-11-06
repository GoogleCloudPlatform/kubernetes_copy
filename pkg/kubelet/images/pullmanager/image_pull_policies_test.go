/*
Copyright 2024 The Kubernetes Authors.

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

package pullmanager

import (
	"reflect"
	"testing"
)

func TestNeverVerifyPreloadedPullPolicy(t *testing.T) {
	tests := []struct {
		name              string
		imagePresent      bool
		imageRecordsExist bool
		want              bool
	}{
		{
			name:              "image does not exist and there are no records about it being pulled",
			imagePresent:      false,
			imageRecordsExist: false,
			want:              true,
		},
		{
			name:              "image exists and there are no records about it being pulled",
			imagePresent:      true,
			imageRecordsExist: false,
			want:              false,
		},
		{
			name:              "image exists and there are records about it being pulled",
			imagePresent:      true,
			imageRecordsExist: true,
			want:              true,
		},
		{
			name:              "image does not exist but there are records about it being pulled",
			imagePresent:      false,
			imageRecordsExist: true,
			want:              true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeverVerifyPreloadedPullPolicy("test-image", tt.imagePresent, tt.imageRecordsExist); got != tt.want {
				t.Errorf("NeverVerifyPreloadedPullPolicy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewNeverVerifyAllowListedPullPolicy(t *testing.T) {
	tests := []struct {
		name              string
		imagePresent      bool
		imageRecordsExist bool
		allowlist         []string
		want              bool
		wantErr           bool
	}{
		{
			name:              "image does not exist and there are no records about it being pulled, not in allowlist",
			imagePresent:      false,
			imageRecordsExist: false,
			want:              true,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/image3"},
		},
		{
			name:              "image exists and there are no records about it being pulled, not in allowlist",
			imagePresent:      true,
			imageRecordsExist: false,
			want:              true,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/image3"},
		},
		{
			name:              "image exists and there are records about it being pulled, not in allowlist",
			imagePresent:      true,
			imageRecordsExist: true,
			want:              true,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/image3"},
		},
		{
			name:              "image does not exist but there are records about it being pulled, not in allowlist",
			imagePresent:      false,
			imageRecordsExist: true,
			want:              true,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/image3"},
		},
		{
			name:              "image does not exist and there are no records about it being pulled, appears in allowlist",
			imagePresent:      false,
			imageRecordsExist: false,
			want:              true,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/test-image", "test.io/test/image3"},
		},
		{
			name:              "image exists and there are no records about it being pulled, appears in allowlist",
			imagePresent:      true,
			imageRecordsExist: false,
			want:              false,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/test-image", "test.io/test/image3"},
		},
		{
			name:              "image exists and there are records about it being pulled, appears in allowlist",
			imagePresent:      true,
			imageRecordsExist: true,
			want:              false,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/test-image", "test.io/test/image3"},
		},
		{
			name:              "image does not exist but there are records about it being pulled, appears in allowlist",
			imagePresent:      false,
			imageRecordsExist: true,
			want:              true,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/test-image", "test.io/test/image3"},
		},
		{
			name:      "invalid allowlist pattern - wildcard in the middle",
			wantErr:   true,
			allowlist: []string{"image.repo/pokus*/imagename"},
		},
		{
			name:      "invalid allowlist pattern - trailing non-segment wildcard middle",
			wantErr:   true,
			allowlist: []string{"image.repo/pokus*"},
		},
		{
			name:      "invalid allowlist pattern - wildcard path segment in the middle",
			wantErr:   true,
			allowlist: []string{"image.repo/*/imagename"},
		},
		{
			name:      "invalid allowlist pattern - only wildcard segment",
			wantErr:   true,
			allowlist: []string{"/*"},
		},
		{
			name:      "invalid allowlist pattern - ends with a '/'",
			wantErr:   true,
			allowlist: []string{"image.repo/"},
		},
		{
			name:      "invalid allowlist pattern - empty",
			wantErr:   true,
			allowlist: []string{""},
		},
		{
			name:      "invalid allowlist pattern - asterisk",
			wantErr:   true,
			allowlist: []string{"*"},
		},
		{
			name:      "invalid allowlist pattern - image with a tag",
			wantErr:   true,
			allowlist: []string{"test.io/test/image1:tagged"},
		},
		{
			name:      "invalid allowlist pattern - image with a digest",
			wantErr:   true,
			allowlist: []string{"test.io/test/image1@sha256:38a8906435c4dd5f4258899d46621bfd8eea3ad6ff494ee3c2f17ef0321625bd"},
		},
		{
			name:      "invalid allowlist pattern - trailing whitespace",
			wantErr:   true,
			allowlist: []string{"test.io/test/image1 "},
		},
		{
			name:              "image exists and there are no records about it being pulled, not in allowlist - different repo wildcard",
			imagePresent:      true,
			imageRecordsExist: false,
			want:              true,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "different.repo/test/*"},
		},
		{
			name:              "image exists and there are no records about it being pulled, not in allowlist - matches org wildcard",
			imagePresent:      true,
			imageRecordsExist: false,
			want:              false,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/test/*"},
		},
		{
			name:              "image exists and there are no records about it being pulled, not in allowlist - matches repo wildcard",
			imagePresent:      true,
			imageRecordsExist: false,
			want:              false,
			allowlist:         []string{"test.io/test/image1", "test.io/test/image2", "test.io/*"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policyEnforcer, err := NewNeverVerifyAllowListedPullPolicy(tt.allowlist)
			if tt.wantErr != (err != nil) {
				t.Fatalf("wanted error: %t, got: %v", tt.wantErr, err)
			}

			if err != nil {
				return
			}

			got := policyEnforcer("test.io/test/test-image", tt.imagePresent, tt.imageRecordsExist)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewNeverVerifyAllowListedPullPolicy() = %v, want %v", got, tt.want)
			}
		})
	}
}

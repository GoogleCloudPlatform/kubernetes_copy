/*
Copyright 2025 The Kubernetes Authors.

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

package spdy

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	restclient "k8s.io/client-go/rest"
)

func TestWebSocketRoundTripper_CustomDialerError(t *testing.T) {
	// Validate config without custom dialer set does *not* return an error.
	rt, upgradeRT, err := RoundTripperFor(&restclient.Config{Host: "fakehost"})
	require.NoError(t, err)
	require.NotNil(t, rt, "roundtripper should be non-nil")
	require.NotNil(t, upgradeRT, "upgrade roundtripper should be non-nil")
	// Validate that custom dialer returns error.
	rt, upgradeRT, err = RoundTripperFor(&restclient.Config{
		Host: "fakehost",
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("not used")
		},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "custom dial function not supported for streaming connections")
	require.Nil(t, rt, "invalid rest config should cause roundtripper to be nil, got (%v)", rt)
	require.Nil(t, upgradeRT, "invalid rest config should cause upgrade roundtripper to be nil, got (%v)", upgradeRT)
}

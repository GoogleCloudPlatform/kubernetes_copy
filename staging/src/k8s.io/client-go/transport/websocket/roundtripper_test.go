/*
Copyright 2023 The Kubernetes Authors.

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

package websocket

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/wsstream"
	"k8s.io/apimachinery/pkg/util/remotecommand"
	restclient "k8s.io/client-go/rest"
)

func TestWebSocketRoundTripper_RoundTripperSucceeds(t *testing.T) {
	// Create fake WebSocket server.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w)
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
	}))
	defer websocketServer.Close()

	// Create the wrapped roundtripper and websocket upgrade roundtripper and call "RoundTrip()".
	websocketLocation, err := url.Parse(websocketServer.URL)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), "GET", websocketServer.URL, nil)
	require.NoError(t, err)
	rt, wsRt, err := RoundTripperFor(&restclient.Config{Host: websocketLocation.Host})
	require.NoError(t, err)
	requestedProtocol := remotecommand.StreamProtocolV5Name
	req.Header[wsstream.WebSocketProtocolHeader] = []string{requestedProtocol}
	_, err = rt.RoundTrip(req)
	require.NoError(t, err)
	// WebSocket Connection is stored in websocket RoundTripper.
	// Compare the expected negotiated subprotocol with the actual subprotocol.
	actualProtocol := wsRt.Connection().Subprotocol()
	assert.Equal(t, requestedProtocol, actualProtocol)

}

func TestWebSocketRoundTripper_RoundTripperFails(t *testing.T) {
	testCases := map[string]struct {
		statusCode    int
		body          string
		status        *metav1.Status
		expectedError string
	}{
		"Empty response status still returns basic websocket error": {
			statusCode:    -1,
			body:          "",
			expectedError: "websocket: bad handshake",
		},
		"Empty response body still returns status": {
			statusCode:    http.StatusForbidden,
			body:          "",
			expectedError: "(403 Forbidden)",
		},
		"Error response body returned as string when can not be cast as metav1.Status": {
			statusCode:    http.StatusForbidden,
			body:          "RBAC violated",
			expectedError: "(403 Forbidden): RBAC violated",
		},
		"Error returned as metav1.Status within response body": {
			statusCode: http.StatusBadRequest,
			body:       "",
			status: &metav1.Status{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "meta.k8s.io/v1",
					Kind:       "Status",
				},
				Status: "Failure",
				Reason: "Unable to negotiate sub-protocol",
				Code:   http.StatusBadRequest,
			},
		},
	}
	encoder := statusCodecs.LegacyCodec(metav1.SchemeGroupVersion)
	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			// Create fake WebSocket server.
			websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if testCase.statusCode > 0 {
					w.WriteHeader(testCase.statusCode)
				}
				if testCase.status != nil {
					statusBytes, err := runtime.Encode(encoder, testCase.status)
					require.NoError(t, err)
					_, err = w.Write(statusBytes)
					require.NoError(t, err)
				} else if len(testCase.body) > 0 {
					_, err := w.Write([]byte(testCase.body))
					require.NoError(t, err)
				}
			}))
			defer websocketServer.Close()

			// Create the wrapped roundtripper and websocket upgrade roundtripper and call "RoundTrip()".
			websocketLocation, err := url.Parse(websocketServer.URL)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(context.Background(), "GET", websocketServer.URL, nil)
			require.NoError(t, err)
			rt, _, err := RoundTripperFor(&restclient.Config{Host: websocketLocation.Host})
			require.NoError(t, err)
			_, err = rt.RoundTrip(req)
			require.Error(t, err)
			assert.True(t, httpstream.IsUpgradeFailure(err))
			if testCase.status != nil {
				upgradeErr := &httpstream.UpgradeFailureError{}
				validErr := errors.As(err, &upgradeErr)
				assert.True(t, validErr, "could not cast error as httpstream.UpgradeFailureError")
				statusErr := upgradeErr.Cause
				apiErr := &apierrors.StatusError{}
				validErr = errors.As(statusErr, &apiErr)
				assert.True(t, validErr, "could not cast error as apierrors.StatusError")
				assert.Equal(t, *testCase.status, apiErr.ErrStatus)
			} else {
				assert.Contains(t, err.Error(), testCase.expectedError,
					"expected (%s), got (%s)", testCase.expectedError, err.Error())
			}
		})
	}
}

func TestWebSocketRoundTripper_NegotiateCreatesConnection(t *testing.T) {
	// Create fake WebSocket server.
	websocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conns, err := webSocketServerStreams(req, w)
		if err != nil {
			t.Fatalf("error on webSocketServerStreams: %v", err)
		}
		defer conns.conn.Close()
	}))
	defer websocketServer.Close()

	// Create the websocket roundtripper and call "Negotiate" to create websocket connection.
	websocketLocation, err := url.Parse(websocketServer.URL)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), "GET", websocketServer.URL, nil)
	require.NoError(t, err)
	rt, wsRt, err := RoundTripperFor(&restclient.Config{Host: websocketLocation.Host})
	require.NoError(t, err)
	requestedProtocol := remotecommand.StreamProtocolV5Name
	conn, err := Negotiate(rt, wsRt, req, requestedProtocol)
	require.NoError(t, err)
	// Compare the expected negotiated subprotocol with the actual subprotocol.
	actualProtocol := conn.Subprotocol()
	assert.Equal(t, requestedProtocol, actualProtocol)
}

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

// websocketStreams contains the WebSocket connection and streams from a server.
type websocketStreams struct {
	conn io.Closer
}

func webSocketServerStreams(req *http.Request, w http.ResponseWriter) (*websocketStreams, error) {
	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		remotecommand.StreamProtocolV5Name: {
			Binary:   true,
			Channels: []wsstream.ChannelType{},
		},
	})
	conn.SetIdleTimeout(4 * time.Hour)
	// Opening the connection responds to WebSocket client, negotiating
	// the WebSocket upgrade connection and the subprotocol.
	_, _, err := conn.Open(w, req)
	if err != nil {
		return nil, err
	}
	return &websocketStreams{conn: conn}, nil
}

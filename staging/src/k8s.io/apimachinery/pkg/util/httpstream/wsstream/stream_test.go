/*
Copyright 2015 The Kubernetes Authors.

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

package wsstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestStream(t *testing.T) {
	input := "some random text"
	//nolint:logcheck // Intentionally uses the old API.
	r := NewReader(bytes.NewBuffer([]byte(input)), true, NewDefaultReaderProtocols())
	r.SetIdleTimeout(time.Second)
	data, err := readWebSocket(r, t, nil)
	if !reflect.DeepEqual(data, []byte(input)) {
		t.Errorf("unexpected server read: %v", data)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestStreamPing(t *testing.T) {
	input := "some random text"
	//nolint:logcheck // Intentionally uses the old API.
	r := NewReader(bytes.NewBuffer([]byte(input)), true, NewDefaultReaderProtocols())
	r.SetIdleTimeout(time.Second)
	err := expectWebSocketFrames(r, t, nil, [][]byte{
		{},
		[]byte(input),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStreamBase64(t *testing.T) {
	input := "some random text"
	encoded := base64.StdEncoding.EncodeToString([]byte(input))
	//nolint:logcheck // Intentionally uses the old API.
	r := NewReader(bytes.NewBuffer([]byte(input)), true, NewDefaultReaderProtocols())
	data, err := readWebSocket(r, t, nil, "base64.binary.k8s.io")
	if !reflect.DeepEqual(data, []byte(encoded)) {
		t.Errorf("unexpected server read: %v\n%v", data, []byte(encoded))
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestStreamVersionedBase64(t *testing.T) {
	input := "some random text"
	encoded := base64.StdEncoding.EncodeToString([]byte(input))
	//nolint:logcheck // Intentionally uses the old API.
	r := NewReader(bytes.NewBuffer([]byte(input)), true, map[string]ReaderProtocolConfig{
		"":                        {Binary: true},
		"binary.k8s.io":           {Binary: true},
		"base64.binary.k8s.io":    {Binary: false},
		"v1.binary.k8s.io":        {Binary: true},
		"v1.base64.binary.k8s.io": {Binary: false},
		"v2.binary.k8s.io":        {Binary: true},
		"v2.base64.binary.k8s.io": {Binary: false},
	})
	data, err := readWebSocket(r, t, nil, "v2.base64.binary.k8s.io")
	if !reflect.DeepEqual(data, []byte(encoded)) {
		t.Errorf("unexpected server read: %v\n%v", data, []byte(encoded))
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestStreamVersionedCopy(t *testing.T) {
	for i, test := range versionTests() {
		func() {
			supportedProtocols := map[string]ReaderProtocolConfig{}
			for p, binary := range test.supported {
				supportedProtocols[p] = ReaderProtocolConfig{
					Binary: binary,
				}
			}
			input := "some random text"
			//nolint:logcheck // Intentionally uses the old API.
			r := NewReader(bytes.NewBuffer([]byte(input)), true, supportedProtocols)
			s, addr := newServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				err := r.Copy(w, req)
				if err != nil {
					w.WriteHeader(503)
				}
			}))
			defer s.Close()

			config, err := websocket.NewConfig("ws://"+addr, "http://localhost/")
			if err != nil {
				t.Error(err)
				return
			}
			config.Protocol = test.requested
			client, err := websocket.DialConfig(config)
			if err != nil {
				if !test.error {
					t.Errorf("test %d: didn't expect error: %v", i, err)
				}
				return
			}
			defer client.Close()
			if test.error && err == nil {
				t.Errorf("test %d: expected an error", i)
				return
			}

			<-r.err
			if got, expected := r.selectedProtocol, test.expected; got != expected {
				t.Errorf("test %d: unexpected protocol version: got=%s expected=%s", i, got, expected)
			}
		}()
	}
}

func TestStreamError(t *testing.T) {
	input := "some random text"
	errs := &errorReader{
		reads: [][]byte{
			[]byte("some random"),
			[]byte(" text"),
		},
		err: fmt.Errorf("bad read"),
	}
	//nolint:logcheck // Intentionally uses the old API.
	r := NewReader(errs, false, NewDefaultReaderProtocols())

	data, err := readWebSocket(r, t, nil)
	if !reflect.DeepEqual(data, []byte(input)) {
		t.Errorf("unexpected server read: %v", data)
	}
	if err == nil || err.Error() != "bad read" {
		t.Fatal(err)
	}
}

func TestStreamSurvivesPanic(t *testing.T) {
	input := "some random text"
	errs := &errorReader{
		reads: [][]byte{
			[]byte("some random"),
			[]byte(" text"),
		},
		panicMessage: "bad read",
	}
	//nolint:logcheck // Intentionally uses the old API.
	r := NewReader(errs, false, NewDefaultReaderProtocols())

	// do not call runtime.HandleCrash() in handler. Otherwise, the tests are interrupted.
	r.handleCrash = func(_ context.Context, additionalHandlers ...func(context.Context, interface{})) { recover() }

	data, err := readWebSocket(r, t, nil)
	if !reflect.DeepEqual(data, []byte(input)) {
		t.Errorf("unexpected server read: %v", data)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestStreamClosedDuringRead(t *testing.T) {
	for i := 0; i < 25; i++ {
		ch := make(chan struct{})
		input := "some random text"
		errs := &errorReader{
			reads: [][]byte{
				[]byte("some random"),
				[]byte(" text"),
			},
			err:   fmt.Errorf("stuff"),
			pause: ch,
		}
		//nolint:logcheck // Intentionally uses the old API.
		r := NewReader(errs, false, NewDefaultReaderProtocols())

		data, err := readWebSocket(r, t, func(c *websocket.Conn) {
			c.Close()
			close(ch)
		})
		// verify that the data returned by the server on an early close always has a specific error
		if err == nil || !strings.Contains(err.Error(), "use of closed network connection") {
			t.Fatal(err)
		}
		// verify that the data returned is a strict subset of the input
		if !bytes.HasPrefix([]byte(input), data) && len(data) != 0 {
			t.Fatalf("unexpected server read: %q", string(data))
		}
	}
}

type errorReader struct {
	reads        [][]byte
	err          error
	panicMessage string
	pause        chan struct{}
}

func (r *errorReader) Read(p []byte) (int, error) {
	if len(r.reads) == 0 {
		if r.pause != nil {
			<-r.pause
		}
		if len(r.panicMessage) != 0 {
			panic(r.panicMessage)
		}
		return 0, r.err
	}
	next := r.reads[0]
	r.reads = r.reads[1:]
	copy(p, next)
	return len(next), nil
}

func readWebSocket(r *Reader, t *testing.T, fn func(*websocket.Conn), protocols ...string) ([]byte, error) {
	errCh := make(chan error, 1)
	s, addr := newServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		errCh <- r.Copy(w, req)
	}))
	defer s.Close()

	config, _ := websocket.NewConfig("ws://"+addr, "http://"+addr)
	config.Protocol = protocols
	client, err := websocket.DialConfig(config)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	if fn != nil {
		fn(client)
	}

	data, err := io.ReadAll(client)
	if err != nil {
		return data, err
	}
	return data, <-errCh
}

func expectWebSocketFrames(r *Reader, t *testing.T, fn func(*websocket.Conn), frames [][]byte, protocols ...string) error {
	errCh := make(chan error, 1)
	s, addr := newServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		errCh <- r.Copy(w, req)
	}))
	defer s.Close()

	config, _ := websocket.NewConfig("ws://"+addr, "http://"+addr)
	config.Protocol = protocols
	ws, err := websocket.DialConfig(config)
	if err != nil {
		return err
	}
	defer ws.Close()

	if fn != nil {
		fn(ws)
	}

	for i := range frames {
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err != nil {
			return err
		}
		if !reflect.DeepEqual(frames[i], data) {
			return fmt.Errorf("frame %d did not match expected: %v", data, err)
		}
	}
	var data []byte
	if err := websocket.Message.Receive(ws, &data); err != io.EOF {
		return fmt.Errorf("expected no more frames: %v (%v)", err, data)
	}
	return <-errCh
}

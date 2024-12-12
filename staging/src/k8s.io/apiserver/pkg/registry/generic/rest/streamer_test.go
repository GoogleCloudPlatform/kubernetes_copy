/*
Copyright 2014 The Kubernetes Authors.

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

package rest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestInputStreamReader(t *testing.T) {
	resultString := "Test output"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(resultString))
	}))
	defer s.Close()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Errorf("Error parsing server URL: %v", err)
		return
	}
	streamer := &LocationStreamer{
		Location: u,
	}
	readCloser, _, _, err := streamer.InputStream(context.Background(), "", "")
	if err != nil {
		t.Errorf("Unexpected error when getting stream: %v", err)
		return
	}
	defer readCloser.Close()
	result, _ := io.ReadAll(readCloser)
	if string(result) != resultString {
		t.Errorf("Stream content does not match. Got: %s. Expected: %s.", string(result), resultString)
	}
}

func TestInputStreamNullLocation(t *testing.T) {
	streamer := &LocationStreamer{
		Location: nil,
	}
	readCloser, _, _, err := streamer.InputStream(context.Background(), "", "")
	if err != nil {
		t.Errorf("Unexpected error when getting stream with null location: %v", err)
	}
	if readCloser != nil {
		t.Errorf("Expected stream to be nil. Got: %#v", readCloser)
	}
}

type testTransport struct {
	body string
}

func (tt *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := bufio.NewReader(bytes.NewBufferString(tt.body))
	return http.ReadResponse(r, req)
}

func fakeTransport(mime, message string) http.RoundTripper {
	content := fmt.Sprintf("HTTP/1.1 200 OK\nContent-Type: %s\n\n%s", mime, message)
	return &testTransport{body: content}
}

func TestInputStreamContentType(t *testing.T) {
	location, _ := url.Parse("http://www.example.com")
	streamer := &LocationStreamer{
		Location:  location,
		Transport: fakeTransport("application/json", "hello world"),
	}
	readCloser, _, contentType, err := streamer.InputStream(context.Background(), "", "")
	if err != nil {
		t.Errorf("Unexpected error when getting stream: %v", err)
		return
	}
	defer readCloser.Close()
	if contentType != "application/json" {
		t.Errorf("Unexpected content type. Got: %s. Expected: application/json", contentType)
	}
}

func TestInputStreamTransport(t *testing.T) {
	message := "hello world"
	location, _ := url.Parse("http://www.example.com")
	streamer := &LocationStreamer{
		Location:  location,
		Transport: fakeTransport("text/plain", message),
	}
	readCloser, _, _, err := streamer.InputStream(context.Background(), "", "")
	if err != nil {
		t.Errorf("Unexpected error when getting stream: %v", err)
		return
	}
	defer readCloser.Close()
	result, _ := io.ReadAll(readCloser)
	if string(result) != message {
		t.Errorf("Stream content does not match. Got: %s. Expected: %s.", string(result), message)
	}
}

func fakeInternalServerErrorTransport(mime, message string) http.RoundTripper {
	content := fmt.Sprintf("HTTP/1.1 500 \"Internal Server Error\"\nContent-Type: %s\n\n%s", mime, message)
	return &testTransport{body: content}
}

func TestInputStreamInternalServerErrorTransport(t *testing.T) {
	message := "Pod is in PodPending"
	location, _ := url.Parse("http://www.example.com")
	streamer := &LocationStreamer{
		Location:        location,
		Transport:       fakeInternalServerErrorTransport("text/plain", message),
		ResponseChecker: NewGenericHttpResponseChecker(schema.GroupResource{}, ""),
	}
	expectedError := errors.NewInternalError(fmt.Errorf("%s", message))

	_, _, _, err := streamer.InputStream(context.Background(), "", "")
	if err == nil {
		t.Errorf("unexpected non-error")
		return
	}

	if !reflect.DeepEqual(err, expectedError) {
		t.Errorf("StreamInternalServerError does not match. Got: %s. Expected: %s.", err, expectedError)
	}
}

func TestInputStreamRedirects(t *testing.T) {
	const redirectPath = "/redirect"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == redirectPath {
			t.Fatal("Redirects should not be followed")
		} else {
			http.Redirect(w, req, redirectPath, http.StatusFound)
		}
	}))
	loc, err := url.Parse(s.URL)
	require.NoError(t, err, "Error parsing server URL")

	streamer := &LocationStreamer{
		Location:        loc,
		RedirectChecker: PreventRedirects,
	}
	_, _, _, err = streamer.InputStream(context.Background(), "", "")
	assert.Error(t, err, "Redirect should trigger an error")
}

// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httptypes

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type HTTPError struct {
	Message string `json:"message"`
	// Code is the HTTP status code
	Code int `json:"-"`
}

func (e HTTPError) Error() string {
	return e.Message
}

func (e HTTPError) WriteTo(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code)
	b, err := json.Marshal(e)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal HTTPError: %v", err))
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return nil
}

func NewHTTPError(code int, m string) *HTTPError {
	return &HTTPError{
		Message: m,
		Code:    code,
	}
}

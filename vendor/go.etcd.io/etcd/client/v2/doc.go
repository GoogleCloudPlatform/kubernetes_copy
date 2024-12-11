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

/*
Package client provides bindings for the etcd APIs.

Create a Config and exchange it for a Client:

	import (
		"net/http"
		"context"

		"go.etcd.io/etcd/client/v2"
	)

	cfg := client.Config{
		Endpoints: []string{"http://127.0.0.1:2379"},
		Transport: DefaultTransport,
	}

	c, err := client.New(cfg)
	if err != nil {
		// handle error
	}

Clients are safe for concurrent use by multiple goroutines.

Create a KeysAPI using the Client, then use it to interact with etcd:

	kAPI := client.NewKeysAPI(c)

	// create a new key /foo with the value "bar"
	_, err = kAPI.Create(context.Background(), "/foo", "bar")
	if err != nil {
		// handle error
	}

	// delete the newly created key only if the value is still "bar"
	_, err = kAPI.Delete(context.Background(), "/foo", &DeleteOptions{PrevValue: "bar"})
	if err != nil {
		// handle error
	}

Use a custom context to set timeouts on your operations:

	import "time"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// set a new key, ignoring its previous state
	_, err := kAPI.Set(ctx, "/ping", "pong", nil)
	if err != nil {
		if err == context.DeadlineExceeded {
			// request took longer than 5s
		} else {
			// handle error
		}
	}

*/
package client

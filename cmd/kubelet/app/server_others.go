//go:build !windows
// +build !windows

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

package app

import (
	"context"
	"fmt"
	"os"
)

func checkPermissions(ctx context.Context) error {
	if uid := os.Getuid(); uid != 0 {
		return fmt.Errorf("kubelet needs to run as uid `0`. It is being run as %d", uid)
	}
	// TODO: Check if kubelet is running in the `initial` user namespace.
	// http://man7.org/linux/man-pages/man7/user_namespaces.7.html
	return nil
}

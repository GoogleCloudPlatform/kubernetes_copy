//go:build windows
// +build windows

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
	"errors"
	"os/user"

	"golang.org/x/sys/windows"
	"k8s.io/klog/v2"
)

func checkPermissions(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	u, err := user.Current()
	if err != nil {
		logger.Error(err, "Unable to get current user")
		return err
	}

	// For Windows user.UserName contains the login name and user.Name contains
	// the user's display name - https://pkg.go.dev/os/user#User
	logger.Info("Kubelet is running as", "login name", u.Username, "dispaly name", u.Name)

	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("kubelet needs to run with elevated permissions!")
	}

	return nil
}

//go:build windows
// +build windows

/*
Copyright 2018 The Kubernetes Authors.

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
	"unsafe"

	"golang.org/x/sys/windows"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/windows/service"
)

const (
	serviceName = "kubelet"
)

// getPriorityValue returns the value associated with a Windows process priorityClass
// Ref: https://docs.microsoft.com/en-us/windows/win32/cimwin32prov/setpriority-method-in-class-win32-process
func getPriorityValue(priorityClassName string) uint32 {
	var priorityClassMap = map[string]uint32{
		"IDLE_PRIORITY_CLASS":         uint32(windows.IDLE_PRIORITY_CLASS),
		"BELOW_NORMAL_PRIORITY_CLASS": uint32(windows.BELOW_NORMAL_PRIORITY_CLASS),
		"NORMAL_PRIORITY_CLASS":       uint32(windows.NORMAL_PRIORITY_CLASS),
		"ABOVE_NORMAL_PRIORITY_CLASS": uint32(windows.ABOVE_NORMAL_PRIORITY_CLASS),
		"HIGH_PRIORITY_CLASS":         uint32(windows.HIGH_PRIORITY_CLASS),
		"REALTIME_PRIORITY_CLASS":     uint32(windows.REALTIME_PRIORITY_CLASS),
	}
	return priorityClassMap[priorityClassName]
}

// createWindowsJobObject creates a new Job Object
// (https://docs.microsoft.com/en-us/windows/win32/procthread/job-objects),
// and specifies the priority class for the job object to the specified value.
// A job object is used here so that any spawned processes such as powershell or
// wmic are created at the specified thread priority class.
// Running kubelet with above normal / high priority  can help improve
// responsiveness on machines with high CPU utilization.
func createWindowsJobObject(pc uint32) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("windows.CreateJobObject failed: %w", err)
	}
	limitInfo := windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
		LimitFlags:    windows.JOB_OBJECT_LIMIT_PRIORITY_CLASS,
		PriorityClass: pc,
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectBasicLimitInformation,
		uintptr(unsafe.Pointer(&limitInfo)),
		uint32(unsafe.Sizeof(limitInfo))); err != nil {
		return windows.InvalidHandle, fmt.Errorf("windows.SetInformationJobObject failed: %w", err)
	}
	return job, nil
}

func initForOS(ctx context.Context, windowsService bool, windowsPriorityClass string) error {
	logger := klog.FromContext(ctx)
	priority := getPriorityValue(windowsPriorityClass)
	if priority == 0 {
		return fmt.Errorf("unknown priority class %s, valid ones are available at "+
			"https://docs.microsoft.com/en-us/windows/win32/procthread/scheduling-priorities", windowsPriorityClass)
	}
	logger.Info("Creating a Windows job object and adding kubelet process to it", "windowsPriorityClass", windowsPriorityClass)
	job, err := createWindowsJobObject(priority)
	if err != nil {
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		return fmt.Errorf("windows.AssignProcessToJobObject failed: %w", err)
	}

	if windowsService {
		return service.InitServiceWithShutdown(serviceName)
	}
	return nil
}

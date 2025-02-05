//go:build windows
// +build windows

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

package services

import (
	"os/exec"
	"sync"

	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	once      sync.Once
	jobHandle windows.Handle
)

// func startProcess starts the process with the given command and arguments
func startProcess(cmd *exec.Cmd, monitorParent bool) error {
	if monitorParent {
		// create a job object, which is a kernel object that manages the lifetime of child processes
		// the job object is created as an singlton instance
		jobHandle, err := getJobHandle()
		if err != nil {
			return err
		}

		go func() {
			defer windows.CloseHandle(jobHandle)

			// create the child process in a suspended state
			// cmd.SysProcAttr = &syscall.SysProcAttr{
			// 	CreationFlags: windows.CREATE_SUSPENDED,
			// }
			waitForTerminationSignal()
		}()

		err = cmd.Start()
		if err != nil {
			return err
		}

		// add the process to the job object
		err = assignProcessToJob(jobHandle, cmd.Process.Pid)
		if err != nil {
			return err
		}

		// for simplicity purpose, skip the Pause/Resume the process
		// resumeErr := resumeProcess(cmd.Process.Pid)
		// if resumeErr != nil {
		// 	framework.Failf("Failed to resume process: %v", resumeErr)
		// }

		return nil

	} else {
		// start the command directly
		err := cmd.Start()
		return err
	}
}

// func getJobHandle() returns the single instance of job handle
func getJobHandle() (windows.Handle, error) {
	var err error

	once.Do(func() {
		jobHandle, err = createJobObject()
	})

	return jobHandle, err
}

// func createJobObject creates a windows job object
func createJobObject() (windows.Handle, error) {
	jobHandle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}

	var info windows.JOBOBJECT_BASIC_LIMIT_INFORMATION
	info.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	var extendedInfo windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	extendedInfo.BasicLimitInformation = info
	_, err = windows.SetInformationJobObject(
		jobHandle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&extendedInfo)),
		uint32(unsafe.Sizeof(extendedInfo)),
	)

	if err != nil {
		windows.CloseHandle(jobHandle)
		return 0, err
	}

	return jobHandle, nil
}

// func assignProcessToJob assigns the process to the job object
func assignProcessToJob(jobHandle windows.Handle, pid int) error {
	// open a handle to the child process using its PID
	processHandle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(processHandle)

	// assign the process to the job object
	err = windows.AssignProcessToJobObject(jobHandle, processHandle)
	if err != nil {
		return err
	}

	return nil
}

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows
// +build windows

package resource // import "go.opentelemetry.io/otel/sdk/resource"

import (
	"golang.org/x/sys/windows/registry"
)

// implements hostIDReader.
type hostIDReaderWindows struct{}

// read reads MachineGuid from the Windows registry key:
// SOFTWARE\Microsoft\Cryptography.
func (*hostIDReaderWindows) read() (string, error) {
	k, err := registry.OpenKey(
		registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return "", err
	}
	defer k.Close()

	guid, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", err
	}

	return guid, nil
}

var platformHostIDReader hostIDReader = &hostIDReaderWindows{}

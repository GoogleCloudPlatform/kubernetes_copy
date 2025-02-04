//go:build linux
// +build linux

/*
Copyright 2020 The Kubernetes Authors.

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

package systemd

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"k8s.io/klog/v2"
)

const (
	logindService   = "org.freedesktop.login1"
	logindObject    = dbus.ObjectPath("/org/freedesktop/login1")
	logindInterface = "org.freedesktop.login1.Manager"
)

type dBusConnector interface {
	Object(dest string, path dbus.ObjectPath) dbus.BusObject
	AddMatchSignal(options ...dbus.MatchOption) error
	Signal(ch chan<- *dbus.Signal)
}

// DBusCon has functions that can be used to interact with systemd and logind over dbus.
type DBusCon struct {
	SystemBus dBusConnector
}

func NewDBusCon() (*DBusCon, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}

	return &DBusCon{
		SystemBus: conn,
	}, nil
}

// InhibitLock is a lock obtained after creating an systemd inhibitor by calling InhibitShutdown().
type InhibitLock uint32

// CurrentInhibitDelay returns the current delay inhibitor timeout value as configured in logind.conf(5).
// see https://www.freedesktop.org/software/systemd/man/logind.conf.html for more details.
func (bus *DBusCon) CurrentInhibitDelay() (time.Duration, error) {
	obj := bus.SystemBus.Object(logindService, logindObject)
	res, err := obj.GetProperty(logindInterface + ".InhibitDelayMaxUSec")
	if err != nil {
		return 0, fmt.Errorf("failed reading InhibitDelayMaxUSec property from logind: %w", err)
	}

	delay, ok := res.Value().(uint64)
	if !ok {
		return 0, fmt.Errorf("InhibitDelayMaxUSec from logind is not a uint64 as expected")
	}

	// InhibitDelayMaxUSec is in microseconds
	duration := time.Duration(delay) * time.Microsecond
	return duration, nil
}

// InhibitShutdown creates an systemd inhibitor by calling logind's Inhibt() and returns the inhibitor lock
// see https://www.freedesktop.org/wiki/Software/systemd/inhibit/ for more details.
func (bus *DBusCon) InhibitShutdown() (InhibitLock, error) {
	obj := bus.SystemBus.Object(logindService, logindObject)
	what := "shutdown"
	who := "kubelet"
	why := "Kubelet needs time to handle node shutdown"
	mode := "delay"

	call := obj.Call("org.freedesktop.login1.Manager.Inhibit", 0, what, who, why, mode)
	if call.Err != nil {
		return InhibitLock(0), fmt.Errorf("failed creating systemd inhibitor: %w", call.Err)
	}

	var fd uint32
	err := call.Store(&fd)
	if err != nil {
		return InhibitLock(0), fmt.Errorf("failed storing inhibit lock file descriptor: %w", err)
	}

	return InhibitLock(fd), nil
}

// ReleaseInhibitLock will release the underlying inhibit lock which will cause the shutdown to start.
func (bus *DBusCon) ReleaseInhibitLock(lock InhibitLock) error {
	err := syscall.Close(int(lock))

	if err != nil {
		return fmt.Errorf("unable to close systemd inhibitor lock: %w", err)
	}

	return nil
}

// ReloadLogindConf uses dbus to send a SIGHUP to the systemd-logind service causing logind to reload it's configuration.
func (bus *DBusCon) ReloadLogindConf() error {
	systemdService := "org.freedesktop.systemd1"
	systemdObject := "/org/freedesktop/systemd1"
	systemdInterface := "org.freedesktop.systemd1.Manager"

	obj := bus.SystemBus.Object(systemdService, dbus.ObjectPath(systemdObject))
	unit := "systemd-logind.service"
	who := "all"
	var signal int32 = 1 // SIGHUP

	call := obj.Call(systemdInterface+".KillUnit", 0, unit, who, signal)
	if call.Err != nil {
		return fmt.Errorf("unable to reload logind conf: %w", call.Err)
	}

	return nil
}

// MonitorShutdown detects the node shutdown by watching for "PrepareForShutdown" logind events.
// see https://www.freedesktop.org/wiki/Software/systemd/inhibit/ for more details.
func (bus *DBusCon) MonitorShutdown(logger klog.Logger) (<-chan bool, error) {
	err := bus.SystemBus.AddMatchSignal(dbus.WithMatchInterface(logindInterface), dbus.WithMatchMember("PrepareForShutdown"), dbus.WithMatchObjectPath("/org/freedesktop/login1"))

	if err != nil {
		return nil, err
	}

	busChan := make(chan *dbus.Signal, 1)
	bus.SystemBus.Signal(busChan)

	shutdownChan := make(chan bool, 1)

	go func() {
		for {
			event, ok := <-busChan
			if !ok {
				close(shutdownChan)
				return
			}
			if event == nil || len(event.Body) == 0 {
				logger.Error(nil, "Failed obtaining shutdown event, PrepareForShutdown event was empty")
				continue
			}
			shutdownActive, ok := event.Body[0].(bool)
			if !ok {
				logger.Error(nil, "Failed obtaining shutdown event, PrepareForShutdown event was not bool type as expected")
				continue
			}
			shutdownChan <- shutdownActive
		}
	}()

	return shutdownChan, nil
}

const (
	logindConfigDirectory = "/etc/systemd/logind.conf.d/"
	kubeletLogindConf     = "99-kubelet.conf"
)

// OverrideInhibitDelay writes a config file to logind overriding InhibitDelayMaxSec to the value desired.
func (bus *DBusCon) OverrideInhibitDelay(inhibitDelayMax time.Duration) error {
	err := os.MkdirAll(logindConfigDirectory, 0755)
	if err != nil {
		return fmt.Errorf("failed creating %v directory: %w", logindConfigDirectory, err)
	}

	// This attempts to set the `InhibitDelayMaxUSec` dbus property of logind which is MaxInhibitDelay measured in microseconds.
	// The corresponding logind config file property is named `InhibitDelayMaxSec` and is measured in seconds which is set via logind.conf config.
	// Refer to https://www.freedesktop.org/software/systemd/man/logind.conf.html for more details.

	inhibitOverride := fmt.Sprintf(`# Kubelet logind override
[Login]
InhibitDelayMaxSec=%.0f
`, inhibitDelayMax.Seconds())

	logindOverridePath := filepath.Join(logindConfigDirectory, kubeletLogindConf)
	if err := os.WriteFile(logindOverridePath, []byte(inhibitOverride), 0644); err != nil {
		return fmt.Errorf("failed writing logind shutdown inhibit override file %v: %w", logindOverridePath, err)
	}

	return nil
}

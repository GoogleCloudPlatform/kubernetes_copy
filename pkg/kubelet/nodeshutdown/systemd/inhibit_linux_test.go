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
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/stretchr/testify/assert"
	"k8s.io/klog/v2/ktesting"
)

type fakeDBusObject struct {
	properties map[string]interface{}
	bodyValue  interface{}
}

func (obj *fakeDBusObject) Call(method string, flags dbus.Flags, args ...interface{}) *dbus.Call {
	return &dbus.Call{Err: nil, Body: []interface{}{obj.bodyValue}}
}

func (obj *fakeDBusObject) CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...interface{}) *dbus.Call {
	return nil
}

func (obj *fakeDBusObject) Go(method string, flags dbus.Flags, ch chan *dbus.Call, args ...interface{}) *dbus.Call {
	return nil
}

func (obj *fakeDBusObject) GoWithContext(ctx context.Context, method string, flags dbus.Flags, ch chan *dbus.Call, args ...interface{}) *dbus.Call {
	return nil
}

func (obj *fakeDBusObject) AddMatchSignal(iface, member string, options ...dbus.MatchOption) *dbus.Call {
	return nil
}

func (obj *fakeDBusObject) RemoveMatchSignal(iface, member string, options ...dbus.MatchOption) *dbus.Call {
	return nil
}

func (obj *fakeDBusObject) GetProperty(p string) (dbus.Variant, error) {
	value, ok := obj.properties[p]

	if !ok {
		return dbus.Variant{}, fmt.Errorf("property %q does not exist in properties: %+v", p, obj.properties)
	}

	return dbus.MakeVariant(value), nil
}

func (obj *fakeDBusObject) SetProperty(p string, v interface{}) error {
	return nil
}

func (obj *fakeDBusObject) StoreProperty(p string, v interface{}) error {
	return nil
}

func (obj *fakeDBusObject) Destination() string {
	return ""
}

func (obj *fakeDBusObject) Path() dbus.ObjectPath {
	return ""
}

type fakeSystemDBus struct {
	fakeDBusObject *fakeDBusObject
	signalChannel  chan<- *dbus.Signal
}

func (f *fakeSystemDBus) Object(dest string, path dbus.ObjectPath) dbus.BusObject {
	return f.fakeDBusObject
}

func (f *fakeSystemDBus) Signal(ch chan<- *dbus.Signal) {
	f.signalChannel = ch
}

func (f *fakeSystemDBus) AddMatchSignal(options ...dbus.MatchOption) error {
	return nil
}

func TestCurrentInhibitDelay(t *testing.T) {
	thirtySeconds := time.Duration(30) * time.Second

	bus := DBusCon{
		SystemBus: &fakeSystemDBus{
			fakeDBusObject: &fakeDBusObject{
				properties: map[string]interface{}{
					"org.freedesktop.login1.Manager.InhibitDelayMaxUSec": uint64(thirtySeconds / time.Microsecond),
				},
			},
		},
	}

	delay, err := bus.CurrentInhibitDelay()
	assert.NoError(t, err)
	assert.Equal(t, thirtySeconds, delay)
}

func TestInhibitShutdown(t *testing.T) {
	var fakeFd uint32 = 42

	bus := DBusCon{
		SystemBus: &fakeSystemDBus{
			fakeDBusObject: &fakeDBusObject{
				bodyValue: fakeFd,
			},
		},
	}

	fdLock, err := bus.InhibitShutdown()
	assert.Equal(t, InhibitLock(fakeFd), fdLock)
	assert.NoError(t, err)
}

func TestReloadLogindConf(t *testing.T) {
	bus := DBusCon{
		SystemBus: &fakeSystemDBus{
			fakeDBusObject: &fakeDBusObject{},
		},
	}
	assert.NoError(t, bus.ReloadLogindConf())
}

func TestMonitorShutdown(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	var tests = []struct {
		desc           string
		shutdownActive bool
	}{
		{
			desc:           "shutdown is active",
			shutdownActive: true,
		},
		{
			desc:           "shutdown is not active",
			shutdownActive: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			fakeSystemBus := &fakeSystemDBus{}
			bus := DBusCon{
				SystemBus: fakeSystemBus,
			}

			outChan, err := bus.MonitorShutdown(logger)
			assert.NoError(t, err)

			done := make(chan bool)

			go func() {
				select {
				case res := <-outChan:
					assert.Equal(t, tc.shutdownActive, res)
					done <- true
				case <-time.After(5 * time.Second):
					t.Errorf("Timed out waiting for shutdown message")
					done <- true
				}
			}()

			signal := &dbus.Signal{Body: []interface{}{tc.shutdownActive}}
			fakeSystemBus.signalChannel <- signal
			<-done
		})
	}
}

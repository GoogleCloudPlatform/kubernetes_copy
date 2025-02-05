//go:build linux
// +build linux

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

package iptables

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"
)

func getLockPaths() (string, string) {
	lock14x := fmt.Sprintf("@xtables-%d", time.Now().Nanosecond())
	lock16x := fmt.Sprintf("xtables-%d.lock", time.Now().Nanosecond())
	return lock14x, lock16x
}

type testCommand struct {
	command string
	action  fakeexec.FakeAction
}

// Creates a FakeExec that expects exactly commands to be run (and will fail otherwise).
func fakeExecForCommands(commands []testCommand) *fakeexec.FakeExec {
	fexec := &fakeexec.FakeExec{
		CommandScript: make([]fakeexec.FakeCommandAction, len(commands)),
		ExactOrder:    true,
	}
	for i := range commands {
		fcmd := fakeexec.FakeCmd{
			CombinedOutputScript: []fakeexec.FakeAction{commands[i].action},
		}
		argv := strings.Fields(commands[i].command)
		fexec.CommandScript[i] = func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, argv[0], argv[1:]...) }
	}
	return fexec
}

func TestFakeExecForCommands(t *testing.T) {
	var panicresult interface{}
	defer func() {
		panicresult = recover()
	}()

	fake1 := fakeExecForCommands([]testCommand{{
		command: "foo bar baz",
		action:  func() ([]byte, []byte, error) { return []byte("output"), nil, nil },
	}})
	cmd := fake1.Command("foo", "bar", "baz")
	out, err := cmd.CombinedOutput()
	if string(out) != "output" {
		t.Errorf("fake1: wrong output: expected %q, got %q", "output", out)
	}
	if err != nil {
		t.Errorf("fake1: expected no error, got %v", err)
	}
	if panicresult != nil {
		t.Errorf("fake1: expected no panic, got %q", panicresult)
	}

	fake2 := fakeExecForCommands([]testCommand{{
		command: "foo bar baz",
		action:  func() ([]byte, []byte, error) { return []byte("output"), nil, nil },
	}})
	_ = fake2.Command("foo", "baz")
	if panicresult == nil {
		t.Errorf("fake2: expected panic from FakeExec, got none")
	}
}

func TestNew(t *testing.T) {
	testCases := []struct {
		name     string
		commands []testCommand
		expected *runner
	}{
		{
			name: "ancient",
			commands: []testCommand{
				{
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.4.0"), nil, nil },
				},
				{
					// iptables-restore version check: ignores --version and just no-ops
					command: "iptables-restore --version",
					action:  func() ([]byte, []byte, error) { return nil, nil, nil },
				},
			},
			expected: &runner{
				hasCheck:        false,
				hasRandomFully:  false,
				waitFlag:        nil,
				restoreWaitFlag: nil,
			},
		},
		{
			name: "RHEL/CentOS 7",
			commands: []testCommand{
				{
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.4.21"), nil, nil },
				},
				{
					command: "iptables-restore --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables-restore v1.4.21"), nil, nil },
				},
			},
			expected: &runner{
				hasCheck:        true,
				hasRandomFully:  false,
				waitFlag:        []string{"-w"},
				restoreWaitFlag: []string{"-w"},
			},
		},
		{
			name: "1.6",
			commands: []testCommand{
				{
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.6.2"), nil, nil },
				},
			},
			expected: &runner{
				hasCheck:        true,
				hasRandomFully:  true,
				waitFlag:        []string{"-w", "5", "-W", "100000"},
				restoreWaitFlag: []string{"-w", "5", "-W", "100000"},
			},
		},
		{
			name: "1.8",
			commands: []testCommand{
				{
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.8.11"), nil, nil },
				},
			},
			expected: &runner{
				hasCheck:        true,
				hasRandomFully:  true,
				waitFlag:        []string{"-w", "5", "-W", "100000"},
				restoreWaitFlag: []string{"-w", "5", "-W", "100000"},
			},
		},
		{
			name: "no iptables",
			commands: []testCommand{
				{
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("no such file or directory") },
				},
			},
			expected: &runner{
				hasCheck:        false,
				hasRandomFully:  false,
				waitFlag:        nil,
				restoreWaitFlag: nil,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fexec := fakeExecForCommands(tc.commands)
			runner := newInternal(fexec, ProtocolIPv4, "", "").(*runner)

			if runner.hasCheck != tc.expected.hasCheck {
				t.Errorf("Expected hasCheck=%v, got %v", tc.expected.hasCheck, runner.hasCheck)
			}
			if runner.hasRandomFully != tc.expected.hasRandomFully {
				t.Errorf("Expected hasRandomFully=%v, got %v", tc.expected.hasRandomFully, runner.hasRandomFully)
			}
			if !reflect.DeepEqual(runner.waitFlag, tc.expected.waitFlag) {
				t.Errorf("Expected waitFlag=%v, got %v", tc.expected.waitFlag, runner.waitFlag)
			}
			if !reflect.DeepEqual(runner.restoreWaitFlag, tc.expected.restoreWaitFlag) {
				t.Errorf("Expected restoreWaitFlag=%v, got %v", tc.expected.restoreWaitFlag, runner.restoreWaitFlag)
			}
		})
	}
}

func TestNewDualStack(t *testing.T) {
	testCases := []struct {
		name     string
		commands []testCommand
		ipv4     bool
		ipv6     bool
	}{
		{
			name: "both available",
			commands: []testCommand{
				{
					// ipv4 creation
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.8.0"), nil, nil },
				},
				{
					// ipv4 Present()
					command: "iptables -w 5 -W 100000 -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, nil },
				},
				{
					// ipv6 creation
					command: "ip6tables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.8.0"), nil, nil },
				},
				{
					// ipv6 Present()
					command: "ip6tables -w 5 -W 100000 -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, nil },
				},
			},
			ipv4: true,
			ipv6: true,
		},
		{
			name: "ipv4 available, ipv6 not installed",
			commands: []testCommand{
				{
					// ipv4 creation
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.8.0"), nil, nil },
				},
				{
					// ipv4 Present()
					command: "iptables -w 5 -W 100000 -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, nil },
				},
				{
					// ipv6 creation
					command: "ip6tables --version",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("no such file or directory") },
				},
				{
					// ipv6 Present()
					command: "ip6tables -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("no such file or directory") },
				},
			},
			ipv4: true,
			ipv6: false,
		},
		{
			name: "ipv4 available, ipv6 disabled",
			commands: []testCommand{
				{
					// ipv4 creation
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.8.0"), nil, nil },
				},
				{
					// ipv4 Present()
					command: "iptables -w 5 -W 100000 -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, nil },
				},
				{
					// ipv6 creation
					command: "ip6tables --version",
					action:  func() ([]byte, []byte, error) { return []byte("iptables v1.8.0"), nil, nil },
				},
				{
					// ipv6 Present()
					command: "ip6tables -w 5 -W 100000 -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("ipv6 is broken") },
				},
			},
			ipv4: true,
			ipv6: false,
		},
		{
			name: "no iptables support",
			commands: []testCommand{
				{
					// ipv4 creation
					command: "iptables --version",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("no such file or directory") },
				},
				{
					// ipv4 Present()
					command: "iptables -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("no such file or directory") },
				},
				{
					// ipv6 creation
					command: "ip6tables --version",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("no such file or directory") },
				},
				{
					// ipv6 Present()
					command: "ip6tables -S POSTROUTING -t nat",
					action:  func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("no such file or directory") },
				},
			},
			ipv4: false,
			ipv6: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fexec := fakeExecForCommands(tc.commands)
			runners := newDualStackInternal(fexec)

			if tc.ipv4 && runners[v1.IPv4Protocol] == nil {
				t.Errorf("Expected ipv4 runner, got nil")
			} else if !tc.ipv4 && runners[v1.IPv4Protocol] != nil {
				t.Errorf("Expected no ipv4 runner, got one")
			}
			if tc.ipv6 && runners[v1.IPv6Protocol] == nil {
				t.Errorf("Expected ipv6 runner, got nil")
			} else if !tc.ipv6 && runners[v1.IPv6Protocol] != nil {
				t.Errorf("Expected no ipv6 runner, got one")
			}
		})
	}
}
func testEnsureChain(t *testing.T, protocol Protocol) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Exists.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
			// Failure.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 2} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, protocol, "", "")
	// Success.
	exists, err := runner.EnsureChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("%s new chain: Expected success, got %v", protocol, err)
	}
	if exists {
		t.Errorf("%s new chain: Expected exists = false", protocol)
	}
	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("%s new chain: Expected 2 CombinedOutput() calls, got %d", protocol, fcmd.CombinedOutputCalls)
	}
	cmd := iptablesCommand(protocol)
	if !sets.New(fcmd.CombinedOutputLog[1]...).HasAll(cmd, "-t", "nat", "-N", "FOOBAR") {
		t.Errorf("%s new chain: Expected cmd containing '%s -t nat -N FOOBAR', got %s", protocol, cmd, fcmd.CombinedOutputLog[2])
	}
	// Exists.
	exists, err = runner.EnsureChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("%s existing chain: Expected success, got %v", protocol, err)
	}
	if !exists {
		t.Errorf("%s existing chain: Expected exists = true", protocol)
	}
	// Simulate failure.
	_, err = runner.EnsureChain(TableNAT, Chain("FOOBAR"))
	if err == nil {
		t.Errorf("%s: Expected failure", protocol)
	}
}

func TestEnsureChainIPv4(t *testing.T) {
	testEnsureChain(t, ProtocolIPv4)
}

func TestEnsureChainIPv6(t *testing.T) {
	testEnsureChain(t, ProtocolIPv6)
}

func TestFlushChain(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Failure.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	// Success.
	err := runner.FlushChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[1]...).HasAll("iptables", "-t", "nat", "-F", "FOOBAR") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
	// Failure.
	err = runner.FlushChain(TableNAT, Chain("FOOBAR"))
	if err == nil {
		t.Errorf("expected failure")
	}
}

func TestDeleteChain(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Failure.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	// Success.
	err := runner.DeleteChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[1]...).HasAll("iptables", "-t", "nat", "-X", "FOOBAR") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
	// Failure.
	err = runner.DeleteChain(TableNAT, Chain("FOOBAR"))
	if err == nil {
		t.Errorf("expected failure")
	}
}

func TestEnsureRuleAlreadyExists(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Success of that exec means "done".
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	exists, err := runner.EnsureRule(Append, TableNAT, ChainOutput, "abc", "123")
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if !exists {
		t.Errorf("expected exists = true")
	}
	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[1]...).HasAll("iptables", "-t", "nat", "-C", "OUTPUT", "abc", "123") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
}

func TestEnsureRuleNew(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Status 1 on the first call.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
			// Success on the second call.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Failure of that means create it.
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	exists, err := runner.EnsureRule(Append, TableNAT, ChainOutput, "abc", "123")
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if exists {
		t.Errorf("expected exists = false")
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[2]...).HasAll("iptables", "-t", "nat", "-A", "OUTPUT", "abc", "123") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[3])
	}
}

func TestEnsureRuleErrorChecking(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Status 2 on the first call.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 2} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Failure of that means create it.
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	_, err := runner.EnsureRule(Append, TableNAT, ChainOutput, "abc", "123")
	if err == nil {
		t.Errorf("expected failure")
	}
	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
}

func TestEnsureRuleErrorCreating(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Status 1 on the first call.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
			// Status 1 on the second call.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Failure of that means create it.
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	_, err := runner.EnsureRule(Append, TableNAT, ChainOutput, "abc", "123")
	if err == nil {
		t.Errorf("expected failure")
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
}

func TestDeleteRuleDoesNotExist(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Status 1 on the first call.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Failure of that exec means "does not exist".
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteRule(TableNAT, ChainOutput, "abc", "123")
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[1]...).HasAll("iptables", "-t", "nat", "-C", "OUTPUT", "abc", "123") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
}

func TestDeleteRuleExists(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Success on the first call.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Success on the second call.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Success of that means delete it.
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteRule(TableNAT, ChainOutput, "abc", "123")
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[2]...).HasAll("iptables", "-t", "nat", "-D", "OUTPUT", "abc", "123") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[3])
	}
}

func TestDeleteRuleErrorChecking(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Status 2 on the first call.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 2} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Failure of that means create it.
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteRule(TableNAT, ChainOutput, "abc", "123")
	if err == nil {
		t.Errorf("expected failure")
	}
	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
}

func TestDeleteRuleErrorDeleting(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			// Success on the first call.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Status 1 on the second call.
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// The second Command() call is checking the rule.  Success of that means delete it.
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteRule(TableNAT, ChainOutput, "abc", "123")
	if err == nil {
		t.Errorf("expected failure")
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
}

func TestGetIPTablesHasCheckCommand(t *testing.T) {
	testCases := []struct {
		Version  string
		Expected bool
	}{
		{"iptables v1.4.7", false},
		{"iptables v1.4.11", true},
		{"iptables v1.4.19.1", true},
		{"iptables v2.0.0", true},
		{"total junk", false},
	}

	for _, testCase := range testCases {
		fcmd := fakeexec.FakeCmd{
			CombinedOutputScript: []fakeexec.FakeAction{
				func() ([]byte, []byte, error) { return []byte(testCase.Version), nil, nil },
				func() ([]byte, []byte, error) { return []byte(testCase.Version), nil, nil },
			},
		}
		fexec := &fakeexec.FakeExec{
			CommandScript: []fakeexec.FakeCommandAction{
				func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
				func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			},
		}
		ipt := newInternal(fexec, ProtocolIPv4, "", "")
		runner := ipt.(*runner)
		if testCase.Expected != runner.hasCheck {
			t.Errorf("Expected result: %v, Got result: %v", testCase.Expected, runner.hasCheck)
		}
	}
}

func TestIPTablesCommands(t *testing.T) {
	testCases := []struct {
		funcName    string
		protocol    Protocol
		expectedCmd string
	}{
		{"iptablesCommand", ProtocolIPv4, cmdIPTables},
		{"iptablesCommand", ProtocolIPv6, cmdIP6Tables},
		{"iptablesSaveCommand", ProtocolIPv4, cmdIPTablesSave},
		{"iptablesSaveCommand", ProtocolIPv6, cmdIP6TablesSave},
		{"iptablesRestoreCommand", ProtocolIPv4, cmdIPTablesRestore},
		{"iptablesRestoreCommand", ProtocolIPv6, cmdIP6TablesRestore},
	}
	for _, testCase := range testCases {
		var cmd string
		switch testCase.funcName {
		case "iptablesCommand":
			cmd = iptablesCommand(testCase.protocol)
		case "iptablesSaveCommand":
			cmd = iptablesSaveCommand(testCase.protocol)
		case "iptablesRestoreCommand":
			cmd = iptablesRestoreCommand(testCase.protocol)
		}
		if cmd != testCase.expectedCmd {
			t.Errorf("Function: %s, Expected result: %s, Actual result: %s", testCase.funcName, testCase.expectedCmd, cmd)
		}
	}
}

func TestCheckRuleWithoutCheckPresent(t *testing.T) {
	iptablesSaveOutput := `# Generated by iptables-save v1.4.7 on Wed Oct 29 14:56:01 2014
*nat
:PREROUTING ACCEPT [2136997:197881818]
:POSTROUTING ACCEPT [4284525:258542680]
:OUTPUT ACCEPT [5901660:357267963]
-A PREROUTING -m addrtype --dst-type LOCAL -m mark --mark 0x00004000/0x00004000 -j DOCKER
COMMIT
# Completed on Wed Oct 29 14:56:01 2014`

	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// Success.
			func() ([]byte, []byte, error) { return []byte(iptablesSaveOutput), nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// The first Command() call is checking the rule.  Success of that exec means "done".
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := &runner{exec: fexec}
	exists, err := runner.checkRuleWithoutCheck(
		TableNAT, ChainPrerouting,
		"-m", "addrtype",
		"-m", "mark", "--mark", "0x4000/0x4000",
		"-j", "DOCKER",
		"--dst-type", "LOCAL")
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if !exists {
		t.Errorf("expected exists = true")
	}
	if fcmd.CombinedOutputCalls != 1 {
		t.Errorf("expected 1 CombinedOutput() call, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[0]...).HasAll("iptables-save", "-t", "nat") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[0])
	}
}

func TestCheckRuleWithoutCheckAbsent(t *testing.T) {
	iptablesSaveOutput := `# Generated by iptables-save v1.4.7 on Wed Oct 29 14:56:01 2014
*nat
:PREROUTING ACCEPT [2136997:197881818]
:POSTROUTING ACCEPT [4284525:258542680]
:OUTPUT ACCEPT [5901660:357267963]
-A PREROUTING -m addrtype --dst-type LOCAL -j DOCKER
COMMIT
# Completed on Wed Oct 29 14:56:01 2014`

	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// Success.
			func() ([]byte, []byte, error) { return []byte(iptablesSaveOutput), nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// The first Command() call is checking the rule.  Success of that exec means "done".
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := &runner{exec: fexec}
	exists, err := runner.checkRuleWithoutCheck(TableNAT, ChainPrerouting, "-m", "addrtype", "-j", "DOCKER")
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if exists {
		t.Errorf("expected exists = false")
	}
	if fcmd.CombinedOutputCalls != 1 {
		t.Errorf("expected 1 CombinedOutput() call, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[0]...).HasAll("iptables-save", "-t", "nat") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[0])
	}
}

func TestIPTablesWaitFlag(t *testing.T) {
	testCases := []struct {
		Version string
		Result  []string
	}{
		{"0.55.55", nil},
		{"1.0.55", nil},
		{"1.4.19", nil},
		{"1.4.20", []string{WaitString}},
		{"1.4.21", []string{WaitString}},
		{"1.4.22", []string{WaitString, WaitSecondsValue}},
		{"1.5.0", []string{WaitString, WaitSecondsValue}},
		{"2.0.0", []string{WaitString, WaitSecondsValue, WaitIntervalString, WaitIntervalUsecondsValue}},
	}

	for _, testCase := range testCases {
		result := getIPTablesWaitFlag(utilversion.MustParseGeneric(testCase.Version))
		if !reflect.DeepEqual(result, testCase.Result) {
			t.Errorf("For %s expected %v got %v", testCase.Version, testCase.Result, result)
		}
	}
}

func TestWaitFlagUnavailable(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.19"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			// iptables version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			// iptables-restore version check
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if sets.New(fcmd.CombinedOutputLog[2]...).Has(WaitString) {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
}

func TestWaitFlagOld(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.20"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[2]...).HasAll("iptables", WaitString) {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
	if sets.New(fcmd.CombinedOutputLog[2]...).Has(WaitSecondsValue) {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
}

func TestWaitFlagNew(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.22"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[2]...).HasAll("iptables", WaitString, WaitSecondsValue) {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
}

func TestWaitIntervalFlagNew(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.6.1"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			// Success.
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, ProtocolIPv4, "", "")
	err := runner.DeleteChain(TableNAT, Chain("FOOBAR"))
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}
	if !sets.New(fcmd.CombinedOutputLog[2]...).HasAll("iptables", WaitString, WaitSecondsValue, WaitIntervalString, WaitIntervalUsecondsValue) {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
}

func testSaveInto(t *testing.T, protocol Protocol) {
	version := " v1.9.22"
	iptablesCmd := iptablesCommand(protocol)
	iptablesSaveCmd := iptablesSaveCommand(protocol)

	output := fmt.Sprintf(`# Generated by %s on Thu Jan 19 11:38:09 2017
*filter
:INPUT ACCEPT [15079:38410730]
:FORWARD ACCEPT [0:0]
:OUTPUT ACCEPT [11045:521562]
COMMIT
# Completed on Thu Jan 19 11:38:09 2017`, iptablesSaveCmd+version)

	stderrOutput := "#STDERR OUTPUT" // SaveInto() should should NOT capture stderr into the buffer

	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte(iptablesCmd + version), nil, nil },
		},
		RunScript: []fakeexec.FakeAction{
			func() ([]byte, []byte, error) { return []byte(output), []byte(stderrOutput), nil },
			func() ([]byte, []byte, error) { return nil, []byte(stderrOutput), &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, protocol, "", "")
	buffer := bytes.NewBuffer(nil)

	// Success.
	err := runner.SaveInto(TableNAT, buffer)
	if err != nil {
		t.Fatalf("%s: Expected success, got %v", protocol, err)
	}

	if buffer.String() != output {
		t.Errorf("%s: Expected output '%s', got '%v'", protocol, output, buffer.String())
	}

	if fcmd.CombinedOutputCalls != 1 {
		t.Errorf("%s: Expected 1 CombinedOutput() calls, got %d", protocol, fcmd.CombinedOutputCalls)
	}
	if fcmd.RunCalls != 1 {
		t.Errorf("%s: Expected 1 Run() call, got %d", protocol, fcmd.RunCalls)
	}
	if !sets.New(fcmd.RunLog[0]...).HasAll(iptablesSaveCmd, "-t", "nat") {
		t.Errorf("%s: Expected cmd containing '%s -t nat', got '%s'", protocol, iptablesSaveCmd, fcmd.RunLog[0])
	}

	// Failure.
	buffer.Reset()
	err = runner.SaveInto(TableNAT, buffer)
	if err == nil {
		t.Errorf("%s: Expected failure", protocol)
	}
	if buffer.String() != stderrOutput {
		t.Errorf("%s: Expected output '%s', got '%v'", protocol, stderrOutput, buffer.String())
	}
}

func TestSaveIntoIPv4(t *testing.T) {
	testSaveInto(t, ProtocolIPv4)
}

func TestSaveIntoIPv6(t *testing.T) {
	testSaveInto(t, ProtocolIPv6)
}

func testRestore(t *testing.T, protocol Protocol) {
	version := " v1.9.22"
	iptablesCmd := iptablesCommand(protocol)
	iptablesRestoreCmd := iptablesRestoreCommand(protocol)

	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte(iptablesCmd + version), nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	runner := newInternal(fexec, protocol, "", "")

	// both flags true
	err := runner.Restore(TableNAT, []byte{}, FlushTables, RestoreCounters)
	if err != nil {
		t.Errorf("%s flush,restore: Expected success, got %v", protocol, err)
	}

	commandSet := sets.New(fcmd.CombinedOutputLog[1]...)
	if !commandSet.HasAll(iptablesRestoreCmd, "-T", string(TableNAT), "--counters") || commandSet.HasAny("--noflush") {
		t.Errorf("%s flush, restore: Expected cmd containing '%s -T %s --counters', got '%s'", protocol, iptablesRestoreCmd, string(TableNAT), fcmd.CombinedOutputLog[1])
	}

	// FlushTables, NoRestoreCounters
	err = runner.Restore(TableNAT, []byte{}, FlushTables, NoRestoreCounters)
	if err != nil {
		t.Errorf("%s flush, no restore: Expected success, got %v", protocol, err)
	}

	commandSet = sets.New(fcmd.CombinedOutputLog[2]...)
	if !commandSet.HasAll(iptablesRestoreCmd, "-T", string(TableNAT)) || commandSet.HasAny("--noflush", "--counters") {
		t.Errorf("%s flush, no restore: Expected cmd containing '--noflush' or '--counters', got '%s'", protocol, fcmd.CombinedOutputLog[2])
	}

	// NoFlushTables, RestoreCounters
	err = runner.Restore(TableNAT, []byte{}, NoFlushTables, RestoreCounters)
	if err != nil {
		t.Errorf("%s no flush, restore: Expected success, got %v", protocol, err)
	}

	commandSet = sets.New(fcmd.CombinedOutputLog[3]...)
	if !commandSet.HasAll(iptablesRestoreCmd, "-T", string(TableNAT), "--noflush", "--counters") {
		t.Errorf("%s no flush, restore: Expected cmd containing '--noflush' and '--counters', got '%s'", protocol, fcmd.CombinedOutputLog[3])
	}

	// NoFlushTables, NoRestoreCounters
	err = runner.Restore(TableNAT, []byte{}, NoFlushTables, NoRestoreCounters)
	if err != nil {
		t.Errorf("%s no flush, no restore: Expected success, got %v", protocol, err)
	}

	commandSet = sets.New(fcmd.CombinedOutputLog[4]...)
	if !commandSet.HasAll(iptablesRestoreCmd, "-T", string(TableNAT), "--noflush") || commandSet.HasAny("--counters") {
		t.Errorf("%s no flush, no restore: Expected cmd containing '%s -T %s --noflush', got '%s'", protocol, iptablesRestoreCmd, string(TableNAT), fcmd.CombinedOutputLog[4])
	}

	if fcmd.CombinedOutputCalls != 5 {
		t.Errorf("%s: Expected 5 total CombinedOutput() calls, got %d", protocol, fcmd.CombinedOutputCalls)
	}

	// Failure.
	err = runner.Restore(TableNAT, []byte{}, FlushTables, RestoreCounters)
	if err == nil {
		t.Errorf("%s Expected a failure", protocol)
	}
}

func TestRestoreIPv4(t *testing.T) {
	testRestore(t, ProtocolIPv4)
}

func TestRestoreIPv6(t *testing.T) {
	testRestore(t, ProtocolIPv6)
}

// TestRestoreAll tests only the simplest use case, as flag handling code is already tested in TestRestore
func TestRestoreAll(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	lockPath14x, lockPath16x := getLockPaths()
	runner := newInternal(fexec, ProtocolIPv4, lockPath14x, lockPath16x)

	err := runner.RestoreAll([]byte{}, NoFlushTables, RestoreCounters)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	commandSet := sets.New(fcmd.CombinedOutputLog[1]...)
	if !commandSet.HasAll("iptables-restore", "--counters", "--noflush") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}

	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}

	// Failure.
	err = runner.Restore(TableNAT, []byte{}, FlushTables, RestoreCounters)
	if err == nil {
		t.Errorf("expected failure")
	}
}

// TestRestoreAllWait tests that the "wait" flag is passed to a compatible iptables-restore
func TestRestoreAllWait(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.9.22"), nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	lockPath14x, lockPath16x := getLockPaths()
	runner := newInternal(fexec, ProtocolIPv4, lockPath14x, lockPath16x)

	err := runner.RestoreAll([]byte{}, NoFlushTables, RestoreCounters)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	commandSet := sets.New(fcmd.CombinedOutputLog[1]...)
	if !commandSet.HasAll("iptables-restore", WaitString, WaitSecondsValue, WaitIntervalString, WaitIntervalUsecondsValue, "--counters", "--noflush") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[1])
	}

	if fcmd.CombinedOutputCalls != 2 {
		t.Errorf("expected 2 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}

	// Failure.
	err = runner.Restore(TableNAT, []byte{}, FlushTables, RestoreCounters)
	if err == nil {
		t.Errorf("expected failure")
	}
}

// TestRestoreAllWaitOldIptablesRestore tests that the "wait" flag is not passed
// to an old iptables-restore
func TestRestoreAllWaitOldIptablesRestore(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.22"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	lockPath14x, lockPath16x := getLockPaths()
	// the lockPath14x is a UNIX socket which is cleaned up automatically on close, but the
	// lockPath16x is a plain file which is not cleaned up.
	defer os.Remove(lockPath16x)
	runner := newInternal(fexec, ProtocolIPv4, lockPath14x, lockPath16x)

	err := runner.RestoreAll([]byte{}, NoFlushTables, RestoreCounters)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	commandSet := sets.New(fcmd.CombinedOutputLog[2]...)
	if !commandSet.HasAll("iptables-restore", "--counters", "--noflush") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
	if commandSet.HasAll(WaitString) {
		t.Errorf("wrong CombinedOutput() log (unexpected %s option), got %s", WaitString, fcmd.CombinedOutputLog[1])
	}

	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}

	// Failure.
	err = runner.Restore(TableNAT, []byte{}, FlushTables, RestoreCounters)
	if err == nil {
		t.Errorf("expected failure")
	}
}

// TestRestoreAllGrabNewLock tests that the iptables code will grab the
// iptables /run lock when using an iptables-restore version that does not
// support the --wait argument
func TestRestoreAllGrabNewLock(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.22"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	lockPath14x, lockPath16x := getLockPaths()
	runner := newInternal(fexec, ProtocolIPv4, lockPath14x, lockPath16x)

	// Grab the /run lock and ensure the RestoreAll fails
	runLock, err := os.OpenFile(lockPath16x, os.O_CREATE, 0600)
	if err != nil {
		t.Fatalf("expected to open %s, got %v", lockPath16x, err)
	}
	defer func() {
		runLock.Close()
		os.Remove(lockPath16x)
	}()

	if err := grabIptablesFileLock(runLock); err != nil {
		t.Errorf("expected to lock %s, got %v", lockPath16x, err)
	}

	err = runner.RestoreAll([]byte{}, NoFlushTables, RestoreCounters)
	if err == nil {
		t.Fatal("expected failure, got success instead")
	}
	if !strings.Contains(err.Error(), "failed to acquire new iptables lock: timed out waiting for the condition") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

// TestRestoreAllGrabOldLock tests that the iptables code will grab the
// iptables @xtables abstract unix socket lock when using an iptables-restore
// version that does not support the --wait argument
func TestRestoreAllGrabOldLock(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.22"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	lockPath14x, lockPath16x := getLockPaths()
	// the lockPath14x is a UNIX socket which is cleaned up automatically on close, but the
	// lockPath16x is a plain file which is not cleaned up.
	defer os.Remove(lockPath16x)
	runner := newInternal(fexec, ProtocolIPv4, lockPath14x, lockPath16x)

	var runLock *net.UnixListener
	// Grab the abstract @xtables socket, will retry if the socket exists
	err := wait.PollImmediate(time.Second, wait.ForeverTestTimeout, func() (done bool, err error) {
		runLock, err = net.ListenUnix("unix", &net.UnixAddr{Name: lockPath14x, Net: "unix"})
		if err != nil {
			t.Logf("Failed to lock %s: %v, will retry.", lockPath14x, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Timed out locking %s", lockPath14x)
	}
	if runLock == nil {
		t.Fatal("Unexpected nil runLock")
	}

	defer runLock.Close()

	err = runner.RestoreAll([]byte{}, NoFlushTables, RestoreCounters)
	if err == nil {
		t.Fatal("expected failure, got success instead")
	}
	if !strings.Contains(err.Error(), "failed to acquire old iptables lock: timed out waiting for the condition") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

// TestRestoreAllWaitBackportedIptablesRestore tests that the "wait" flag is passed
// to a seemingly-old-but-actually-new iptables-restore
func TestRestoreAllWaitBackportedIptablesRestore(t *testing.T) {
	fcmd := fakeexec.FakeCmd{
		CombinedOutputScript: []fakeexec.FakeAction{
			// iptables version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.22"), nil, nil },
			// iptables-restore version check
			func() ([]byte, []byte, error) { return []byte("iptables v1.4.22"), nil, nil },
			func() ([]byte, []byte, error) { return []byte{}, nil, nil },
			func() ([]byte, []byte, error) { return nil, nil, &fakeexec.FakeExitError{Status: 1} },
		},
	}
	fexec := &fakeexec.FakeExec{
		CommandScript: []fakeexec.FakeCommandAction{
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
			func(cmd string, args ...string) exec.Cmd { return fakeexec.InitFakeCmd(&fcmd, cmd, args...) },
		},
	}
	lockPath14x, lockPath16x := getLockPaths()
	runner := newInternal(fexec, ProtocolIPv4, lockPath14x, lockPath16x)

	err := runner.RestoreAll([]byte{}, NoFlushTables, RestoreCounters)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	commandSet := sets.New(fcmd.CombinedOutputLog[2]...)
	if !commandSet.HasAll("iptables-restore", "--counters", "--noflush") {
		t.Errorf("wrong CombinedOutput() log, got %s", fcmd.CombinedOutputLog[2])
	}
	if !commandSet.HasAll(WaitString) {
		t.Errorf("wrong CombinedOutput() log (expected %s option), got %s", WaitString, fcmd.CombinedOutputLog[1])
	}

	if fcmd.CombinedOutputCalls != 3 {
		t.Errorf("expected 3 CombinedOutput() calls, got %d", fcmd.CombinedOutputCalls)
	}

	// Failure.
	err = runner.Restore(TableNAT, []byte{}, FlushTables, RestoreCounters)
	if err == nil {
		t.Errorf("expected failure")
	}
}

// TestExtractLines tests that
func TestExtractLines(t *testing.T) {
	mkLines := func(lines ...LineData) []LineData {
		return lines
	}
	lines := "Line1: 1\nLine2: 2\nLine3: 3\nLine4: 4\nLine5: 5\nLine6: 6\nLine7: 7\nLine8: 8\nLine9: 9\nLine10: 10"
	tests := []struct {
		count int
		line  int
		name  string
		want  []LineData
	}{{
		name:  "test-line-0",
		count: 3,
		line:  0,
		want:  nil,
	}, {
		name:  "test-count-0",
		count: 0,
		line:  3,
		want:  mkLines(LineData{3, "Line3: 3"}),
	}, {
		name:  "test-common-cases",
		count: 3,
		line:  6,
		want: mkLines(
			LineData{3, "Line3: 3"},
			LineData{4, "Line4: 4"},
			LineData{5, "Line5: 5"},
			LineData{6, "Line6: 6"},
			LineData{7, "Line7: 7"},
			LineData{8, "Line8: 8"},
			LineData{9, "Line9: 9"}),
	}, {
		name:  "test4-bound-cases",
		count: 11,
		line:  10,
		want: mkLines(
			LineData{1, "Line1: 1"},
			LineData{2, "Line2: 2"},
			LineData{3, "Line3: 3"},
			LineData{4, "Line4: 4"},
			LineData{5, "Line5: 5"},
			LineData{6, "Line6: 6"},
			LineData{7, "Line7: 7"},
			LineData{8, "Line8: 8"},
			LineData{9, "Line9: 9"},
			LineData{10, "Line10: 10"}),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractLines([]byte(lines), tt.line, tt.count)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got = %v, want = %v", got, tt.want)
			}
		})
	}
}

/*
Copyright 2017 The Kubernetes Authors.

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

package dns

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/events"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	netutils "k8s.io/utils/net"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	// testHostNameserver and testHostDomain are also being used in dns_windows_test.go.
	testHostNameserver = "8.8.8.8"
	testHostDomain     = "host.domain"
	fetchEvent         = func(recorder *events.FakeRecorder) string {
		select {
		case event := <-recorder.Events:
			return event
		default:
			return ""
		}
	}
)

func TestParseResolvConf(t *testing.T) {
	testCases := []struct {
		data        string
		nameservers []string
		searches    []string
		options     []string
		isErr       bool
	}{
		{"", []string{}, []string{}, []string{}, false},
		{" ", []string{}, []string{}, []string{}, false},
		{"\n", []string{}, []string{}, []string{}, false},
		{"\t\n\t", []string{}, []string{}, []string{}, false},
		{"#comment\n", []string{}, []string{}, []string{}, false},
		{" #comment\n", []string{}, []string{}, []string{}, false},
		{"#comment\n#comment", []string{}, []string{}, []string{}, false},
		{"#comment\nnameserver", []string{}, []string{}, []string{}, true},                           // nameserver empty
		{"#comment\nnameserver\nsearch", []string{}, []string{}, []string{}, true},                   // nameserver and search empty
		{"#comment\nnameserver 1.2.3.4\nsearch", []string{"1.2.3.4"}, []string{}, []string{}, false}, // nameserver specified and search empty
		{"nameserver 1.2.3.4", []string{"1.2.3.4"}, []string{}, []string{}, false},
		{" nameserver 1.2.3.4", []string{"1.2.3.4"}, []string{}, []string{}, false},
		{"\tnameserver 1.2.3.4", []string{"1.2.3.4"}, []string{}, []string{}, false},
		{"nameserver\t1.2.3.4", []string{"1.2.3.4"}, []string{}, []string{}, false},
		{"nameserver \t 1.2.3.4", []string{"1.2.3.4"}, []string{}, []string{}, false},
		{"nameserver 1.2.3.4\nnameserver 5.6.7.8", []string{"1.2.3.4", "5.6.7.8"}, []string{}, []string{}, false},
		{"nameserver 1.2.3.4 #comment", []string{"1.2.3.4"}, []string{}, []string{}, false},
		{"search ", []string{}, []string{}, []string{}, false},  // search empty
		{"search .", []string{}, []string{}, []string{}, false}, // ignore lone dot
		{"search . foo", []string{}, []string{"foo"}, []string{}, false},
		{"search foo .", []string{}, []string{"foo"}, []string{}, false},
		{"search foo .  bar", []string{}, []string{"foo", "bar"}, []string{}, false},
		{"search foo", []string{}, []string{"foo"}, []string{}, false},
		{"search foo bar", []string{}, []string{"foo", "bar"}, []string{}, false},
		{"search foo. bar", []string{}, []string{"foo", "bar"}, []string{}, false},
		{"search foo bar bat\n", []string{}, []string{"foo", "bar", "bat"}, []string{}, false},
		{"search foo\nsearch bar", []string{}, []string{"bar"}, []string{}, false},
		{"nameserver 1.2.3.4\nsearch foo bar", []string{"1.2.3.4"}, []string{"foo", "bar"}, []string{}, false},
		{"nameserver 1.2.3.4\nsearch foo\nnameserver 5.6.7.8\nsearch bar", []string{"1.2.3.4", "5.6.7.8"}, []string{"bar"}, []string{}, false},
		{"#comment\nnameserver 1.2.3.4\n#comment\nsearch foo\ncomment", []string{"1.2.3.4"}, []string{"foo"}, []string{}, false},
		{"options ", []string{}, []string{}, []string{}, false},
		{"options ndots:5 attempts:2", []string{}, []string{}, []string{"ndots:5", "attempts:2"}, false},
		{"options ndots:1 ndots:5 attempts:2", []string{}, []string{}, []string{"ndots:5", "attempts:2"}, false},
		{"options ndots:1 edns0 attempts:2 single-request-reopen", []string{}, []string{}, []string{"edns0", "attempts:2", "single-request-reopen", "ndots:1"}, false},
		{"options ndots:1\noptions ndots:5 attempts:3", []string{}, []string{}, []string{"ndots:5", "attempts:3"}, false},
		{"options ndots:1 timeout:3 timeout:1 attempts:3\noptions ndots:5", []string{}, []string{}, []string{"ndots:5", "timeout:1", "attempts:3"}, false},
		{"options ndots:1 attempts:3\noptions ndots:1 attempts:3 ndots:5", []string{}, []string{}, []string{"ndots:5", "attempts:3"}, false},
		{"nameserver 1.2.3.4\nsearch foo\nnameserver 5.6.7.8\nsearch bar\noptions ndots:5 attempts:4", []string{"1.2.3.4", "5.6.7.8"}, []string{"bar"}, []string{"ndots:5", "attempts:4"}, false},
	}
	for i, tc := range testCases {
		ns, srch, opts, err := parseResolvConf(strings.NewReader(tc.data))
		if !tc.isErr {
			require.NoError(t, err)
			assert.EqualValues(t, tc.nameservers, ns, "test case [%d]: name servers", i)
			assert.EqualValues(t, tc.searches, srch, "test case [%d] searches", i)
			assert.EqualValues(t, sets.New[string](tc.options...), sets.New[string](opts...), "test case [%d] options", i)
		} else {
			require.Error(t, err, "tc.searches %v", tc.searches)
		}
	}
}

func TestFormDNSSearchFitsLimits(t *testing.T) {
	searchPathList2048Chars := []string{
		// 2048 = 128 + 127 * 15 + 15
		strings.Repeat("A", 128),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
		strings.Repeat("A", 127),
	}

	recorder := events.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string("testNode"),
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"

	configurer := NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         "",
			Name:        "test_pod",
			Namespace:   "testNS",
			Annotations: map[string]string{},
		},
	}

	testCases := []struct {
		desc         string
		hostNames    []string
		resultSearch []string
		events       []string
	}{
		{
			desc:         "valid: 3 search paths",
			hostNames:    []string{"testNS.svc.TEST", "svc.TEST", "TEST"},
			resultSearch: []string{"testNS.svc.TEST", "svc.TEST", "TEST"},
			events:       []string{},
		},

		{
			desc:         "valid: 5 search paths",
			hostNames:    []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", "BBB"},
			resultSearch: []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", "BBB"},
			events:       []string{},
		},

		{
			desc:         "invalid: longer than 256 characters in search path list",
			hostNames:    []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", strings.Repeat("B", 256), "BBB"},
			resultSearch: []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", "BBB"},
			events:       []string{"Search Line limits were exceeded, some search paths have been omitted, the applied search line is: testNS.svc.TEST svc.TEST TEST AAA BBB"},
		},

		{
			desc:         "valid: 2048 characters in search path list",
			hostNames:    searchPathList2048Chars,
			resultSearch: searchPathList2048Chars,
			events:       []string{},
		},

		{
			desc:         "invalid: 2050 characters in search path list",
			hostNames:    append(searchPathList2048Chars, "B"),
			resultSearch: searchPathList2048Chars,
			events:       []string{fmt.Sprintf("Search Line limits were exceeded, some search paths have been omitted, the applied search line is: %s", strings.Join(searchPathList2048Chars, " "))},
		},

		{
			desc:         "invalid: 256 characters search path",
			hostNames:    []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", strings.Repeat("B", 256), "BBB"},
			resultSearch: []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", "BBB"},
			events:       []string{"Search Line limits were exceeded, some search paths have been omitted, the applied search line is: testNS.svc.TEST svc.TEST TEST AAA BBB"},
		},

		{
			desc:         "valid: 7 search paths",
			hostNames:    []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", "BBB", "CCC", "DDD"},
			resultSearch: []string{"testNS.svc.TEST", "svc.TEST", "TEST", "AAA", "BBB", "CCC", "DDD"},
			events:       []string{},
		},

		{
			desc:         "valid: 32 search paths",
			hostNames:    []string{"testNS.svc.TEST", "svc.TEST", "TEST", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31", "32"},
			resultSearch: []string{"testNS.svc.TEST", "svc.TEST", "TEST", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31", "32"},
			events:       []string{},
		},

		{
			desc:         "invalid: 33 search paths",
			hostNames:    []string{"testNS.svc.TEST", "svc.TEST", "TEST", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31", "32", "33"},
			resultSearch: []string{"testNS.svc.TEST", "svc.TEST", "TEST", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31", "32"},
			events:       []string{"Search Line limits were exceeded, some search paths have been omitted, the applied search line is: testNS.svc.TEST svc.TEST TEST 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32"},
		},
	}

	for i, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			dnsSearch := configurer.formDNSSearchFitsLimits(tc.hostNames, pod)
			assert.EqualValues(t, tc.resultSearch, dnsSearch, "test [%d]", i)
			for _, expectedEvent := range tc.events {
				expected := fmt.Sprintf("%s %s %s", v1.EventTypeWarning, "DNSConfigForming", expectedEvent)
				event := fetchEvent(recorder)
				assert.Equal(t, expected, event, "test [%d]", i)
			}
		})
	}
}

func TestFormDNSNameserversFitsLimits(t *testing.T) {
	recorder := events.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string("testNode"),
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"

	configurer := NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         "",
			Name:        "test_pod",
			Namespace:   "testNS",
			Annotations: map[string]string{},
		},
	}

	testCases := []struct {
		desc               string
		nameservers        []string
		expectedNameserver []string
		expectedEvent      bool
	}{
		{
			desc:               "valid: 1 nameserver",
			nameservers:        []string{"127.0.0.1"},
			expectedNameserver: []string{"127.0.0.1"},
			expectedEvent:      false,
		},
		{
			desc:               "valid: 3 nameservers",
			nameservers:        []string{"127.0.0.1", "10.0.0.10", "8.8.8.8"},
			expectedNameserver: []string{"127.0.0.1", "10.0.0.10", "8.8.8.8"},
			expectedEvent:      false,
		},
		{
			desc:               "invalid: 4 nameservers, trimmed to 3",
			nameservers:        []string{"127.0.0.1", "10.0.0.10", "8.8.8.8", "1.2.3.4"},
			expectedNameserver: []string{"127.0.0.1", "10.0.0.10", "8.8.8.8"},
			expectedEvent:      true,
		},
	}

	for _, tc := range testCases {
		appliedNameservers := configurer.formDNSNameserversFitsLimits(tc.nameservers, pod)
		assert.EqualValues(t, tc.expectedNameserver, appliedNameservers, tc.desc)
		event := fetchEvent(recorder)
		if tc.expectedEvent && len(event) == 0 {
			t.Errorf("%s: formDNSNameserversFitsLimits(%v) expected event, got no event.", tc.desc, tc.nameservers)
		} else if !tc.expectedEvent && len(event) > 0 {
			t.Errorf("%s: formDNSNameserversFitsLimits(%v) expected no event, got event: %v", tc.desc, tc.nameservers, event)
		}
	}
}

func TestMergeDNSOptions(t *testing.T) {
	testOptionValue := "3"

	testCases := []struct {
		desc                     string
		existingDNSConfigOptions []string
		dnsConfigOptions         []v1.PodDNSConfigOption
		expectedOptions          []string
	}{
		{
			desc:                     "Empty dnsConfigOptions",
			existingDNSConfigOptions: []string{"ndots:5", "debug"},
			dnsConfigOptions:         nil,
			expectedOptions:          []string{"ndots:5", "debug"},
		},
		{
			desc:                     "No duplicated entries",
			existingDNSConfigOptions: []string{"ndots:5", "debug"},
			dnsConfigOptions: []v1.PodDNSConfigOption{
				{Name: "single-request"},
				{Name: "attempts", Value: &testOptionValue},
			},
			expectedOptions: []string{"ndots:5", "debug", "single-request", "attempts:3"},
		},
		{
			desc:                     "Overwrite duplicated entries",
			existingDNSConfigOptions: []string{"ndots:5", "debug"},
			dnsConfigOptions: []v1.PodDNSConfigOption{
				{Name: "ndots", Value: &testOptionValue},
				{Name: "debug"},
				{Name: "single-request"},
				{Name: "attempts", Value: &testOptionValue},
			},
			expectedOptions: []string{"ndots:3", "debug", "single-request", "attempts:3"},
		},
	}

	for _, tc := range testCases {
		options := mergeDNSOptions(tc.existingDNSConfigOptions, tc.dnsConfigOptions)
		// Options order may be changed after conversion.
		if !sets.New[string](options...).Equal(sets.New[string](tc.expectedOptions...)) {
			t.Errorf("%s: mergeDNSOptions(%v, %v)=%v, want %v", tc.desc, tc.existingDNSConfigOptions, tc.dnsConfigOptions, options, tc.expectedOptions)
		}
	}
}

func TestGetPodDNSType(t *testing.T) {
	recorder := events.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string("testNode"),
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	testClusterDNSDomain := "TEST"
	clusterNS := "203.0.113.1"
	testClusterDNS := []net.IP{netutils.ParseIPSloppy(clusterNS)}

	configurer := NewConfigurer(recorder, nodeRef, nil, nil, testClusterDNSDomain, "")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:         "",
			Name:        "test_pod",
			Namespace:   "testNS",
			Annotations: map[string]string{},
		},
	}

	testCases := []struct {
		desc            string
		hasClusterDNS   bool
		hostNetwork     bool
		dnsPolicy       v1.DNSPolicy
		expectedDNSType podDNSType
		expectedError   bool
	}{
		{
			desc:            "valid DNSClusterFirst without hostnetwork",
			hasClusterDNS:   true,
			dnsPolicy:       v1.DNSClusterFirst,
			expectedDNSType: podDNSCluster,
		},
		{
			desc:            "valid DNSClusterFirstWithHostNet with hostnetwork",
			hasClusterDNS:   true,
			hostNetwork:     true,
			dnsPolicy:       v1.DNSClusterFirstWithHostNet,
			expectedDNSType: podDNSCluster,
		},
		{
			desc:            "valid DNSClusterFirstWithHostNet without hostnetwork",
			hasClusterDNS:   true,
			dnsPolicy:       v1.DNSClusterFirstWithHostNet,
			expectedDNSType: podDNSCluster,
		},
		{
			desc:            "valid DNSDefault without hostnetwork",
			dnsPolicy:       v1.DNSDefault,
			expectedDNSType: podDNSHost,
		},
		{
			desc:            "valid DNSDefault with hostnetwork",
			hostNetwork:     true,
			dnsPolicy:       v1.DNSDefault,
			expectedDNSType: podDNSHost,
		},
		{
			desc:            "DNSClusterFirst with hostnetwork, fallback to DNSDefault",
			hasClusterDNS:   true,
			hostNetwork:     true,
			dnsPolicy:       v1.DNSClusterFirst,
			expectedDNSType: podDNSHost,
		},
		{
			desc:            "valid DNSNone",
			dnsPolicy:       v1.DNSNone,
			expectedDNSType: podDNSNone,
		},
		{
			desc:          "invalid DNS policy, should return error",
			dnsPolicy:     "invalidPolicy",
			expectedError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			if tc.hasClusterDNS {
				configurer.clusterDNS = testClusterDNS
			} else {
				configurer.clusterDNS = nil
			}
			pod.Spec.DNSPolicy = tc.dnsPolicy
			pod.Spec.HostNetwork = tc.hostNetwork

			resType, err := getPodDNSType(pod)
			if tc.expectedError {
				if err == nil {
					t.Errorf("%s: GetPodDNSType(%v) got no error, want error", tc.desc, pod)
				}
				return
			}
			if resType != tc.expectedDNSType {
				t.Errorf("%s: GetPodDNSType(%v)=%v, want %v", tc.desc, pod, resType, tc.expectedDNSType)
			}
		})
	}
}

func TestGetPodDNS(t *testing.T) {
	recorder := events.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string("testNode"),
		UID:       types.UID("testNode"),
		Namespace: "",
	}
	clusterNS := "203.0.113.1"
	testClusterDNSDomain := "kubernetes.io"
	testClusterDNS := []net.IP{netutils.ParseIPSloppy(clusterNS)}

	configurer := NewConfigurer(recorder, nodeRef, nil, testClusterDNS, testClusterDNSDomain, "")

	pods := newTestPods(4)
	pods[0].Spec.DNSPolicy = v1.DNSClusterFirstWithHostNet
	pods[1].Spec.DNSPolicy = v1.DNSClusterFirst
	pods[2].Spec.DNSPolicy = v1.DNSClusterFirst
	pods[2].Spec.HostNetwork = false
	pods[3].Spec.DNSPolicy = v1.DNSDefault

	options := make([]struct {
		DNS       []string
		DNSSearch []string
	}, 4)
	for i, pod := range pods {
		var err error
		dnsConfig, err := configurer.GetPodDNS(pod)
		if err != nil {
			t.Fatalf("failed to generate container options: %v", err)
		}
		options[i].DNS, options[i].DNSSearch = dnsConfig.Servers, dnsConfig.Searches
	}
	if len(options[0].DNS) != 1 || options[0].DNS[0] != clusterNS {
		t.Errorf("expected nameserver %s, got %+v", clusterNS, options[0].DNS)
	}
	if len(options[0].DNSSearch) == 0 || options[0].DNSSearch[0] != ".svc."+configurer.ClusterDomain {
		t.Errorf("expected search %s, got %+v", ".svc."+configurer.ClusterDomain, options[0].DNSSearch)
	}
	if len(options[1].DNS) != 1 || options[1].DNS[0] != "127.0.0.1" {
		t.Errorf("expected nameserver 127.0.0.1, got %+v", options[1].DNS)
	}
	if len(options[1].DNSSearch) != 1 || options[1].DNSSearch[0] != "." {
		t.Errorf("expected search \".\", got %+v", options[1].DNSSearch)
	}
	if len(options[2].DNS) != 1 || options[2].DNS[0] != clusterNS {
		t.Errorf("expected nameserver %s, got %+v", clusterNS, options[2].DNS)
	}
	if len(options[2].DNSSearch) == 0 || options[2].DNSSearch[0] != ".svc."+configurer.ClusterDomain {
		t.Errorf("expected search %s, got %+v", ".svc."+configurer.ClusterDomain, options[2].DNSSearch)
	}
	if len(options[3].DNS) != 1 || options[3].DNS[0] != "127.0.0.1" {
		t.Errorf("expected nameserver 127.0.0.1, got %+v", options[3].DNS)
	}
	if len(options[3].DNSSearch) != 1 || options[3].DNSSearch[0] != "." {
		t.Errorf("expected search \".\", got %+v", options[3].DNSSearch)
	}

	configurer = NewConfigurer(recorder, nodeRef, nil, testClusterDNS, testClusterDNSDomain, defaultResolvConf)
	configurer.getHostDNSConfig = fakeGetHostDNSConfigCustom
	for i, pod := range pods {
		var err error
		dnsConfig, err := configurer.GetPodDNS(pod)
		if err != nil {
			t.Fatalf("failed to generate container options: %v", err)
		}
		options[i].DNS, options[i].DNSSearch = dnsConfig.Servers, dnsConfig.Searches
	}
	t.Logf("nameservers %+v", options[1].DNS)
	if len(options[0].DNS) != 1 {
		t.Errorf("expected cluster nameserver only, got %+v", options[0].DNS)
	} else if options[0].DNS[0] != clusterNS {
		t.Errorf("expected nameserver %s, got %v", clusterNS, options[0].DNS[0])
	}
	expLength := len(options[1].DNSSearch) + 3

	maxDNSSearchPaths := validation.MaxDNSSearchPaths

	if expLength > maxDNSSearchPaths {
		expLength = maxDNSSearchPaths
	}
	if len(options[0].DNSSearch) != expLength {
		t.Errorf("expected prepend of cluster domain, got %+v", options[0].DNSSearch)
	} else if options[0].DNSSearch[0] != ".svc."+configurer.ClusterDomain {
		t.Errorf("expected domain %s, got %s", ".svc."+configurer.ClusterDomain, options[0].DNSSearch)
	}
	if len(options[2].DNS) != 1 {
		t.Errorf("expected cluster nameserver only, got %+v", options[2].DNS)
	} else if options[2].DNS[0] != clusterNS {
		t.Errorf("expected nameserver %s, got %v", clusterNS, options[2].DNS[0])
	}
	if len(options[2].DNSSearch) != expLength {
		t.Errorf("expected prepend of cluster domain, got %+v", options[2].DNSSearch)
	} else if options[2].DNSSearch[0] != ".svc."+configurer.ClusterDomain {
		t.Errorf("expected domain %s, got %s", ".svc."+configurer.ClusterDomain, options[0].DNSSearch)
	}
}

func TestGetPodDNSCustom(t *testing.T) {
	recorder := events.NewFakeRecorder(20)
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string("testNode"),
		UID:       types.UID("testNode"),
		Namespace: "",
	}

	testPodNamespace := "testNS"
	testClusterNameserver := "10.0.0.10"
	testClusterDNSDomain := "kubernetes.io"
	testSvcDomain := fmt.Sprintf("svc.%s", testClusterDNSDomain)
	testNsSvcDomain := fmt.Sprintf("%s.svc.%s", testPodNamespace, testClusterDNSDomain)
	testNdotsOptionValue := "3"

	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test_pod",
			Namespace: testPodNamespace,
		},
	}

	resolvConfContent := []byte(fmt.Sprintf("nameserver %s\nsearch %s\n", testHostNameserver, testHostDomain))
	tmpfile, err := os.CreateTemp("", "tmpResolvConf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())
	if _, err := tmpfile.Write(resolvConfContent); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	configurer := NewConfigurer(recorder, nodeRef, nil, []net.IP{netutils.ParseIPSloppy(testClusterNameserver)}, testClusterDNSDomain, tmpfile.Name())

	testCases := []struct {
		desc              string
		hostnetwork       bool
		dnsPolicy         v1.DNSPolicy
		dnsConfig         *v1.PodDNSConfig
		expectedDNSConfig *runtimeapi.DNSConfig
	}{
		{
			desc:              "DNSNone without DNSConfig should have empty DNS settings",
			dnsPolicy:         v1.DNSNone,
			expectedDNSConfig: &runtimeapi.DNSConfig{},
		},
		{
			desc:      "DNSNone with DNSConfig should have a merged DNS settings",
			dnsPolicy: v1.DNSNone,
			dnsConfig: &v1.PodDNSConfig{
				Nameservers: []string{"203.0.113.1"},
				Searches:    []string{"my.domain", "second.domain"},
				Options: []v1.PodDNSConfigOption{
					{Name: "ndots", Value: &testNdotsOptionValue},
					{Name: "debug"},
				},
			},
			expectedDNSConfig: &runtimeapi.DNSConfig{
				Servers:  []string{"203.0.113.1"},
				Searches: []string{"my.domain", "second.domain"},
				Options:  []string{"ndots:3", "debug"},
			},
		},
		{
			desc:      "DNSClusterFirst with DNSConfig should have a merged DNS settings",
			dnsPolicy: v1.DNSClusterFirst,
			dnsConfig: &v1.PodDNSConfig{
				Nameservers: []string{"10.0.0.11"},
				Searches:    []string{"my.domain"},
				Options: []v1.PodDNSConfigOption{
					{Name: "ndots", Value: &testNdotsOptionValue},
					{Name: "debug"},
				},
			},
			expectedDNSConfig: &runtimeapi.DNSConfig{
				Servers:  []string{testClusterNameserver, "10.0.0.11"},
				Searches: []string{testNsSvcDomain, testSvcDomain, testClusterDNSDomain, testHostDomain, "my.domain"},
				Options:  []string{"ndots:3", "debug"},
			},
		},
		{
			desc:        "DNSClusterFirstWithHostNet with DNSConfig should have a merged DNS settings",
			hostnetwork: true,
			dnsPolicy:   v1.DNSClusterFirstWithHostNet,
			dnsConfig: &v1.PodDNSConfig{
				Nameservers: []string{"10.0.0.11"},
				Searches:    []string{"my.domain"},
				Options: []v1.PodDNSConfigOption{
					{Name: "ndots", Value: &testNdotsOptionValue},
					{Name: "debug"},
				},
			},
			expectedDNSConfig: &runtimeapi.DNSConfig{
				Servers:  []string{testClusterNameserver, "10.0.0.11"},
				Searches: []string{testNsSvcDomain, testSvcDomain, testClusterDNSDomain, testHostDomain, "my.domain"},
				Options:  []string{"ndots:3", "debug"},
			},
		},
		{
			desc:      "DNSDefault with DNSConfig should have a merged DNS settings",
			dnsPolicy: v1.DNSDefault,
			dnsConfig: &v1.PodDNSConfig{
				Nameservers: []string{"10.0.0.11"},
				Searches:    []string{"my.domain"},
				Options: []v1.PodDNSConfigOption{
					{Name: "ndots", Value: &testNdotsOptionValue},
					{Name: "debug"},
				},
			},
			expectedDNSConfig: &runtimeapi.DNSConfig{
				Servers:  []string{testHostNameserver, "10.0.0.11"},
				Searches: []string{testHostDomain, "my.domain"},
				Options:  []string{"ndots:3", "debug"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			testPod.Spec.HostNetwork = tc.hostnetwork
			testPod.Spec.DNSConfig = tc.dnsConfig
			testPod.Spec.DNSPolicy = tc.dnsPolicy

			resDNSConfig, err := configurer.GetPodDNS(testPod)
			if err != nil {
				t.Errorf("%s: GetPodDNS(%v), unexpected error: %v", tc.desc, testPod, err)
			}
			if !dnsConfigsAreEqual(resDNSConfig, tc.expectedDNSConfig) {
				t.Errorf("%s: GetPodDNS(%v)=%v, want %v", tc.desc, testPod, resDNSConfig, tc.expectedDNSConfig)
			}
		})
	}
}

func dnsConfigsAreEqual(resConfig, expectedConfig *runtimeapi.DNSConfig) bool {
	if len(resConfig.Servers) != len(expectedConfig.Servers) ||
		len(resConfig.Searches) != len(expectedConfig.Searches) ||
		len(resConfig.Options) != len(expectedConfig.Options) {
		return false
	}
	for i, server := range resConfig.Servers {
		if expectedConfig.Servers[i] != server {
			return false
		}
	}
	for i, search := range resConfig.Searches {
		if expectedConfig.Searches[i] != search {
			return false
		}
	}
	// Options order may be changed after conversion.
	return sets.New[string](resConfig.Options...).Equal(sets.New[string](expectedConfig.Options...))
}

func newTestPods(count int) []*v1.Pod {
	pods := make([]*v1.Pod, count)
	for i := 0; i < count; i++ {
		pods[i] = &v1.Pod{
			Spec: v1.PodSpec{
				HostNetwork: true,
			},
			ObjectMeta: metav1.ObjectMeta{
				UID:  types.UID(strconv.Itoa(10000 + i)),
				Name: fmt.Sprintf("pod%d", i),
			},
		}
	}
	return pods
}

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

package token

import (
	"fmt"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/test/utils/ktesting"
	testingclock "k8s.io/utils/clock/testing"
)

func TestTokenCachingAndExpiration(t *testing.T) {
	type suite struct {
		clock *testingclock.FakeClock
		tg    *fakeTokenGetter
		mgr   *Manager
	}

	cases := []struct {
		name string
		exp  time.Duration
		f    func(t *testing.T, s *suite)
	}{
		{
			name: "rotate hour token expires in the last 12 minutes",
			exp:  time.Hour,
			f: func(t *testing.T, s *suite) {
				s.clock.SetTime(s.clock.Now().Add(50 * time.Minute))
				if _, err := s.mgr.GetServiceAccountToken("a", "b", getTokenRequest()); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if s.tg.count != 2 {
					t.Fatalf("expected token to be refreshed: call count was %d", s.tg.count)
				}
			},
		},
		{
			name: "rotate 24 hour token that expires in 40 hours",
			exp:  40 * time.Hour,
			f: func(t *testing.T, s *suite) {
				s.clock.SetTime(s.clock.Now().Add(25 * time.Hour))
				if _, err := s.mgr.GetServiceAccountToken("a", "b", getTokenRequest()); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if s.tg.count != 2 {
					t.Fatalf("expected token to be refreshed: call count was %d", s.tg.count)
				}
			},
		},
		{
			name: "rotate hour token fails, old token is still valid, doesn't error",
			exp:  time.Hour,
			f: func(t *testing.T, s *suite) {
				s.clock.SetTime(s.clock.Now().Add(50 * time.Minute))
				tg := &fakeTokenGetter{
					err: fmt.Errorf("err"),
				}
				s.mgr.getToken = tg.getToken
				tr, err := s.mgr.GetServiceAccountToken("a", "b", getTokenRequest())
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tr.Status.Token != "foo" {
					t.Fatalf("unexpected token: %v", tr.Status.Token)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := testingclock.NewFakeClock(time.Time{}.Add(30 * 24 * time.Hour))
			expSecs := int64(c.exp.Seconds())
			s := &suite{
				clock: clock,
				mgr:   NewManager(nil),
				tg: &fakeTokenGetter{
					tr: &authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							ExpirationSeconds: &expSecs,
						},
						Status: authenticationv1.TokenRequestStatus{
							Token:               "foo",
							ExpirationTimestamp: metav1.Time{Time: clock.Now().Add(c.exp)},
						},
					},
				},
			}
			s.mgr.getToken = s.tg.getToken
			s.mgr.clock = s.clock
			if _, err := s.mgr.GetServiceAccountToken("a", "b", getTokenRequest()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.tg.count != 1 {
				t.Fatalf("unexpected client call, got: %d, want: 1", s.tg.count)
			}

			if _, err := s.mgr.GetServiceAccountToken("a", "b", getTokenRequest()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.tg.count != 1 {
				t.Fatalf("expected token to be served from cache: saw %d", s.tg.count)
			}

			c.f(t, s)
		})
	}
}

func TestRequiresRefresh(t *testing.T) {
	tCtx := ktesting.Init(t)
	start := time.Now()
	cases := []struct {
		now, exp      time.Time
		expectRefresh bool
		requestTweaks func(*authenticationv1.TokenRequest)
	}{
		{
			now:           start.Add(10 * time.Minute),
			exp:           start.Add(60 * time.Minute),
			expectRefresh: false,
		},
		{
			now:           start.Add(50 * time.Minute),
			exp:           start.Add(60 * time.Minute),
			expectRefresh: true,
		},
		{
			now:           start.Add(25 * time.Hour),
			exp:           start.Add(60 * time.Hour),
			expectRefresh: true,
		},
		{
			now:           start.Add(70 * time.Minute),
			exp:           start.Add(60 * time.Minute),
			expectRefresh: true,
		},
		{
			// expiry will be overwritten by the tweak below.
			now:           start.Add(0 * time.Minute),
			exp:           start.Add(60 * time.Minute),
			expectRefresh: false,
			requestTweaks: func(tr *authenticationv1.TokenRequest) {
				tr.Spec.ExpirationSeconds = nil
			},
		},
	}

	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			clock := testingclock.NewFakeClock(c.now)
			secs := int64(c.exp.Sub(start).Seconds())
			tr := &authenticationv1.TokenRequest{
				Spec: authenticationv1.TokenRequestSpec{
					ExpirationSeconds: &secs,
				},
				Status: authenticationv1.TokenRequestStatus{
					ExpirationTimestamp: metav1.Time{Time: c.exp},
				},
			}

			if c.requestTweaks != nil {
				c.requestTweaks(tr)
			}

			mgr := NewManager(nil)
			mgr.clock = clock

			rr := mgr.requiresRefresh(tCtx, tr)
			if rr != c.expectRefresh {
				t.Fatalf("unexpected requiresRefresh result, got: %v, want: %v", rr, c.expectRefresh)
			}
		})
	}
}

func TestDeleteServiceAccountToken(t *testing.T) {
	type request struct {
		name, namespace string
		tr              authenticationv1.TokenRequest
		shouldFail      bool
	}

	cases := []struct {
		name         string
		requestIndex []int
		deletePodUID []types.UID
		expLeftIndex []int
	}{
		{
			name:         "delete none with all success requests",
			requestIndex: []int{0, 1, 2},
			expLeftIndex: []int{0, 1, 2},
		},
		{
			name:         "delete one with all success requests",
			requestIndex: []int{0, 1, 2},
			deletePodUID: []types.UID{"fake-uid-1"},
			expLeftIndex: []int{1, 2},
		},
		{
			name:         "delete two with all success requests",
			requestIndex: []int{0, 1, 2},
			deletePodUID: []types.UID{"fake-uid-1", "fake-uid-3"},
			expLeftIndex: []int{1},
		},
		{
			name:         "delete all with all success requests",
			requestIndex: []int{0, 1, 2},
			deletePodUID: []types.UID{"fake-uid-1", "fake-uid-2", "fake-uid-3"},
		},
		{
			name:         "delete no pod with failed requests",
			requestIndex: []int{0, 1, 2, 3},
			deletePodUID: []types.UID{},
			expLeftIndex: []int{0, 1, 2},
		},
		{
			name:         "delete other pod with failed requests",
			requestIndex: []int{0, 1, 2, 3},
			deletePodUID: []types.UID{"fake-uid-2"},
			expLeftIndex: []int{0, 2},
		},
		{
			name:         "delete no pod with request which success after failure",
			requestIndex: []int{0, 1, 2, 3, 4},
			deletePodUID: []types.UID{},
			expLeftIndex: []int{0, 1, 2, 4},
		},
		{
			name:         "delete the pod which success after failure",
			requestIndex: []int{0, 1, 2, 3, 4},
			deletePodUID: []types.UID{"fake-uid-4"},
			expLeftIndex: []int{0, 1, 2},
		},
		{
			name:         "delete other pod with request which success after failure",
			requestIndex: []int{0, 1, 2, 3, 4},
			deletePodUID: []types.UID{"fake-uid-1"},
			expLeftIndex: []int{1, 2, 4},
		},
		{
			name:         "delete some pod not in the set",
			requestIndex: []int{0, 1, 2},
			deletePodUID: []types.UID{"fake-uid-100", "fake-uid-200"},
			expLeftIndex: []int{0, 1, 2},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requests := []request{
				{
					name:      "fake-name-1",
					namespace: "fake-namespace-1",
					tr: authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								UID:  "fake-uid-1",
								Name: "fake-name-1",
							},
						},
					},
					shouldFail: false,
				},
				{
					name:      "fake-name-2",
					namespace: "fake-namespace-2",
					tr: authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								UID:  "fake-uid-2",
								Name: "fake-name-2",
							},
						},
					},
					shouldFail: false,
				},
				{
					name:      "fake-name-3",
					namespace: "fake-namespace-3",
					tr: authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								UID:  "fake-uid-3",
								Name: "fake-name-3",
							},
						},
					},
					shouldFail: false,
				},
				{
					name:      "fake-name-4",
					namespace: "fake-namespace-4",
					tr: authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								UID:  "fake-uid-4",
								Name: "fake-name-4",
							},
						},
					},
					shouldFail: true,
				},
				{
					//exactly the same with last one, besides it will success
					name:      "fake-name-4",
					namespace: "fake-namespace-4",
					tr: authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								UID:  "fake-uid-4",
								Name: "fake-name-4",
							},
						},
					},
					shouldFail: false,
				},
			}
			testMgr := NewManager(nil)
			testMgr.clock = testingclock.NewFakeClock(time.Time{}.Add(30 * 24 * time.Hour))

			successGetToken := func(_, _ string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
				tr.Status = authenticationv1.TokenRequestStatus{
					ExpirationTimestamp: metav1.Time{Time: testMgr.clock.Now().Add(10 * time.Hour)},
				}
				return tr, nil
			}
			failGetToken := func(_, _ string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
				return nil, fmt.Errorf("fail tr")
			}

			for _, index := range c.requestIndex {
				req := requests[index]
				if req.shouldFail {
					testMgr.getToken = failGetToken
				} else {
					testMgr.getToken = successGetToken
				}
				testMgr.GetServiceAccountToken(req.namespace, req.name, &req.tr)
			}

			for _, uid := range c.deletePodUID {
				testMgr.DeleteServiceAccountToken(uid)
			}
			if len(c.expLeftIndex) != len(testMgr.cache) {
				t.Errorf("%s got unexpected result: expected left cache size is %d, got %d", c.name, len(c.expLeftIndex), len(testMgr.cache))
			}
			for _, leftIndex := range c.expLeftIndex {
				r := requests[leftIndex]
				_, ok := testMgr.get(keyFunc(r.name, r.namespace, &r.tr))
				if !ok {
					t.Errorf("%s got unexpected result: expected token request %v exist in cache, but not", c.name, r)
				}
			}
		})
	}
}

type fakeTokenGetter struct {
	count int
	tr    *authenticationv1.TokenRequest
	err   error
}

func (ftg *fakeTokenGetter) getToken(name, namespace string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
	ftg.count++
	return ftg.tr, ftg.err
}

func TestCleanup(t *testing.T) {
	cases := []struct {
		name              string
		relativeExp       time.Duration
		expectedCacheSize int
	}{
		{
			name:              "don't cleanup unexpired tokens",
			relativeExp:       -1 * time.Hour,
			expectedCacheSize: 0,
		},
		{
			name:              "cleanup expired tokens",
			relativeExp:       time.Hour,
			expectedCacheSize: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := testingclock.NewFakeClock(time.Time{}.Add(24 * time.Hour))
			mgr := NewManager(nil)
			mgr.clock = clock

			mgr.set("key", &authenticationv1.TokenRequest{
				Status: authenticationv1.TokenRequestStatus{
					ExpirationTimestamp: metav1.Time{Time: mgr.clock.Now().Add(c.relativeExp)},
				},
			})
			mgr.cleanup()
			if got, want := len(mgr.cache), c.expectedCacheSize; got != want {
				t.Fatalf("unexpected number of cache entries after cleanup, got: %d, want: %d", got, want)
			}
		})
	}
}

func TestKeyFunc(t *testing.T) {
	type tokenRequestUnit struct {
		name      string
		namespace string
		tr        *authenticationv1.TokenRequest
	}
	getKeyFunc := func(u tokenRequestUnit) string {
		return keyFunc(u.name, u.namespace, u.tr)
	}

	cases := []struct {
		name   string
		trus   []tokenRequestUnit
		target tokenRequestUnit

		shouldHit bool
	}{
		{
			name: "hit",
			trus: []tokenRequestUnit{
				{
					name:      "foo-sa",
					namespace: "foo-ns",
					tr: &authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							Audiences:         []string{"foo1", "foo2"},
							ExpirationSeconds: getInt64Point(2000),
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								Kind: "pod",
								Name: "foo-pod",
								UID:  "foo-uid",
							},
						},
					},
				},
				{
					name:      "ame-sa",
					namespace: "ame-ns",
					tr: &authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							Audiences:         []string{"ame1", "ame2"},
							ExpirationSeconds: getInt64Point(2000),
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								Kind: "pod",
								Name: "ame-pod",
								UID:  "ame-uid",
							},
						},
					},
				},
			},
			target: tokenRequestUnit{
				name:      "foo-sa",
				namespace: "foo-ns",
				tr: &authenticationv1.TokenRequest{
					Spec: authenticationv1.TokenRequestSpec{
						Audiences:         []string{"foo1", "foo2"},
						ExpirationSeconds: getInt64Point(2000),
						BoundObjectRef: &authenticationv1.BoundObjectReference{
							Kind: "pod",
							Name: "foo-pod",
							UID:  "foo-uid",
						},
					},
				},
			},
			shouldHit: true,
		},
		{
			name: "not hit due to different ExpirationSeconds",
			trus: []tokenRequestUnit{
				{
					name:      "foo-sa",
					namespace: "foo-ns",
					tr: &authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							Audiences:         []string{"foo1", "foo2"},
							ExpirationSeconds: getInt64Point(2000),
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								Kind: "pod",
								Name: "foo-pod",
								UID:  "foo-uid",
							},
						},
					},
				},
			},
			target: tokenRequestUnit{
				name:      "foo-sa",
				namespace: "foo-ns",
				tr: &authenticationv1.TokenRequest{
					Spec: authenticationv1.TokenRequestSpec{
						Audiences: []string{"foo1", "foo2"},
						// everything is same besides ExpirationSeconds
						ExpirationSeconds: getInt64Point(2001),
						BoundObjectRef: &authenticationv1.BoundObjectReference{
							Kind: "pod",
							Name: "foo-pod",
							UID:  "foo-uid",
						},
					},
				},
			},
			shouldHit: false,
		},
		{
			name: "not hit due to different BoundObjectRef",
			trus: []tokenRequestUnit{
				{
					name:      "foo-sa",
					namespace: "foo-ns",
					tr: &authenticationv1.TokenRequest{
						Spec: authenticationv1.TokenRequestSpec{
							Audiences:         []string{"foo1", "foo2"},
							ExpirationSeconds: getInt64Point(2000),
							BoundObjectRef: &authenticationv1.BoundObjectReference{
								Kind: "pod",
								Name: "foo-pod",
								UID:  "foo-uid",
							},
						},
					},
				},
			},
			target: tokenRequestUnit{
				name:      "foo-sa",
				namespace: "foo-ns",
				tr: &authenticationv1.TokenRequest{
					Spec: authenticationv1.TokenRequestSpec{
						Audiences:         []string{"foo1", "foo2"},
						ExpirationSeconds: getInt64Point(2000),
						BoundObjectRef: &authenticationv1.BoundObjectReference{
							Kind: "pod",
							// everything is same besides BoundObjectRef.Name
							Name: "diff-pod",
							UID:  "foo-uid",
						},
					},
				},
			},
			shouldHit: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr := NewManager(nil)
			mgr.clock = testingclock.NewFakeClock(time.Time{}.Add(30 * 24 * time.Hour))
			for _, tru := range c.trus {
				mgr.set(getKeyFunc(tru), &authenticationv1.TokenRequest{
					Status: authenticationv1.TokenRequestStatus{
						// make sure the token cache would not be cleaned by token manager cleanup func
						ExpirationTimestamp: metav1.Time{Time: mgr.clock.Now().Add(50 * time.Minute)},
					},
				})
			}
			_, hit := mgr.get(getKeyFunc(c.target))

			if hit != c.shouldHit {
				t.Errorf("%s got unexpected hit result: expected to be %t, got %t", c.name, c.shouldHit, hit)
			}
		})
	}

}

func getTokenRequest() *authenticationv1.TokenRequest {
	return &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{"foo1", "foo2"},
			ExpirationSeconds: getInt64Point(2000),
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				Kind: "pod",
				Name: "foo-pod",
				UID:  "foo-uid",
			},
		},
	}
}

func getInt64Point(v int64) *int64 {
	return &v
}

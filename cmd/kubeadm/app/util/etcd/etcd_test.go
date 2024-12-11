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

package etcd

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/pkg/errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	"k8s.io/kubernetes/cmd/kubeadm/app/constants"
	testresources "k8s.io/kubernetes/cmd/kubeadm/test/resources"
)

var errNotImplemented = errors.New("not implemented")

type fakeEtcdClient struct {
	members   []*pb.Member
	endpoints []string
}

// Close shuts down the client's etcd connections.
func (f *fakeEtcdClient) Close() error {
	f.members = []*pb.Member{}
	return nil
}

// Endpoints lists the registered endpoints for the client.
func (f *fakeEtcdClient) Endpoints() []string {
	return f.endpoints
}

// MemberList lists the current cluster membership.
func (f *fakeEtcdClient) MemberList(_ context.Context, _ ...clientv3.OpOption) (*clientv3.MemberListResponse, error) {
	return &clientv3.MemberListResponse{
		Members: f.members,
	}, nil
}

// MemberAdd adds a new member into the cluster.
func (f *fakeEtcdClient) MemberAdd(_ context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error) {
	return nil, errNotImplemented
}

// MemberAddAsLearner adds a new learner member into the cluster.
func (f *fakeEtcdClient) MemberAddAsLearner(_ context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error) {
	return nil, errNotImplemented
}

// MemberRemove removes an existing member from the cluster.
func (f *fakeEtcdClient) MemberRemove(_ context.Context, id uint64) (*clientv3.MemberRemoveResponse, error) {
	return nil, errNotImplemented
}

// MemberPromote promotes a member from raft learner (non-voting) to raft voting member.
func (f *fakeEtcdClient) MemberPromote(_ context.Context, id uint64) (*clientv3.MemberPromoteResponse, error) {
	return nil, errNotImplemented
}

// Status gets the status of the endpoint.
func (f *fakeEtcdClient) Status(_ context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	return nil, errNotImplemented
}

// Sync synchronizes client's endpoints with the known endpoints from the etcd membership.
func (f *fakeEtcdClient) Sync(_ context.Context) error {
	return errNotImplemented
}

func testGetURL(t *testing.T, getURLFunc func(*kubeadmapi.APIEndpoint) string, port int) {
	portStr := strconv.Itoa(port)
	tests := []struct {
		name             string
		advertiseAddress string
		expectedURL      string
	}{
		{
			name:             "IPv4",
			advertiseAddress: "10.10.10.10",
			expectedURL:      fmt.Sprintf("https://10.10.10.10:%s", portStr),
		},
		{
			name:             "IPv6",
			advertiseAddress: "2001:db8::2",
			expectedURL:      fmt.Sprintf("https://[2001:db8::2]:%s", portStr),
		},
		{
			name:             "IPv4 localhost",
			advertiseAddress: "127.0.0.1",
			expectedURL:      fmt.Sprintf("https://127.0.0.1:%s", portStr),
		},
		{
			name:             "IPv6 localhost",
			advertiseAddress: "::1",
			expectedURL:      fmt.Sprintf("https://[::1]:%s", portStr),
		},
	}

	for _, test := range tests {
		url := getURLFunc(&kubeadmapi.APIEndpoint{AdvertiseAddress: test.advertiseAddress})
		if url != test.expectedURL {
			t.Errorf("expected %s, got %s", test.expectedURL, url)
		}
	}
}

func TestGetClientURL(t *testing.T) {
	testGetURL(t, GetClientURL, constants.EtcdListenClientPort)
}

func TestGetPeerURL(t *testing.T) {
	testGetURL(t, GetPeerURL, constants.EtcdListenPeerPort)
}

func TestGetClientURLByIP(t *testing.T) {
	portStr := strconv.Itoa(constants.EtcdListenClientPort)
	tests := []struct {
		name        string
		ip          string
		expectedURL string
	}{
		{
			name:        "IPv4",
			ip:          "10.10.10.10",
			expectedURL: fmt.Sprintf("https://10.10.10.10:%s", portStr),
		},
		{
			name:        "IPv6",
			ip:          "2001:db8::2",
			expectedURL: fmt.Sprintf("https://[2001:db8::2]:%s", portStr),
		},
		{
			name:        "IPv4 localhost",
			ip:          "127.0.0.1",
			expectedURL: fmt.Sprintf("https://127.0.0.1:%s", portStr),
		},
		{
			name:        "IPv6 localhost",
			ip:          "::1",
			expectedURL: fmt.Sprintf("https://[::1]:%s", portStr),
		},
	}

	for _, test := range tests {
		url := GetClientURLByIP(test.ip)
		if url != test.expectedURL {
			t.Errorf("expected %s, got %s", test.expectedURL, url)
		}
	}
}

func TestGetEtcdEndpointsWithBackoff(t *testing.T) {
	tests := []struct {
		name              string
		pods              []testresources.FakeStaticPod
		expectedEndpoints []string
		expectedErr       bool
	}{
		{
			name:              "no pod annotations",
			expectedEndpoints: []string{},
			expectedErr:       true,
		},
		{
			name: "ipv4 endpoint in pod annotation; port is preserved",
			pods: []testresources.FakeStaticPod{
				{
					Component: constants.Etcd,
					Annotations: map[string]string{
						constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:1234",
					},
				},
			},
			expectedEndpoints: []string{"https://1.2.3.4:1234"},
		},
	}
	for _, rt := range tests {
		t.Run(rt.name, func(t *testing.T) {
			client := clientsetfake.NewSimpleClientset()
			for _, pod := range rt.pods {
				if err := pod.Create(client); err != nil {
					t.Errorf("error setting up test creating pod for node %q", pod.NodeName)
				}
			}
			endpoints, err := getEtcdEndpointsWithRetry(client, time.Microsecond*10, time.Millisecond*100)
			if err != nil && !rt.expectedErr {
				t.Errorf("got error %q; was expecting no errors", err)
				return
			} else if err == nil && rt.expectedErr {
				t.Error("got no error; was expecting an error")
				return
			} else if err != nil && rt.expectedErr {
				return
			}

			if !reflect.DeepEqual(endpoints, rt.expectedEndpoints) {
				t.Errorf("expected etcd endpoints: %v; got: %v", rt.expectedEndpoints, endpoints)
			}
		})
	}
}

func TestGetRawEtcdEndpointsFromPodAnnotation(t *testing.T) {
	tests := []struct {
		name              string
		pods              []testresources.FakeStaticPod
		clientSetup       func(*clientsetfake.Clientset)
		expectedEndpoints []string
		expectedErr       bool
	}{
		{
			name: "exactly one pod with annotation",
			pods: []testresources.FakeStaticPod{
				{
					NodeName:    "cp-0",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:2379"},
				},
			},
			expectedEndpoints: []string{"https://1.2.3.4:2379"},
		},
		{
			name: "two pods; one is missing annotation",
			pods: []testresources.FakeStaticPod{
				{
					NodeName:    "cp-0",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:2379"},
				},
				{
					NodeName:  "cp-1",
					Component: constants.Etcd,
				},
			},
			expectedEndpoints: []string{"https://1.2.3.4:2379"},
			expectedErr:       true,
		},
		{
			name:        "no pods with annotation",
			expectedErr: true,
		},
		{
			name: "exactly one pod with annotation; all requests fail",
			pods: []testresources.FakeStaticPod{
				{
					NodeName:    "cp-0",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:2379"},
				},
			},
			clientSetup: func(clientset *clientsetfake.Clientset) {
				clientset.PrependReactor("list", "pods", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewInternalError(errors.New("API server down"))
				})
			},
			expectedErr: true,
		},
	}
	for _, rt := range tests {
		t.Run(rt.name, func(t *testing.T) {
			client := clientsetfake.NewSimpleClientset()
			for i, pod := range rt.pods {
				if err := pod.CreateWithPodSuffix(client, strconv.Itoa(i)); err != nil {
					t.Errorf("error setting up test creating pod for node %q", pod.NodeName)
				}
			}
			if rt.clientSetup != nil {
				rt.clientSetup(client)
			}
			endpoints, err := getRawEtcdEndpointsFromPodAnnotation(client, time.Microsecond*10, time.Millisecond*100)
			if err != nil && !rt.expectedErr {
				t.Errorf("got error %v, but wasn't expecting any error", err)
				return
			} else if err == nil && rt.expectedErr {
				t.Error("didn't get any error; but was expecting an error")
				return
			} else if err != nil && rt.expectedErr {
				return
			}
			if !reflect.DeepEqual(endpoints, rt.expectedEndpoints) {
				t.Errorf("expected etcd endpoints: %v; got: %v", rt.expectedEndpoints, endpoints)
			}
		})
	}
}

func TestGetRawEtcdEndpointsFromPodAnnotationWithoutRetry(t *testing.T) {
	tests := []struct {
		name              string
		pods              []testresources.FakeStaticPod
		clientSetup       func(*clientsetfake.Clientset)
		expectedEndpoints []string
		expectedErr       bool
	}{
		{
			name:              "no pods",
			expectedEndpoints: []string{},
		},
		{
			name: "exactly one pod with annotation",
			pods: []testresources.FakeStaticPod{
				{
					NodeName:    "cp-0",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:2379"},
				},
			},
			expectedEndpoints: []string{"https://1.2.3.4:2379"},
		},
		{
			name: "two pods; one is missing annotation",
			pods: []testresources.FakeStaticPod{
				{
					NodeName:    "cp-0",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:2379"},
				},
				{
					NodeName:  "cp-1",
					Component: constants.Etcd,
				},
			},
			expectedEndpoints: []string{"https://1.2.3.4:2379"},
		},
		{
			name: "two pods with annotation",
			pods: []testresources.FakeStaticPod{
				{
					NodeName:    "cp-0",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:2379"},
				},
				{
					NodeName:    "cp-1",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.5:2379"},
				},
			},
			expectedEndpoints: []string{"https://1.2.3.4:2379", "https://1.2.3.5:2379"},
		},
		{
			name: "exactly one pod with annotation; request fails",
			pods: []testresources.FakeStaticPod{
				{
					NodeName:    "cp-0",
					Component:   constants.Etcd,
					Annotations: map[string]string{constants.EtcdAdvertiseClientUrlsAnnotationKey: "https://1.2.3.4:2379"},
				},
			},
			clientSetup: func(clientset *clientsetfake.Clientset) {
				clientset.PrependReactor("list", "pods", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, apierrors.NewInternalError(errors.New("API server down"))
				})
			},
			expectedErr: true,
		},
	}
	for _, rt := range tests {
		t.Run(rt.name, func(t *testing.T) {
			client := clientsetfake.NewSimpleClientset()
			for _, pod := range rt.pods {
				if err := pod.Create(client); err != nil {
					t.Errorf("error setting up test creating pod for node %q", pod.NodeName)
					return
				}
			}
			if rt.clientSetup != nil {
				rt.clientSetup(client)
			}
			endpoints, _, err := getRawEtcdEndpointsFromPodAnnotationWithoutRetry(client)
			if err != nil && !rt.expectedErr {
				t.Errorf("got error %v, but wasn't expecting any error", err)
				return
			} else if err == nil && rt.expectedErr {
				t.Error("didn't get any error; but was expecting an error")
				return
			} else if err != nil && rt.expectedErr {
				return
			}
			if !reflect.DeepEqual(endpoints, rt.expectedEndpoints) {
				t.Errorf("expected etcd endpoints: %v; got: %v", rt.expectedEndpoints, endpoints)
			}
		})
	}
}

func TestClient_GetMemberID(t *testing.T) {
	type fields struct {
		Endpoints     []string
		newEtcdClient func(endpoints []string) (etcdClient, error)
	}
	type args struct {
		peerURL string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    uint64
		wantErr error
	}{
		{
			name: "member ID found",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								ID:   1,
								Name: "member1",
								PeerURLs: []string{
									"https://member1:2380",
								},
							},
						},
					}
					return f, nil
				},
			},
			args: args{
				peerURL: "https://member1:2380",
			},
			wantErr: nil,
			want:    1,
		},
		{
			name: "member ID not found",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								ID:   1,
								Name: "member1",
								PeerURLs: []string{
									"https://member1:2380",
								},
							},
						},
					}
					return f, nil
				},
			},
			args: args{
				peerURL: "https://member2:2380",
			},
			wantErr: ErrNoMemberIDForPeerURL,
			want:    0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{
				Endpoints:     tt.fields.Endpoints,
				newEtcdClient: tt.fields.newEtcdClient,
			}
			c.listMembersFunc = func(_ time.Duration) (*clientv3.MemberListResponse, error) {
				f, _ := c.newEtcdClient([]string{})
				resp, _ := f.MemberList(context.Background())
				return resp, nil
			}

			got, err := c.GetMemberID(tt.args.peerURL)
			if !errors.Is(tt.wantErr, err) {
				t.Errorf("Client.GetMemberID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("Client.GetMemberID() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListMembers(t *testing.T) {
	type fields struct {
		Endpoints       []string
		newEtcdClient   func(endpoints []string) (etcdClient, error)
		listMembersFunc func(timeout time.Duration) (*clientv3.MemberListResponse, error)
	}
	tests := []struct {
		name      string
		fields    fields
		want      []Member
		wantError bool
	}{
		{
			name: "PeerURLs are empty",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{}
					return f, nil
				},
			},
			want: []Member{},
		},
		{
			name: "PeerURLs are non-empty",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								ID:   1,
								Name: "member1",
								PeerURLs: []string{
									"https://member1:2380",
								},
							},
							{
								ID:   2,
								Name: "member2",
								PeerURLs: []string{
									"https://member2:2380",
								},
							},
						},
					}
					return f, nil
				},
			},
			want: []Member{
				{
					Name:    "member1",
					PeerURL: "https://member1:2380",
				},
				{
					Name:    "member2",
					PeerURL: "https://member2:2380",
				},
			},
		},
		{
			name: "PeerURLs has multiple urls",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								ID:   1,
								Name: "member1",
								PeerURLs: []string{
									"https://member1:2380",
									"https://member2:2380",
								},
							},
						},
					}
					return f, nil
				},
			},
			want: []Member{
				{
					Name:    "member1",
					PeerURL: "https://member1:2380",
				},
			},
		},
		{
			name: "ListMembers return error",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								ID:   1,
								Name: "member1",
								PeerURLs: []string{
									"https://member1:2380",
									"https://member2:2380",
								},
							},
						},
					}
					return f, nil
				},
				listMembersFunc: func(_ time.Duration) (*clientv3.MemberListResponse, error) {
					return nil, errNotImplemented
				},
			},
			want:      nil,
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{
				Endpoints:       tt.fields.Endpoints,
				newEtcdClient:   tt.fields.newEtcdClient,
				listMembersFunc: tt.fields.listMembersFunc,
			}
			if c.listMembersFunc == nil {
				c.listMembersFunc = func(_ time.Duration) (*clientv3.MemberListResponse, error) {
					return c.listMembers(100 * time.Millisecond)
				}
			}
			got, err := c.ListMembers()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ListMembers() = %v, want %v", got, tt.want)
			}
			if (err != nil) != (tt.wantError) {
				t.Errorf("ListMembers() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestIsLearner(t *testing.T) {
	type fields struct {
		Endpoints       []string
		newEtcdClient   func(endpoints []string) (etcdClient, error)
		listMembersFunc func(timeout time.Duration) (*clientv3.MemberListResponse, error)
	}
	tests := []struct {
		name      string
		fields    fields
		memberID  uint64
		want      bool
		wantError bool
	}{
		{
			name: "The specified member is not a learner",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								ID:   1,
								Name: "member1",
								PeerURLs: []string{
									"https://member1:2380",
								},
								IsLearner: false,
							},
						},
					}
					return f, nil
				},
			},
			memberID: 1,
			want:     false,
		},
		{
			name: "The specified member is a learner",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								ID:   1,
								Name: "member1",
								PeerURLs: []string{
									"https://member1:2380",
								},
								IsLearner: true,
							},
							{
								ID:   2,
								Name: "member2",
								PeerURLs: []string{
									"https://member2:2380",
								},
							},
						},
					}
					return f, nil
				},
			},
			memberID: 1,
			want:     true,
		},
		{
			name: "The specified member does not exist",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{},
					}
					return f, nil
				},
			},
			memberID: 3,
			want:     false,
		},
		{
			name: "Learner ID is empty",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								Name: "member2",
								PeerURLs: []string{
									"https://member2:2380",
								},
								IsLearner: true,
							},
						},
					}
					return f, nil
				},
			},
			want: true,
		},
		{
			name: "ListMembers returns an error",
			fields: fields{
				Endpoints: []string{},
				newEtcdClient: func(endpoints []string) (etcdClient, error) {
					f := &fakeEtcdClient{
						members: []*pb.Member{
							{
								Name: "member2",
								PeerURLs: []string{
									"https://member2:2380",
								},
								IsLearner: true,
							},
						},
					}
					return f, nil
				},
				listMembersFunc: func(_ time.Duration) (*clientv3.MemberListResponse, error) {
					return nil, errNotImplemented
				},
			},
			want:      false,
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{
				Endpoints:       tt.fields.Endpoints,
				newEtcdClient:   tt.fields.newEtcdClient,
				listMembersFunc: tt.fields.listMembersFunc,
			}
			if c.listMembersFunc == nil {
				c.listMembersFunc = func(t_ time.Duration) (*clientv3.MemberListResponse, error) {
					f, _ := c.newEtcdClient([]string{})
					resp, _ := f.MemberList(context.Background())
					return resp, nil
				}
			}
			got, err := c.isLearner(tt.memberID)
			if got != tt.want {
				t.Errorf("isLearner() = %v, want %v", got, tt.want)
			}
			if (err != nil) != (tt.wantError) {
				t.Errorf("isLearner() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

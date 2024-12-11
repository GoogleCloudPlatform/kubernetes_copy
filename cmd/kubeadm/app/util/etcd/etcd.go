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
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	"k8s.io/kubernetes/cmd/kubeadm/app/constants"
)

const etcdTimeout = 2 * time.Second

// ErrNoMemberIDForPeerURL is returned when it is not possible to obtain a member ID
// from a given peer URL
var ErrNoMemberIDForPeerURL = errors.New("no member id found for peer URL")

// ClusterInterrogator is an interface to get etcd cluster related information
type ClusterInterrogator interface {
	CheckClusterHealth() error
	WaitForClusterAvailable(retries int, retryInterval time.Duration) (bool, error)
	Sync() error
	ListMembers() ([]Member, error)
	AddMember(name string, peerAddrs string) ([]Member, error)
	AddMemberAsLearner(name string, peerAddrs string) ([]Member, error)
	MemberPromote(learnerID uint64) error
	GetMemberID(peerURL string) (uint64, error)
	RemoveMember(id uint64) ([]Member, error)
}

type etcdClient interface {
	// Close shuts down the client's etcd connections.
	Close() error

	// Endpoints lists the registered endpoints for the client.
	Endpoints() []string

	// MemberList lists the current cluster membership.
	MemberList(ctx context.Context, opts ...clientv3.OpOption) (*clientv3.MemberListResponse, error)

	// MemberAdd adds a new member into the cluster.
	MemberAdd(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error)

	// MemberAddAsLearner adds a new learner member into the cluster.
	MemberAddAsLearner(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error)

	// MemberRemove removes an existing member from the cluster.
	MemberRemove(ctx context.Context, id uint64) (*clientv3.MemberRemoveResponse, error)

	// MemberPromote promotes a member from raft learner (non-voting) to raft voting member.
	MemberPromote(ctx context.Context, id uint64) (*clientv3.MemberPromoteResponse, error)

	// Status gets the status of the endpoint.
	Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error)

	// Sync synchronizes client's endpoints with the known endpoints from the etcd membership.
	Sync(ctx context.Context) error
}

// Client provides connection parameters for an etcd cluster
type Client struct {
	Endpoints []string

	newEtcdClient func(endpoints []string) (etcdClient, error)

	listMembersFunc func(timeout time.Duration) (*clientv3.MemberListResponse, error)
}

// New creates a new EtcdCluster client
func New(endpoints []string, ca, cert, key string) (*Client, error) {
	client := Client{Endpoints: endpoints}

	var err error
	var tlsConfig *tls.Config
	if ca != "" || cert != "" || key != "" {
		tlsInfo := transport.TLSInfo{
			CertFile:      cert,
			KeyFile:       key,
			TrustedCAFile: ca,
		}
		tlsConfig, err = tlsInfo.ClientConfig()
		if err != nil {
			return nil, err
		}
	}

	client.newEtcdClient = func(endpoints []string) (etcdClient, error) {
		return clientv3.New(clientv3.Config{
			Endpoints:   endpoints,
			DialTimeout: etcdTimeout,
			DialOptions: []grpc.DialOption{
				grpc.WithBlock(), // block until the underlying connection is up
			},
			TLS: tlsConfig,
		})
	}

	client.listMembersFunc = client.listMembers

	return &client, nil
}

// NewFromCluster creates an etcd client for the etcd endpoints present in etcd member list. In order to compose this information,
// it will first discover at least one etcd endpoint to connect to. Once created, the client synchronizes client's endpoints with
// the known endpoints from the etcd membership API, since it is the authoritative source of truth for the list of available members.
func NewFromCluster(client clientset.Interface, certificatesDir string) (*Client, error) {
	// Discover at least one etcd endpoint to connect to by inspecting the existing etcd pods

	// Get the list of etcd endpoints
	endpoints, err := getEtcdEndpoints(client)
	if err != nil {
		return nil, err
	}
	klog.V(1).Infof("etcd endpoints read from pods: %s", strings.Join(endpoints, ","))

	// Creates an etcd client
	etcdClient, err := New(
		endpoints,
		filepath.Join(certificatesDir, constants.EtcdCACertName),
		filepath.Join(certificatesDir, constants.EtcdHealthcheckClientCertName),
		filepath.Join(certificatesDir, constants.EtcdHealthcheckClientKeyName),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "error creating etcd client for %v endpoints", endpoints)
	}

	// synchronizes client's endpoints with the known endpoints from the etcd membership.
	err = etcdClient.Sync()
	if err != nil {
		return nil, errors.Wrap(err, "error syncing endpoints with etcd")
	}
	klog.V(1).Infof("update etcd endpoints: %s", strings.Join(etcdClient.Endpoints, ","))

	return etcdClient, nil
}

// getEtcdEndpoints returns the list of etcd endpoints.
func getEtcdEndpoints(client clientset.Interface) ([]string, error) {
	return getEtcdEndpointsWithRetry(client,
		constants.KubernetesAPICallRetryInterval, kubeadmapi.GetActiveTimeouts().KubernetesAPICall.Duration)
}

func getEtcdEndpointsWithRetry(client clientset.Interface, interval, timeout time.Duration) ([]string, error) {
	return getRawEtcdEndpointsFromPodAnnotation(client, interval, timeout)
}

// getRawEtcdEndpointsFromPodAnnotation returns the list of endpoints as reported on etcd's pod annotations using the given backoff
func getRawEtcdEndpointsFromPodAnnotation(client clientset.Interface, interval, timeout time.Duration) ([]string, error) {
	etcdEndpoints := []string{}
	var lastErr error
	// Let's tolerate some unexpected transient failures from the API server or load balancers. Also, if
	// static pods were not yet mirrored into the API server we want to wait for this propagation.
	err := wait.PollUntilContextTimeout(context.Background(), interval, timeout, true,
		func(_ context.Context) (bool, error) {
			var overallEtcdPodCount int
			if etcdEndpoints, overallEtcdPodCount, lastErr = getRawEtcdEndpointsFromPodAnnotationWithoutRetry(client); lastErr != nil {
				return false, nil
			}
			if len(etcdEndpoints) == 0 || overallEtcdPodCount != len(etcdEndpoints) {
				klog.V(4).Infof("found a total of %d etcd pods and the following endpoints: %v; retrying",
					overallEtcdPodCount, etcdEndpoints)
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		const message = "could not retrieve the list of etcd endpoints"
		if lastErr != nil {
			return []string{}, errors.Wrap(lastErr, message)
		}
		return []string{}, errors.Wrap(err, message)
	}
	return etcdEndpoints, nil
}

// getRawEtcdEndpointsFromPodAnnotationWithoutRetry returns the list of etcd endpoints as reported by etcd Pod annotations,
// along with the number of global etcd pods. This allows for callers to tell the difference between "no endpoints found",
// and "no endpoints found and pods were listed", so they can skip retrying.
func getRawEtcdEndpointsFromPodAnnotationWithoutRetry(client clientset.Interface) ([]string, int, error) {
	klog.V(3).Infof("retrieving etcd endpoints from %q annotation in etcd Pods", constants.EtcdAdvertiseClientUrlsAnnotationKey)
	podList, err := client.CoreV1().Pods(metav1.NamespaceSystem).List(
		context.TODO(),
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("component=%s,tier=%s", constants.Etcd, constants.ControlPlaneTier),
		},
	)
	if err != nil {
		return []string{}, 0, err
	}
	etcdEndpoints := []string{}
	for _, pod := range podList.Items {
		podIsReady := false
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				podIsReady = true
				break
			}
		}
		if !podIsReady {
			klog.V(3).Infof("etcd pod %q is not ready", pod.ObjectMeta.Name)
		}
		etcdEndpoint, ok := pod.ObjectMeta.Annotations[constants.EtcdAdvertiseClientUrlsAnnotationKey]
		if !ok {
			klog.V(3).Infof("etcd Pod %q is missing the %q annotation; cannot infer etcd advertise client URL using the Pod annotation", pod.ObjectMeta.Name, constants.EtcdAdvertiseClientUrlsAnnotationKey)
			continue
		}
		etcdEndpoints = append(etcdEndpoints, etcdEndpoint)
	}
	return etcdEndpoints, len(podList.Items), nil
}

// Sync synchronizes client's endpoints with the known endpoints from the etcd membership.
func (c *Client) Sync() error {
	// Syncs the list of endpoints
	var cli etcdClient
	var lastError error
	err := wait.PollUntilContextTimeout(context.Background(), constants.EtcdAPICallRetryInterval, constants.EtcdAPICallTimeout,
		true, func(_ context.Context) (bool, error) {
			var err error
			cli, err = c.newEtcdClient(c.Endpoints)
			if err != nil {
				lastError = err
				return false, nil
			}
			defer func() { _ = cli.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
			err = cli.Sync(ctx)
			cancel()
			if err == nil {
				return true, nil
			}
			klog.V(5).Infof("Failed to sync etcd endpoints: %v", err)
			lastError = err
			return false, nil
		})
	if err != nil {
		return lastError
	}
	klog.V(1).Infof("etcd endpoints read from etcd: %s", strings.Join(cli.Endpoints(), ","))

	c.Endpoints = cli.Endpoints()
	return nil
}

// Member struct defines an etcd member; it is used for avoiding to spread github.com/coreos/etcd dependency
// across kubeadm codebase
type Member struct {
	Name    string
	PeerURL string
}

func (c *Client) listMembers(timeout time.Duration) (*clientv3.MemberListResponse, error) {
	// Gets the member list
	var lastError error
	var resp *clientv3.MemberListResponse
	if timeout == 0 {
		timeout = constants.EtcdAPICallTimeout
	}
	err := wait.PollUntilContextTimeout(context.Background(), constants.EtcdAPICallRetryInterval, timeout,
		true, func(_ context.Context) (bool, error) {
			cli, err := c.newEtcdClient(c.Endpoints)
			if err != nil {
				lastError = err
				return false, nil
			}
			defer func() { _ = cli.Close() }()

			ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
			resp, err = cli.MemberList(ctx)
			cancel()
			if err == nil {
				return true, nil
			}
			klog.V(5).Infof("Failed to get etcd member list: %v", err)
			lastError = err
			return false, nil
		})
	if err != nil {
		return nil, lastError
	}
	return resp, nil
}

// GetMemberID returns the member ID of the given peer URL
func (c *Client) GetMemberID(peerURL string) (uint64, error) {
	resp, err := c.listMembersFunc(0)
	if err != nil {
		return 0, err
	}

	for _, member := range resp.Members {
		if member.GetPeerURLs()[0] == peerURL {
			return member.GetID(), nil
		}
	}
	return 0, ErrNoMemberIDForPeerURL
}

// ListMembers returns the member list.
func (c *Client) ListMembers() ([]Member, error) {
	resp, err := c.listMembersFunc(0)
	if err != nil {
		return nil, err
	}

	ret := make([]Member, 0, len(resp.Members))
	for _, m := range resp.Members {
		ret = append(ret, Member{Name: m.Name, PeerURL: m.PeerURLs[0]})
	}
	return ret, nil
}

// RemoveMember notifies an etcd cluster to remove an existing member
func (c *Client) RemoveMember(id uint64) ([]Member, error) {
	cli, err := c.newEtcdClient(c.Endpoints)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()

	// Remove an existing member from the cluster
	var (
		lastError   error
		respMembers []*etcdserverpb.Member
	)
	err = wait.PollUntilContextTimeout(context.Background(), constants.EtcdAPICallRetryInterval, constants.EtcdAPICallTimeout,
		true, func(_ context.Context) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
			defer cancel()

			// List members and quickly return if the member does not exist.
			listResp, err := cli.MemberList(ctx)
			if err != nil {
				klog.V(5).Infof("Failed to check whether the member %s exists: %v", strconv.FormatUint(id, 16), err)
				lastError = err
				return false, nil
			}
			found := false
			for _, member := range listResp.Members {
				if member.GetID() == id {
					found = true
					break
				}
			}
			if !found {
				klog.V(5).Infof("Member %s was not found; assuming it was already removed", strconv.FormatUint(id, 16))
				respMembers = listResp.Members
				return true, nil
			}

			resp, err := cli.MemberRemove(ctx, id)
			if err == nil {
				respMembers = resp.Members
				return true, nil
			}
			if errors.Is(rpctypes.ErrMemberNotFound, err) {
				klog.V(5).Infof("Member was already removed, because member %s was not found", strconv.FormatUint(id, 16))
				listResp, err = cli.MemberList(ctx)
				if err == nil {
					respMembers = listResp.Members
					return true, nil
				}
			}
			klog.V(5).Infof("Failed to remove etcd member: %v", err)
			lastError = err
			return false, nil
		})
	if err != nil {
		return nil, lastError
	}

	// Returns the updated list of etcd members
	ret := []Member{}
	for _, m := range respMembers {
		ret = append(ret, Member{Name: m.Name, PeerURL: m.PeerURLs[0]})
	}

	return ret, nil
}

// AddMember adds a new member into the etcd cluster
func (c *Client) AddMember(name string, peerAddrs string) ([]Member, error) {
	return c.addMember(name, peerAddrs, false)
}

// AddMemberAsLearner adds a new learner member into the etcd cluster.
func (c *Client) AddMemberAsLearner(name string, peerAddrs string) ([]Member, error) {
	return c.addMember(name, peerAddrs, true)
}

// addMember notifies an existing etcd cluster that a new member is joining, and
// return the updated list of members. If the member has already been added to the
// cluster, this will return the existing list of etcd members.
func (c *Client) addMember(name string, peerAddrs string, isLearner bool) ([]Member, error) {
	// Parse the peer address, required to add the client URL later to the list
	// of endpoints for this client. Parsing as a first operation to make sure that
	// if this fails no member addition is performed on the etcd cluster.
	parsedPeerAddrs, err := url.Parse(peerAddrs)
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing peer address %s", peerAddrs)
	}

	cli, err := c.newEtcdClient(c.Endpoints)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()

	// Adds a new member to the cluster
	var (
		lastError   error
		respMembers []*etcdserverpb.Member
		resp        *clientv3.MemberAddResponse
	)
	err = wait.PollUntilContextTimeout(context.Background(), constants.EtcdAPICallRetryInterval, constants.EtcdAPICallTimeout,
		true, func(_ context.Context) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
			defer cancel()

			// List members and quickly return if the member already exists.
			listResp, err := cli.MemberList(ctx)
			if err != nil {
				klog.V(5).Infof("Failed to check whether the member %q exists: %v", peerAddrs, err)
				lastError = err
				return false, nil
			}
			found := false
			for _, member := range listResp.Members {
				if member.GetPeerURLs()[0] == peerAddrs {
					found = true
					break
				}
			}
			if found {
				klog.V(5).Infof("The peer URL %q for the added etcd member already exists. Skipping etcd member addition", peerAddrs)
				respMembers = listResp.Members
				return true, nil
			}

			if isLearner {
				klog.V(1).Infof("[etcd] Adding etcd member %q as learner", peerAddrs)
				resp, err = cli.MemberAddAsLearner(ctx, []string{peerAddrs})
			} else {
				klog.V(1).Infof("[etcd] Adding etcd member %q", peerAddrs)
				resp, err = cli.MemberAdd(ctx, []string{peerAddrs})
			}
			if err == nil {
				respMembers = resp.Members
				return true, nil
			}

			// If the error indicates that the peer already exists, exit early. In this situation, resp is nil, so
			// call out to MemberList to fetch all the members before returning.
			if errors.Is(err, rpctypes.ErrPeerURLExist) {
				klog.V(5).Info("The peer URL for the added etcd member already exists. Fetching the existing etcd members")
				listResp, err = cli.MemberList(ctx)
				if err == nil {
					respMembers = listResp.Members
					return true, nil
				}
			}

			klog.V(5).Infof("Failed to add etcd member: %v", err)
			lastError = err
			return false, nil
		})
	if err != nil {
		return nil, lastError
	}

	// Returns the updated list of etcd members
	ret := []Member{}
	for _, m := range respMembers {
		// If the peer address matches, this is the member we are adding.
		// Use the name we passed to the function.
		if peerAddrs == m.PeerURLs[0] {
			ret = append(ret, Member{Name: name, PeerURL: peerAddrs})
			continue
		}
		// Otherwise, we are processing other existing etcd members returned by AddMembers.
		memberName := m.Name
		// In some cases during concurrent join, some members can end up without a name.
		// Use the member ID as name for those.
		if len(memberName) == 0 {
			memberName = strconv.FormatUint(m.ID, 16)
		}
		ret = append(ret, Member{Name: memberName, PeerURL: m.PeerURLs[0]})
	}

	// Add the new member client address to the list of endpoints
	c.Endpoints = append(c.Endpoints, GetClientURLByIP(parsedPeerAddrs.Hostname()))

	return ret, nil
}

// isLearner returns true if the given member ID is a learner.
func (c *Client) isLearner(memberID uint64) (bool, error) {
	resp, err := c.listMembersFunc(0)
	if err != nil {
		return false, err
	}

	for _, member := range resp.Members {
		if member.ID == memberID && member.IsLearner {
			return true, nil
		}
	}
	return false, nil
}

// MemberPromote promotes a member as a voting member. If the given member ID is already a voting member this method
// will return early and do nothing.
func (c *Client) MemberPromote(learnerID uint64) error {
	isLearner, err := c.isLearner(learnerID)
	if err != nil {
		return err
	}
	if !isLearner {
		klog.V(1).Infof("[etcd] Member %s already promoted.", strconv.FormatUint(learnerID, 16))
		return nil
	}

	klog.V(1).Infof("[etcd] Promoting a learner as a voting member: %s", strconv.FormatUint(learnerID, 16))
	cli, err := c.newEtcdClient(c.Endpoints)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	// TODO: warning logs from etcd client should be removed.
	// The warning logs are printed by etcd client code for several reasons, including
	// 1. can not promote yet(no synced)
	// 2. context deadline exceeded
	// 3. peer URLs already exists
	// Once the client provides a way to check if the etcd learner is ready to promote, the retry logic can be revisited.
	var (
		lastError error
	)
	err = wait.PollUntilContextTimeout(context.Background(), constants.EtcdAPICallRetryInterval, constants.EtcdAPICallTimeout,
		true, func(_ context.Context) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
			defer cancel()

			_, err = cli.MemberPromote(ctx, learnerID)
			if err == nil {
				klog.V(1).Infof("[etcd] The learner was promoted as a voting member: %s", strconv.FormatUint(learnerID, 16))
				return true, nil
			}
			klog.V(5).Infof("[etcd] Promoting the learner %s failed: %v", strconv.FormatUint(learnerID, 16), err)
			lastError = err
			return false, nil
		})
	if err != nil {
		return lastError
	}
	return nil
}

// CheckClusterHealth returns nil for status Up or error for status Down
func (c *Client) CheckClusterHealth() error {
	_, err := c.getClusterStatus()
	return err
}

// getClusterStatus returns nil for status Up (along with endpoint status response map) or error for status Down
func (c *Client) getClusterStatus() (map[string]*clientv3.StatusResponse, error) {
	clusterStatus := make(map[string]*clientv3.StatusResponse)
	for _, ep := range c.Endpoints {
		// Gets the member status
		var lastError error
		var resp *clientv3.StatusResponse
		err := wait.PollUntilContextTimeout(context.Background(), constants.EtcdAPICallRetryInterval, constants.EtcdAPICallTimeout,
			true, func(_ context.Context) (bool, error) {
				cli, err := c.newEtcdClient(c.Endpoints)
				if err != nil {
					lastError = err
					return false, nil
				}
				defer func() { _ = cli.Close() }()

				ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
				resp, err = cli.Status(ctx, ep)
				cancel()
				if err == nil {
					return true, nil
				}
				klog.V(5).Infof("Failed to get etcd status for %s: %v", ep, err)
				lastError = err
				return false, nil
			})
		if err != nil {
			return nil, lastError
		}

		clusterStatus[ep] = resp
	}
	return clusterStatus, nil
}

// WaitForClusterAvailable returns true if all endpoints in the cluster are available after retry attempts, an error is returned otherwise
func (c *Client) WaitForClusterAvailable(retries int, retryInterval time.Duration) (bool, error) {
	for i := 0; i < retries; i++ {
		if i > 0 {
			klog.V(1).Infof("[etcd] Waiting %v until next retry\n", retryInterval)
			time.Sleep(retryInterval)
		}
		klog.V(2).Infof("[etcd] attempting to see if all cluster endpoints (%s) are available %d/%d", c.Endpoints, i+1, retries)
		_, err := c.getClusterStatus()
		if err != nil {
			switch err {
			case context.DeadlineExceeded:
				klog.V(1).Infof("[etcd] Attempt timed out")
			default:
				klog.V(1).Infof("[etcd] Attempt failed with error: %v\n", err)
			}
			continue
		}
		return true, nil
	}
	return false, errors.New("timeout waiting for etcd cluster to be available")
}

// GetClientURL creates an HTTPS URL that uses the configured advertise
// address and client port for the API controller
func GetClientURL(localEndpoint *kubeadmapi.APIEndpoint) string {
	return "https://" + net.JoinHostPort(localEndpoint.AdvertiseAddress, strconv.Itoa(constants.EtcdListenClientPort))
}

// GetPeerURL creates an HTTPS URL that uses the configured advertise
// address and peer port for the API controller
func GetPeerURL(localEndpoint *kubeadmapi.APIEndpoint) string {
	return "https://" + net.JoinHostPort(localEndpoint.AdvertiseAddress, strconv.Itoa(constants.EtcdListenPeerPort))
}

// GetClientURLByIP creates an HTTPS URL based on an IP address
// and the client listening port.
func GetClientURLByIP(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(constants.EtcdListenClientPort))
}

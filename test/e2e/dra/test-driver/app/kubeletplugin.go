/*
Copyright 2022 The Kubernetes Authors.

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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"google.golang.org/grpc"

	resourceapi "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	drapbv1alpha4 "k8s.io/kubelet/pkg/apis/dra/v1alpha4"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1beta1"
)

type ExamplePlugin struct {
	stopCh     <-chan struct{}
	logger     klog.Logger
	kubeClient kubernetes.Interface
	d          kubeletplugin.DRAPlugin
	fileOps    FileOperations

	cdiDir      string
	driverName  string
	nodeName    string
	deviceNames sets.Set[string]

	mutex     sync.Mutex
	prepared  map[ClaimID][]Device // prepared claims -> result of nodePrepareResource
	gRPCCalls []GRPCCall

	blockPrepareResourcesMutex   sync.Mutex
	blockUnprepareResourcesMutex sync.Mutex

	prepareResourcesFailure   error
	failPrepareResourcesMutex sync.Mutex

	unprepareResourcesFailure   error
	failUnprepareResourcesMutex sync.Mutex
}

type GRPCCall struct {
	// FullMethod is the fully qualified, e.g. /package.service/method.
	FullMethod string

	// Request contains the parameters of the call.
	Request interface{}

	// Response contains the reply of the plugin. It is nil for calls that are in progress.
	Response interface{}

	// Err contains the error return value of the plugin. It is nil for calls that are in progress or succeeded.
	Err error
}

// ClaimID contains both claim name and UID to simplify debugging. The
// namespace is not included because it is random in E2E tests and the UID is
// sufficient to make the ClaimID unique.
type ClaimID struct {
	Name string
	UID  string
}

type Device struct {
	PoolName    string
	DeviceName  string
	RequestName string
	CDIDeviceID string
}

var _ drapb.DRAPluginServer = &ExamplePlugin{}

// getJSONFilePath returns the absolute path where CDI file is/should be.
func (ex *ExamplePlugin) getJSONFilePath(claimUID string, requestName string) string {
	baseRequestRef := resourceclaim.BaseRequestRef(requestName)
	return filepath.Join(ex.cdiDir, fmt.Sprintf("%s-%s-%s.json", ex.driverName, claimUID, baseRequestRef))
}

// FileOperations defines optional callbacks for handling CDI files
// and some other configuration.
type FileOperations struct {
	// Create must overwrite the file.
	Create func(name string, content []byte) error

	// Remove must remove the file. It must not return an error when the
	// file does not exist.
	Remove func(name string) error

	// NumDevices determines whether the plugin reports devices
	// and how many. It reports nothing if negative.
	NumDevices int

	// Pre-defined devices, with each device name mapped to
	// the device attributes. Not used if NumDevices >= 0.
	Devices map[string]map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
}

// StartPlugin sets up the servers that are necessary for a DRA kubelet plugin.
func StartPlugin(ctx context.Context, cdiDir, driverName string, kubeClient kubernetes.Interface, nodeName string, fileOps FileOperations, opts ...kubeletplugin.Option) (*ExamplePlugin, error) {
	logger := klog.FromContext(ctx)

	if fileOps.Create == nil {
		fileOps.Create = func(name string, content []byte) error {
			return os.WriteFile(name, content, os.FileMode(0644))
		}
	}
	if fileOps.Remove == nil {
		fileOps.Remove = func(name string) error {
			if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
	}
	ex := &ExamplePlugin{
		stopCh:      ctx.Done(),
		logger:      logger,
		kubeClient:  kubeClient,
		fileOps:     fileOps,
		cdiDir:      cdiDir,
		driverName:  driverName,
		nodeName:    nodeName,
		prepared:    make(map[ClaimID][]Device),
		deviceNames: sets.New[string](),
	}

	for i := 0; i < ex.fileOps.NumDevices; i++ {
		ex.deviceNames.Insert(fmt.Sprintf("device-%02d", i))
	}
	for deviceName := range ex.fileOps.Devices {
		ex.deviceNames.Insert(deviceName)
	}
	opts = append(opts,
		kubeletplugin.DriverName(driverName),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.KubeClient(kubeClient),
		kubeletplugin.GRPCInterceptor(ex.recordGRPCCall),
		kubeletplugin.GRPCStreamInterceptor(ex.recordGRPCStream),
	)
	// Both APIs get provided, the legacy one via wrapping. The options
	// determine which one(s) really get served (by default, both).
	// The options are a bit redundant now because a single instance cannot
	// implement both, but that might be different in the future.
	nodeServers := []any{
		drapb.DRAPluginServer(ex), // Casting is done only for clarity here, it's not needed.
		drapbv1alpha4.V1Beta1ServerWrapper{DRAPluginServer: ex},
	}
	d, err := kubeletplugin.Start(ctx, nodeServers, opts...)
	if err != nil {
		return nil, fmt.Errorf("start kubelet plugin: %w", err)
	}
	ex.d = d

	if fileOps.NumDevices >= 0 {
		devices := make([]resourceapi.Device, ex.fileOps.NumDevices)
		for i := 0; i < ex.fileOps.NumDevices; i++ {
			devices[i] = resourceapi.Device{
				Name:  fmt.Sprintf("device-%02d", i),
				Basic: &resourceapi.BasicDevice{},
			}
		}
		driverResources := resourceslice.DriverResources{
			Pools: map[string]resourceslice.Pool{
				nodeName: {
					Slices: []resourceslice.Slice{{
						Devices: devices,
					}},
				},
			},
		}
		if err := ex.d.PublishResources(ctx, driverResources); err != nil {
			return nil, fmt.Errorf("start kubelet plugin: publish resources: %w", err)
		}
	} else if len(ex.fileOps.Devices) > 0 {
		devices := make([]resourceapi.Device, len(ex.fileOps.Devices))
		for i, deviceName := range sets.List(ex.deviceNames) {
			devices[i] = resourceapi.Device{
				Name:  deviceName,
				Basic: &resourceapi.BasicDevice{Attributes: ex.fileOps.Devices[deviceName]},
			}
		}
		driverResources := resourceslice.DriverResources{
			Pools: map[string]resourceslice.Pool{
				nodeName: {
					Slices: []resourceslice.Slice{{
						Devices: devices,
					}},
				},
			},
		}
		if err := ex.d.PublishResources(ctx, driverResources); err != nil {
			return nil, fmt.Errorf("start kubelet plugin: publish resources: %w", err)
		}
	}

	return ex, nil
}

// Stop ensures that all servers are stopped and resources freed.
func (ex *ExamplePlugin) Stop() {
	ex.d.Stop()
}

func (ex *ExamplePlugin) IsRegistered() bool {
	status := ex.d.RegistrationStatus()
	if status == nil {
		return false
	}
	return status.PluginRegistered
}

// BlockNodePrepareResources locks blockPrepareResourcesMutex and returns unlocking function for it
func (ex *ExamplePlugin) BlockNodePrepareResources() func() {
	ex.blockPrepareResourcesMutex.Lock()
	return func() {
		ex.blockPrepareResourcesMutex.Unlock()
	}
}

// BlockNodeUnprepareResources locks blockUnprepareResourcesMutex and returns unlocking function for it
func (ex *ExamplePlugin) BlockNodeUnprepareResources() func() {
	ex.blockUnprepareResourcesMutex.Lock()
	return func() {
		ex.blockUnprepareResourcesMutex.Unlock()
	}
}

// SetNodePrepareResourcesFailureMode sets the failure mode for NodePrepareResources call
// and returns a function to unset the failure mode
func (ex *ExamplePlugin) SetNodePrepareResourcesFailureMode() func() {
	ex.failPrepareResourcesMutex.Lock()
	ex.prepareResourcesFailure = errors.New("simulated PrepareResources failure")
	ex.failPrepareResourcesMutex.Unlock()

	return func() {
		ex.failPrepareResourcesMutex.Lock()
		ex.prepareResourcesFailure = nil
		ex.failPrepareResourcesMutex.Unlock()
	}
}

func (ex *ExamplePlugin) getPrepareResourcesFailure() error {
	ex.failPrepareResourcesMutex.Lock()
	defer ex.failPrepareResourcesMutex.Unlock()
	return ex.prepareResourcesFailure
}

// SetNodeUnprepareResourcesFailureMode sets the failure mode for NodeUnprepareResources call
// and returns a function to unset the failure mode
func (ex *ExamplePlugin) SetNodeUnprepareResourcesFailureMode() func() {
	ex.failUnprepareResourcesMutex.Lock()
	ex.unprepareResourcesFailure = errors.New("simulated UnprepareResources failure")
	ex.failUnprepareResourcesMutex.Unlock()

	return func() {
		ex.failUnprepareResourcesMutex.Lock()
		ex.unprepareResourcesFailure = nil
		ex.failUnprepareResourcesMutex.Unlock()
	}
}

func (ex *ExamplePlugin) getUnprepareResourcesFailure() error {
	ex.failUnprepareResourcesMutex.Lock()
	defer ex.failUnprepareResourcesMutex.Unlock()
	return ex.unprepareResourcesFailure
}

// NodePrepareResource ensures that the CDI file(s) (one per request) for the claim exists. It uses
// a deterministic name to simplify NodeUnprepareResource (no need to remember
// or discover the name) and idempotency (when called again, the file simply
// gets written again).
func (ex *ExamplePlugin) nodePrepareResource(ctx context.Context, claimReq *drapb.Claim) ([]Device, error) {
	logger := klog.FromContext(ctx)

	// The plugin must retrieve the claim itself to get it in the version
	// that it understands.
	claim, err := ex.kubeClient.ResourceV1beta1().ResourceClaims(claimReq.Namespace).Get(ctx, claimReq.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("retrieve claim %s/%s: %w", claimReq.Namespace, claimReq.Name, err)
	}
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim %s/%s not allocated", claimReq.Namespace, claimReq.Name)
	}
	if claim.UID != types.UID(claimReq.UID) {
		return nil, fmt.Errorf("claim %s/%s got replaced", claimReq.Namespace, claimReq.Name)
	}

	ex.mutex.Lock()
	defer ex.mutex.Unlock()
	ex.blockPrepareResourcesMutex.Lock()
	defer ex.blockPrepareResourcesMutex.Unlock()

	claimID := ClaimID{Name: claimReq.Name, UID: claimReq.UID}
	if result, ok := ex.prepared[claimID]; ok {
		// Idempotent call, nothing to do.
		return result, nil
	}

	var devices []Device
	for _, result := range claim.Status.Allocation.Devices.Results {
		// Only handle allocations for the current driver.
		if ex.driverName != result.Driver {
			continue
		}

		baseRequestName := resourceclaim.BaseRequestRef(result.Request)

		// The driver joins all env variables in the order in which
		// they appear in results (last one wins).
		configs := resourceclaim.ConfigForResult(claim.Status.Allocation.Devices.Config, result, ex.driverName)
		env := make(map[string]string)
		for i, config := range configs {
			if err := extractParameters(config.Opaque.Parameters, &env, config.Source == resourceapi.AllocationConfigSourceClass); err != nil {
				return nil, fmt.Errorf("parameters in config #%d: %w", i, err)
			}
		}

		// It also sets a claim_<claim name>_<request name>=true env variable.
		// This can be used to identify which devices where mapped into a container.
		claimReqName := "claim_" + claim.Name + "_" + baseRequestName
		claimReqName = regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(claimReqName, "_")
		env[claimReqName] = "true"

		deviceName := "claim-" + claimReq.UID + "-" + baseRequestName
		vendor := ex.driverName
		class := "test"
		cdiDeviceID := vendor + "/" + class + "=" + deviceName

		// CDI wants env variables as set of strings.
		envs := []string{}
		for key, val := range env {
			envs = append(envs, key+"="+val)
		}
		sort.Strings(envs)

		if len(envs) == 0 {
			// CDI does not support empty ContainerEdits. For example,
			// kubelet+crio then fail with:
			//    CDI device injection failed: unresolvable CDI devices ...
			//
			// Inject nothing instead, which is supported by DRA.
			continue
		}

		spec := &spec{
			Version: "0.3.0", // This has to be a version accepted by the runtimes.
			Kind:    vendor + "/" + class,
			// At least one device is required and its entry must have more
			// than just the name.
			Devices: []device{
				{
					Name: deviceName,
					ContainerEdits: containerEdits{
						Env: envs,
					},
				},
			},
		}
		filePath := ex.getJSONFilePath(claimReq.UID, baseRequestName)
		buffer, err := json.Marshal(spec)
		if err != nil {
			return nil, fmt.Errorf("marshal spec: %w", err)
		}
		if err := ex.fileOps.Create(filePath, buffer); err != nil {
			return nil, fmt.Errorf("failed to write CDI file: %w", err)
		}
		device := Device{
			PoolName:    result.Pool,
			DeviceName:  result.Device,
			RequestName: baseRequestName,
			CDIDeviceID: cdiDeviceID,
		}
		devices = append(devices, device)
	}

	logger.V(3).Info("CDI file(s) created", "devices", devices)
	ex.prepared[claimID] = devices
	return devices, nil
}

func extractParameters(parameters runtime.RawExtension, env *map[string]string, admin bool) error {
	if len(parameters.Raw) == 0 {
		return nil
	}
	kind := "user"
	if admin {
		kind = "admin"
	}
	var data map[string]string
	if err := json.Unmarshal(parameters.Raw, &data); err != nil {
		return fmt.Errorf("decoding %s parameters: %w", kind, err)
	}
	if len(data) > 0 && *env == nil {
		*env = make(map[string]string)
	}
	for key, value := range data {
		(*env)[kind+"_"+key] = value
	}
	return nil
}

func (ex *ExamplePlugin) NodePrepareResources(ctx context.Context, req *drapb.NodePrepareResourcesRequest) (*drapb.NodePrepareResourcesResponse, error) {
	resp := &drapb.NodePrepareResourcesResponse{
		Claims: make(map[string]*drapb.NodePrepareResourceResponse),
	}

	if failure := ex.getPrepareResourcesFailure(); failure != nil {
		return resp, failure
	}

	for _, claimReq := range req.Claims {
		devices, err := ex.nodePrepareResource(ctx, claimReq)
		if err != nil {
			resp.Claims[claimReq.UID] = &drapb.NodePrepareResourceResponse{
				Error: err.Error(),
			}
		} else {
			r := &drapb.NodePrepareResourceResponse{}
			for _, device := range devices {
				pbDevice := &drapb.Device{
					PoolName:     device.PoolName,
					DeviceName:   device.DeviceName,
					RequestNames: []string{device.RequestName},
					CDIDeviceIDs: []string{device.CDIDeviceID},
				}
				r.Devices = append(r.Devices, pbDevice)
			}
			resp.Claims[claimReq.UID] = r
		}
	}
	return resp, nil
}

// NodeUnprepareResource removes the CDI file created by
// NodePrepareResource. It's idempotent, therefore it is not an error when that
// file is already gone.
func (ex *ExamplePlugin) nodeUnprepareResource(ctx context.Context, claimReq *drapb.Claim) error {
	ex.blockUnprepareResourcesMutex.Lock()
	defer ex.blockUnprepareResourcesMutex.Unlock()

	logger := klog.FromContext(ctx)

	claimID := ClaimID{Name: claimReq.Name, UID: claimReq.UID}
	devices, ok := ex.prepared[claimID]
	if !ok {
		// Idempotent call, nothing to do.
		return nil
	}

	for _, device := range devices {
		filePath := ex.getJSONFilePath(claimReq.UID, device.RequestName)
		if err := ex.fileOps.Remove(filePath); err != nil {
			return fmt.Errorf("error removing CDI file: %w", err)
		}
		logger.V(3).Info("CDI file removed", "path", filePath)
	}

	delete(ex.prepared, claimID)

	return nil
}

func (ex *ExamplePlugin) NodeUnprepareResources(ctx context.Context, req *drapb.NodeUnprepareResourcesRequest) (*drapb.NodeUnprepareResourcesResponse, error) {
	resp := &drapb.NodeUnprepareResourcesResponse{
		Claims: make(map[string]*drapb.NodeUnprepareResourceResponse),
	}

	if failure := ex.getUnprepareResourcesFailure(); failure != nil {
		return resp, failure
	}

	for _, claimReq := range req.Claims {
		err := ex.nodeUnprepareResource(ctx, claimReq)
		if err != nil {
			resp.Claims[claimReq.UID] = &drapb.NodeUnprepareResourceResponse{
				Error: err.Error(),
			}
		} else {
			resp.Claims[claimReq.UID] = &drapb.NodeUnprepareResourceResponse{}
		}
	}
	return resp, nil
}

func (ex *ExamplePlugin) GetPreparedResources() []ClaimID {
	ex.mutex.Lock()
	defer ex.mutex.Unlock()
	var prepared []ClaimID
	for claimID := range ex.prepared {
		prepared = append(prepared, claimID)
	}
	return prepared
}

func (ex *ExamplePlugin) recordGRPCCall(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	call := GRPCCall{
		FullMethod: info.FullMethod,
		Request:    req,
	}
	ex.mutex.Lock()
	ex.gRPCCalls = append(ex.gRPCCalls, call)
	index := len(ex.gRPCCalls) - 1
	ex.mutex.Unlock()

	// We don't hold the mutex here to allow concurrent calls.
	call.Response, call.Err = handler(ctx, req)

	ex.mutex.Lock()
	ex.gRPCCalls[index] = call
	ex.mutex.Unlock()

	return call.Response, call.Err
}

func (ex *ExamplePlugin) recordGRPCStream(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	call := GRPCCall{
		FullMethod: info.FullMethod,
	}
	ex.mutex.Lock()
	ex.gRPCCalls = append(ex.gRPCCalls, call)
	index := len(ex.gRPCCalls) - 1
	ex.mutex.Unlock()

	// We don't hold the mutex here to allow concurrent calls.
	call.Err = handler(srv, stream)

	ex.mutex.Lock()
	ex.gRPCCalls[index] = call
	ex.mutex.Unlock()

	return call.Err
}

func (ex *ExamplePlugin) GetGRPCCalls() []GRPCCall {
	ex.mutex.Lock()
	defer ex.mutex.Unlock()

	// We must return a new slice, otherwise adding new calls would become
	// visible to the caller. We also need to copy the entries because
	// they get mutated by recordGRPCCall.
	calls := make([]GRPCCall, 0, len(ex.gRPCCalls))
	calls = append(calls, ex.gRPCCalls...)
	return calls
}

// CountCalls counts GRPC calls with the given method suffix.
func (ex *ExamplePlugin) CountCalls(methodSuffix string) int {
	count := 0
	for _, call := range ex.GetGRPCCalls() {
		if strings.HasSuffix(call.FullMethod, methodSuffix) {
			count += 1
		}
	}
	return count
}

func (ex *ExamplePlugin) UpdateStatus(ctx context.Context, resourceClaim *resourceapi.ResourceClaim) (*resourceapi.ResourceClaim, error) {
	return ex.kubeClient.ResourceV1beta1().ResourceClaims(resourceClaim.Namespace).UpdateStatus(ctx, resourceClaim, metav1.UpdateOptions{})
}

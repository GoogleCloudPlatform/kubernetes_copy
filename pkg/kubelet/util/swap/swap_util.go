/*
Copyright 2024 The Kubernetes Authors.

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

package swap

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	sysruntime "runtime"
	"strings"
	"sync"

	inuserns "github.com/moby/sys/userns"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/klog/v2"
	utilkernel "k8s.io/kubernetes/pkg/util/kernel"
	"k8s.io/mount-utils"
)

var (
	tmpfsNoswapOptionSupported        bool
	tmpfsNoswapOptionAvailabilityOnce sync.Once
	swapOn                            bool
	swapOnErr                         error
	swapOnOnce                        sync.Once
)

const TmpfsNoswapOption = "noswap"

func IsTmpfsNoswapOptionSupported(mounter mount.Interface, mountPath string) bool {
	isTmpfsNoswapOptionSupportedHelper := func() bool {
		if sysruntime.GOOS == "windows" {
			return false
		}

		if inuserns.RunningInUserNS() {
			// Turning off swap in unprivileged tmpfs mounts unsupported
			// https://github.com/torvalds/linux/blob/v6.8/mm/shmem.c#L4004-L4011
			// https://github.com/kubernetes/kubernetes/issues/125137
			klog.InfoS("Running under a user namespace - tmpfs noswap is not supported")
			return false
		}

		kernelVersion, err := utilkernel.GetVersion()
		if err != nil {
			klog.ErrorS(err, "cannot determine kernel version, unable to determine is tmpfs noswap is supported")
			return false
		}

		if kernelVersion.AtLeast(version.MustParseGeneric(utilkernel.TmpfsNoswapSupportKernelVersion)) {
			return true
		}

		if mountPath == "" {
			klog.ErrorS(errors.New("mount path is empty, falling back to /tmp"), "")
		}

		mountPath, err = os.MkdirTemp(mountPath, "tmpfs-noswap-test-")
		if err != nil {
			klog.InfoS("error creating dir to test if tmpfs noswap is enabled. Assuming not supported", "mount path", mountPath, "error", err)
			return false
		}

		defer func() {
			err = os.RemoveAll(mountPath)
			if err != nil {
				klog.ErrorS(err, "error removing test tmpfs dir", "mount path", mountPath)
			}
		}()

		err = mounter.MountSensitiveWithoutSystemd("tmpfs", mountPath, "tmpfs", []string{TmpfsNoswapOption}, nil)
		if err != nil {
			klog.InfoS("error mounting tmpfs with the noswap option. Assuming not supported", "error", err)
			return false
		}

		err = mounter.Unmount(mountPath)
		if err != nil {
			klog.ErrorS(err, "error unmounting test tmpfs dir", "mount path", mountPath)
		}

		return true
	}

	tmpfsNoswapOptionAvailabilityOnce.Do(func() {
		tmpfsNoswapOptionSupported = isTmpfsNoswapOptionSupportedHelper()
	})

	return tmpfsNoswapOptionSupported
}

// gets /proc/swaps's content as an input, returns true if swap is enabled.
func isSwapOnAccordingToProcSwaps(procSwapsContent []byte) bool {
	procSwapsContent = bytes.TrimSpace(procSwapsContent) // extra trailing \n
	procSwapsStr := string(procSwapsContent)
	procSwapsLines := strings.Split(procSwapsStr, "\n")

	// If there is more than one line (table headers) in /proc/swaps then swap is enabled
	isSwapOn := len(procSwapsLines) > 1
	if isSwapOn {
		klog.InfoS("Swap is on", "/proc/swaps contents", procSwapsStr)
	}

	return isSwapOn
}

// IsSwapOn detects whether swap in enabled on the system by inspecting
// /proc/swaps. If the file does not exist, an os.NotFound error will be returned.
// If running on windows, swap is assumed to always be false.
func IsSwapOn() (bool, error) {
	isSwapOnHelper := func() (bool, error) {
		if sysruntime.GOOS == "windows" {
			return false, nil
		}

		const swapFilePath = "/proc/swaps"
		procSwapsContent, err := os.ReadFile(swapFilePath)
		if err != nil {
			if os.IsNotExist(err) {
				klog.InfoS("File does not exist, assuming that swap is disabled", "path", swapFilePath)
				return false, nil
			}

			return false, err
		}

		return isSwapOnAccordingToProcSwaps(procSwapsContent), nil
	}

	swapOnOnce.Do(func() {
		swapOn, swapOnErr = isSwapOnHelper()
	})

	return swapOn, swapOnErr
}

// The swap limit is calculated as (<containerMemoryRequest>/<nodeTotalMemory>)*<totalPodsSwapAvailable>.
// For more info, please look at the following KEP: https://kep.k8s.io/2400
func CalcSwapForBurstablePods(containerMemoryRequest, nodeTotalMemory, totalPodsSwapAvailable int64) (int64, error) {
	if nodeTotalMemory <= 0 {
		return 0, fmt.Errorf("total node memory is 0")
	}
	if containerMemoryRequest > nodeTotalMemory {
		return 0, fmt.Errorf("container request %d is larger than total node memory %d", containerMemoryRequest, nodeTotalMemory)
	}

	containerMemoryProportion := float64(containerMemoryRequest) / float64(nodeTotalMemory)
	swapAllocation := containerMemoryProportion * float64(totalPodsSwapAvailable)

	return int64(swapAllocation), nil
}

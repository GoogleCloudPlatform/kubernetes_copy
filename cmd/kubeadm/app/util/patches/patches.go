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

package patches

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/pkg/errors"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"sigs.k8s.io/yaml"
)

// PatchTarget defines a target to be patched, such as a control-plane static Pod.
type PatchTarget struct {
	// Name must be the name of a known target. In the case of Kubernetes objects
	// this is likely to match the ObjectMeta.Name of a target.
	Name string

	// StrategicMergePatchObject is only used for strategic merge patches.
	// It represents the underlying object type that is patched - e.g. "v1.Pod"
	StrategicMergePatchObject interface{}

	// Data must contain the bytes that will be patched.
	Data []byte
}

// PatchManager defines an object that can apply patches.
type PatchManager struct {
	patchSets    []*PatchSet
	knownTargets []string
	output       io.Writer
}

// PatchSet defines a set of patches of a certain type that can patch a PatchTarget.
type PatchSet struct {
	targetName string
	patchType  types.PatchType
	patches    []string
}

// String() is used for unit-testing.
func (ps *PatchSet) String() string {
	return fmt.Sprintf(
		"{%q, %q, %#v}",
		ps.targetName,
		ps.patchType,
		ps.patches,
	)
}

const (
	// KubeletConfiguration defines the kubeletconfiguration patch target.
	KubeletConfiguration = "kubeletconfiguration"
	// CoreDNSDeployment defines the corednsdeployment patch target.
	CoreDNSDeployment = "corednsdeployment"
)

var (
	pathLock  = &sync.RWMutex{}
	pathCache = map[string]*PatchManager{}

	patchTypes = map[string]types.PatchType{
		"json":      types.JSONPatchType,
		"merge":     types.MergePatchType,
		"strategic": types.StrategicMergePatchType,
		"":          types.StrategicMergePatchType, // Default
	}
	patchTypeList    = []string{"json", "merge", "strategic"}
	patchTypesJoined = strings.Join(patchTypeList, "|")
	knownExtensions  = []string{"json", "yaml"}

	regExtension = regexp.MustCompile(`.+\.(` + strings.Join(knownExtensions, "|") + `)$`)

	knownTargets = []string{
		kubeadmconstants.Etcd,
		kubeadmconstants.KubeAPIServer,
		kubeadmconstants.KubeControllerManager,
		kubeadmconstants.KubeScheduler,
		KubeletConfiguration,
		CoreDNSDeployment,
	}
)

// KnownTargets returns the locally defined knownTargets.
func KnownTargets() []string {
	return knownTargets
}

// NewPatchManager creates a patch manager that can be used to apply patches to "knownTargets".
func NewPatchManager(patchSets []*PatchSet, knownTargets []string, output io.Writer) *PatchManager {
	return &PatchManager{
		patchSets:    patchSets,
		knownTargets: knownTargets,
		output:       output,
	}
}

// GetPatchManagerForPath creates a patch manager that can be used to apply patches to "knownTargets".
// "path" should contain patches that can be used to patch the "knownTargets".
// If "output" is non-nil, messages about actions performed by the manager would go on this io.Writer.
func GetPatchManagerForPath(path string, knownTargets []string, output io.Writer) (*PatchManager, error) {
	pathLock.RLock()
	if pm, known := pathCache[path]; known {
		pathLock.RUnlock()
		return pm, nil
	}
	pathLock.RUnlock()

	if output == nil {
		output = io.Discard
	}

	fmt.Fprintf(output, "[patches] Reading patches from path %q\n", path)

	// Get the files in the path.
	patchSets, patchFiles, ignoredFiles, err := getPatchSetsFromPath(path, knownTargets, output)
	if err != nil {
		return nil, err
	}

	if len(patchFiles) > 0 {
		fmt.Fprintf(output, "[patches] Found the following patch files: %v\n", patchFiles)
	}
	if len(ignoredFiles) > 0 {
		fmt.Fprintf(output, "[patches] Ignored the following files: %v\n", ignoredFiles)
	}

	pm := &PatchManager{
		patchSets:    patchSets,
		knownTargets: knownTargets,
		output:       output,
	}
	pathLock.Lock()
	pathCache[path] = pm
	pathLock.Unlock()

	return pm, nil
}

// ApplyPatchesToTarget takes a patch target and patches its "Data" using the patches
// stored in the patch manager. The resulted "Data" is always converted to JSON.
func (pm *PatchManager) ApplyPatchesToTarget(patchTarget *PatchTarget) error {
	var err error
	var patchedData []byte

	var found bool
	for _, pt := range pm.knownTargets {
		if pt == patchTarget.Name {
			found = true
			break
		}
	}
	if !found {
		return errors.Errorf("unknown patch target name %q, must be one of %v", patchTarget.Name, pm.knownTargets)
	}

	// Always convert the target data to JSON.
	patchedData, err = yaml.YAMLToJSON(patchTarget.Data)
	if err != nil {
		return err
	}

	// Iterate over the patchSets.
	for _, patchSet := range pm.patchSets {
		if patchSet.targetName != patchTarget.Name {
			continue
		}

		// Iterate over the patches in the patchSets.
		for _, patch := range patchSet.patches {
			patchBytes := []byte(patch)

			// Patch based on the patch type.
			switch patchSet.patchType {

			// JSON patch.
			case types.JSONPatchType:
				var patchObj jsonpatch.Patch
				patchObj, err = jsonpatch.DecodePatch(patchBytes)
				if err == nil {
					patchedData, err = patchObj.Apply(patchedData)
				}

			// Merge patch.
			case types.MergePatchType:
				patchedData, err = jsonpatch.MergePatch(patchedData, patchBytes)

			// Strategic merge patch.
			case types.StrategicMergePatchType:
				patchedData, err = strategicpatch.StrategicMergePatch(
					patchedData,
					patchBytes,
					patchTarget.StrategicMergePatchObject,
				)
			}

			if err != nil {
				return errors.Wrapf(err, "could not apply the following patch of type %q to target %q:\n%s\n",
					patchSet.patchType,
					patchTarget.Name,
					patch)
			}
			fmt.Fprintf(pm.output, "[patches] Applied patch of type %q to target %q\n", patchSet.patchType, patchTarget.Name)
		}

		// Update the data for this patch target.
		patchTarget.Data = patchedData
	}

	return nil
}

// parseFilename validates a file name and retrieves the encoded target name and patch type.
// - On unknown extension or target name it returns a warning
// - On unknown patch type it returns an error
// - On success it returns a target name and patch type
func parseFilename(fileName string, knownTargets []string) (string, types.PatchType, error, error) {
	// Return a warning if the extension cannot be matched.
	if !regExtension.MatchString(fileName) {
		return "", "", errors.Errorf("the file extension must be one of %v", knownExtensions), nil
	}

	regFileNameSplit := regexp.MustCompile(
		fmt.Sprintf(`^(%s)([^.+\n]*)?(\+)?(%s)?`, strings.Join(knownTargets, "|"), patchTypesJoined),
	)
	// Extract the target name and patch type. The resulting sub-string slice would look like this:
	//   [full-match, targetName, suffix, +, patchType]
	sub := regFileNameSplit.FindStringSubmatch(fileName)
	if sub == nil {
		return "", "", errors.Errorf("unknown target, must be one of %v", knownTargets), nil
	}
	targetName := sub[1]

	if len(sub[3]) > 0 && len(sub[4]) == 0 {
		return "", "", nil, errors.Errorf("unknown or missing patch type after '+', must be one of %v", patchTypeList)
	}
	patchType := patchTypes[sub[4]]

	return targetName, patchType, nil, nil
}

// CreatePatchSet creates a patchSet object, by splitting the given "data" by "\n---".
func CreatePatchSet(targetName string, patchType types.PatchType, data string) (*PatchSet, error) {
	var patches []string

	// Split the patches and convert them to JSON.
	// Data that is already JSON will not cause an error.
	buf := bytes.NewBuffer([]byte(data))
	reader := utilyaml.NewYAMLReader(bufio.NewReader(buf))
	for {
		patch, err := reader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, errors.Wrapf(err, "could not split patches for data:\n%s\n", data)
		}

		patch = bytes.TrimSpace(patch)
		if len(patch) == 0 {
			continue
		}

		patchJSON, err := yaml.YAMLToJSON(patch)
		if err != nil {
			return nil, errors.Wrapf(err, "could not convert patch to JSON:\n%s\n", patch)
		}
		patches = append(patches, string(patchJSON))
	}

	return &PatchSet{
		targetName: targetName,
		patchType:  patchType,
		patches:    patches,
	}, nil
}

// getPatchSetsFromPath walks a path, ignores sub-directories and non-patch files, and
// returns a list of patchFile objects.
func getPatchSetsFromPath(targetPath string, knownTargets []string, output io.Writer) ([]*PatchSet, []string, []string, error) {
	patchFiles := []string{}
	ignoredFiles := []string{}
	patchSets := []*PatchSet{}

	// Check if targetPath is a directory.
	info, err := os.Lstat(targetPath)
	if err != nil {
		goto return_path_error
	}
	if !info.IsDir() {
		err = &os.PathError{
			Op:   "getPatchSetsFromPath",
			Path: info.Name(),
			Err:  errors.New("not a directory"),
		}
		goto return_path_error
	}

	err = filepath.WalkDir(targetPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Sub-directories and "." are ignored.
		if d.IsDir() {
			return nil
		}

		baseName := d.Name()

		// Parse the filename and retrieve the target and patch type
		targetName, patchType, warn, err := parseFilename(baseName, knownTargets)
		if err != nil {
			return err
		}
		if warn != nil {
			fmt.Fprintf(output, "[patches] Ignoring file %q: %v\n", baseName, warn)
			ignoredFiles = append(ignoredFiles, baseName)
			return nil
		}

		// Read the patch file.
		data, err := os.ReadFile(path)
		if err != nil {
			return errors.Wrapf(err, "could not read the file %q", path)
		}

		if len(data) == 0 {
			fmt.Fprintf(output, "[patches] Ignoring empty file: %q\n", baseName)
			ignoredFiles = append(ignoredFiles, baseName)
			return nil
		}

		// Create a patchSet object.
		patchSet, err := CreatePatchSet(targetName, patchType, string(data))
		if err != nil {
			return err
		}

		patchFiles = append(patchFiles, baseName)
		patchSets = append(patchSets, patchSet)
		return nil
	})

return_path_error:
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "could not list patch files for path %q", targetPath)
	}

	return patchSets, patchFiles, ignoredFiles, nil
}

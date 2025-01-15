/*
Copyright 2015 The Kubernetes Authors.

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

package downwardapi

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	utiltesting "k8s.io/client-go/util/testing"
	"k8s.io/kubernetes/pkg/fieldpath"
	filesystem "k8s.io/kubernetes/pkg/util/filesystem"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/emptydir"
	volumetest "k8s.io/kubernetes/pkg/volume/testing"
)

const (
	downwardAPIDir = "..data"
	testPodUID     = types.UID("test_pod_uid")
	testNamespace  = "test_metadata_namespace"
	testName       = "test_metadata_name"
)

func newTestHost(t *testing.T, clientset clientset.Interface) (string, volume.VolumeHost) {
	tempDir, err := utiltesting.MkTmpdir("downwardApi_volume_test.")
	if err != nil {
		t.Fatalf("can't make a temp rootdir: %v", err)
	}
	return tempDir, volumetest.NewFakeVolumeHost(t, tempDir, clientset, emptydir.ProbeVolumePlugins())
}

func TestCanSupport(t *testing.T) {
	pluginMgr := volume.VolumePluginMgr{}
	tmpDir, host := newTestHost(t, nil)
	defer os.RemoveAll(tmpDir)
	pluginMgr.InitPlugins(ProbeVolumePlugins(), nil /* prober */, host)

	plugin, err := pluginMgr.FindPluginByName(downwardAPIPluginName)
	if err != nil {
		t.Fatal("Can't find the plugin by name")
	}
	if plugin.GetPluginName() != downwardAPIPluginName {
		t.Errorf("Wrong name: %s", plugin.GetPluginName())
	}
	if !plugin.CanSupport(&volume.Spec{Volume: &v1.Volume{VolumeSource: v1.VolumeSource{DownwardAPI: &v1.DownwardAPIVolumeSource{}}}}) {
		t.Errorf("Expected true")
	}
	if plugin.CanSupport(&volume.Spec{Volume: &v1.Volume{VolumeSource: v1.VolumeSource{}}}) {
		t.Errorf("Expected false")
	}
}

func TestDownwardAPI(t *testing.T) {
	// Skip tests that fail on Windows, as discussed during the SIG Testing meeting from January 10, 2023
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test that fails on Windows")
	}

	labels1 := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}
	labels2 := map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}
	annotations := map[string]string{
		"a1":        "value1",
		"a2":        "value2",
		"multiline": "c\nb\na",
	}
	testCases := []struct {
		name           string
		files          map[string]string
		modes          map[string]int32
		podLabels      map[string]string
		podAnnotations map[string]string
		steps          []testStep
	}{
		{
			name:      "test_labels",
			files:     map[string]string{"labels": "metadata.labels"},
			podLabels: labels1,
			steps: []testStep{
				// for steps that involve files, stepName is also
				// used as the name of the file to verify
				verifyMapInFile{stepName{"labels"}, labels1},
			},
		},
		{
			name:           "test_annotations",
			files:          map[string]string{"annotations": "metadata.annotations"},
			podAnnotations: annotations,
			steps: []testStep{
				verifyMapInFile{stepName{"annotations"}, annotations},
			},
		},
		{
			name:  "test_name",
			files: map[string]string{"name_file_name": "metadata.name"},
			steps: []testStep{
				verifyLinesInFile{stepName{"name_file_name"}, testName},
			},
		},
		{
			name:  "test_namespace",
			files: map[string]string{"namespace_file_name": "metadata.namespace"},
			steps: []testStep{
				verifyLinesInFile{stepName{"namespace_file_name"}, testNamespace},
			},
		},
		{
			name:      "test_write_twice_no_update",
			files:     map[string]string{"labels": "metadata.labels"},
			podLabels: labels1,
			steps: []testStep{
				reSetUp{stepName{"resetup"}, false, nil},
				verifyMapInFile{stepName{"labels"}, labels1},
			},
		},
		{
			name:      "test_write_twice_with_update",
			files:     map[string]string{"labels": "metadata.labels"},
			podLabels: labels1,
			steps: []testStep{
				reSetUp{stepName{"resetup"}, true, labels2},
				verifyMapInFile{stepName{"labels"}, labels2},
			},
		},
		{
			name: "test_write_with_unix_path",
			files: map[string]string{
				"these/are/my/labels":        "metadata.labels",
				"these/are/your/annotations": "metadata.annotations",
			},
			podLabels:      labels1,
			podAnnotations: annotations,
			steps: []testStep{
				verifyMapInFile{stepName{"these/are/my/labels"}, labels1},
				verifyMapInFile{stepName{"these/are/your/annotations"}, annotations},
			},
		},
		{
			name:      "test_write_with_two_consecutive_slashes_in_the_path",
			files:     map[string]string{"this//labels": "metadata.labels"},
			podLabels: labels1,
			steps: []testStep{
				verifyMapInFile{stepName{"this/labels"}, labels1},
			},
		},
		{
			name:  "test_default_mode",
			files: map[string]string{"name_file_name": "metadata.name"},
			steps: []testStep{
				verifyMode{stepName{"name_file_name"}, 0644},
			},
		},
		{
			name:  "test_item_mode",
			files: map[string]string{"name_file_name": "metadata.name"},
			modes: map[string]int32{"name_file_name": 0600},
			steps: []testStep{
				verifyMode{stepName{"name_file_name"}, 0600},
			},
		},
	}
	for _, testCase := range testCases {
		test := newDownwardAPITest(t, testCase.name, testCase.files, testCase.podLabels, testCase.podAnnotations, testCase.modes)
		for _, step := range testCase.steps {
			test.t.Logf("Test case: %q Step: %q", testCase.name, step.getName())
			step.run(test)
		}
		test.tearDown()
	}
}

type downwardAPITest struct {
	t          *testing.T
	name       string
	plugin     volume.VolumePlugin
	pod        *v1.Pod
	mounter    volume.Mounter
	volumePath string
	rootDir    string
}

func newDownwardAPITest(t *testing.T, name string, volumeFiles, podLabels, podAnnotations map[string]string, modes map[string]int32) *downwardAPITest {
	defaultMode := int32(0644)
	var files []v1.DownwardAPIVolumeFile
	for path, fieldPath := range volumeFiles {
		file := v1.DownwardAPIVolumeFile{
			Path: path,
			FieldRef: &v1.ObjectFieldSelector{
				FieldPath: fieldPath,
			},
		}
		if mode, found := modes[path]; found {
			file.Mode = &mode
		}
		files = append(files, file)
	}
	podMeta := metav1.ObjectMeta{
		Name:        testName,
		Namespace:   testNamespace,
		Labels:      podLabels,
		Annotations: podAnnotations,
	}
	clientset := fake.NewSimpleClientset(&v1.Pod{ObjectMeta: podMeta})

	pluginMgr := volume.VolumePluginMgr{}
	rootDir, host := newTestHost(t, clientset)
	pluginMgr.InitPlugins(ProbeVolumePlugins(), nil /* prober */, host)
	plugin, err := pluginMgr.FindPluginByName(downwardAPIPluginName)
	if err != nil {
		t.Fatal("Can't find the plugin by name")
	}

	volumeSpec := &v1.Volume{
		Name: name,
		VolumeSource: v1.VolumeSource{
			DownwardAPI: &v1.DownwardAPIVolumeSource{
				DefaultMode: &defaultMode,
				Items:       files,
			},
		},
	}
	podMeta.UID = testPodUID
	pod := &v1.Pod{ObjectMeta: podMeta}
	mounter, err := plugin.NewMounter(volume.NewSpecFromVolume(volumeSpec), pod)
	if err != nil {
		t.Errorf("Failed to make a new Mounter: %v", err)
	}
	if mounter == nil {
		t.Fatalf("Got a nil Mounter")
	}

	volumePath := mounter.GetPath()

	err = mounter.SetUp(volume.MounterArgs{})
	if err != nil {
		t.Errorf("Failed to setup volume: %v", err)
	}

	// downwardAPI volume should create its own empty wrapper path
	podWrapperMetadataDir := fmt.Sprintf("%v/pods/%v/plugins/kubernetes.io~empty-dir/wrapped_%v", rootDir, testPodUID, name)

	if _, err := os.Stat(podWrapperMetadataDir); err != nil {
		if os.IsNotExist(err) {
			t.Errorf("SetUp() failed, empty-dir wrapper path was not created: %s", podWrapperMetadataDir)
		} else {
			t.Errorf("SetUp() failed: %v", err)
		}
	}

	return &downwardAPITest{
		t:          t,
		plugin:     plugin,
		pod:        pod,
		mounter:    mounter,
		volumePath: volumePath,
		rootDir:    rootDir,
	}
}

func (test *downwardAPITest) tearDown() {
	unmounter, err := test.plugin.NewUnmounter(test.name, testPodUID)
	if err != nil {
		test.t.Errorf("Failed to make a new Unmounter: %v", err)
	}
	if unmounter == nil {
		test.t.Fatalf("Got a nil Unmounter")
	}

	if err := unmounter.TearDown(); err != nil {
		test.t.Errorf("Expected success, got: %v", err)
	}
	if _, err := os.Stat(test.volumePath); err == nil {
		test.t.Errorf("TearDown() failed, volume path still exists: %s", test.volumePath)
	} else if !os.IsNotExist(err) {
		test.t.Errorf("TearDown() failed: %v", err)
	}
	os.RemoveAll(test.rootDir)
}

// testStep represents a named step of downwardAPITest.
// For steps that deal with files, step name also serves
// as the name of the file that's used by the step.
type testStep interface {
	getName() string
	run(*downwardAPITest)
}

type stepName struct {
	name string
}

func (step stepName) getName() string { return step.name }

func doVerifyLinesInFile(t *testing.T, volumePath, filename string, expected string) {
	data, err := os.ReadFile(filepath.Join(volumePath, filename))
	if err != nil {
		t.Error(err.Error())
		return
	}
	actualStr := string(data)
	expectedStr := expected
	if actualStr != expectedStr {
		t.Errorf("Found `%s`, expected `%s`", actualStr, expectedStr)
	}
}

type verifyLinesInFile struct {
	stepName
	expected string
}

func (step verifyLinesInFile) run(test *downwardAPITest) {
	doVerifyLinesInFile(test.t, test.volumePath, step.name, step.expected)
}

type verifyMapInFile struct {
	stepName
	expected map[string]string
}

func (step verifyMapInFile) run(test *downwardAPITest) {
	doVerifyLinesInFile(test.t, test.volumePath, step.name, fieldpath.FormatMap(step.expected))
}

type verifyMode struct {
	stepName
	expectedMode int32
}

func (step verifyMode) run(test *downwardAPITest) {
	p := filepath.Join(test.volumePath, step.name)

	// For the Windows case, we should evaluate the file mode of the target file, rather
	// that the symlink's.
	p, err := filepath.EvalSymlinks(p)
	if err != nil {
		test.t.Error(err.Error())
		return
	}

	actualMode, err := filesystem.GetFileMode(p)
	if err != nil {
		test.t.Errorf(err.Error())
		return
	}
	expectedMode := os.FileMode(step.expectedMode)
	if actualMode != expectedMode {
		test.t.Errorf("Found mode `%v` expected %v", actualMode, expectedMode)
	}
}

type reSetUp struct {
	stepName
	linkShouldChange bool
	newLabels        map[string]string
}

func (step reSetUp) run(test *downwardAPITest) {
	if step.newLabels != nil {
		test.pod.ObjectMeta.Labels = step.newLabels
	}

	currentTarget, err := os.Readlink(filepath.Join(test.volumePath, downwardAPIDir))
	if err != nil {
		test.t.Errorf("labels file should be a link... %s\n", err.Error())
	}

	// now re-run Setup
	if err = test.mounter.SetUp(volume.MounterArgs{}); err != nil {
		test.t.Errorf("Failed to re-setup volume: %v", err)
	}

	// get the link of the link
	currentTarget2, err := os.Readlink(filepath.Join(test.volumePath, downwardAPIDir))
	if err != nil {
		test.t.Errorf(".current should be a link... %s\n", err.Error())
	}

	switch {
	case step.linkShouldChange && currentTarget2 == currentTarget:
		test.t.Errorf("Got and update between the two Setup... Target link should NOT be the same\n")
	case !step.linkShouldChange && currentTarget2 != currentTarget:
		test.t.Errorf("No update between the two Setup... Target link should be the same %s %s\n",
			currentTarget, currentTarget2)
	}
}

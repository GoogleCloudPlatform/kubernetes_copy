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

package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	kubeletconfigv1alpha1 "k8s.io/kubelet/config/v1alpha1"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletconfigvint1alpha1 "k8s.io/kubernetes/pkg/kubelet/apis/config/v1alpha1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/util/parsers"
)

var _ ImagePullManager = &FileBasedImagePullManager{}

// namedLockSet stores named locks in order to allow to partition context access
// such that callers that are mutually exclusive based on a string value can
// access the same context at the same time, compared to a global lock that
// would create an unnecessary bottleneck.
type namedLockSet struct {
	globalLock sync.Mutex
	locks      map[string]*sync.Mutex
}

func NewNamedLockSet() *namedLockSet {
	return &namedLockSet{
		globalLock: sync.Mutex{},
		locks:      map[string]*sync.Mutex{},
	}
}

func (n *namedLockSet) Lock(name string) {
	func() {
		n.globalLock.Lock()
		defer n.globalLock.Unlock()
		if _, ok := n.locks[name]; !ok {
			n.locks[name] = &sync.Mutex{}
		}
	}()
	// This call cannot be guarded by the global lock as it would block the access
	// to the other locks
	n.locks[name].Lock()
}

// Unlock unlocks the named lock. Can only be called after a previous Lock() call
// for the same named lock.
func (n *namedLockSet) Unlock(name string) {
	// cannot be locked by the global lock as it would deadlock once GlobalLock() gets activated
	if _, ok := n.locks[name]; ok {
		n.locks[name].Unlock()
	}
}

// GlobalLock first locks access to the named locks and then locks all of the
// set locks
func (n *namedLockSet) GlobalLock() {
	n.globalLock.Lock()
	for _, l := range n.locks {
		l.Lock()
	}
}

// GlobalUnlock should only be called after GlobalLock(). It unlocks all the locks
// of the set and then it also unlocks the global lock gating access to the set locks.
func (n *namedLockSet) GlobalUnlock() {
	for _, l := range n.locks {
		l.Unlock()
	}
	n.globalLock.Unlock()
}

// FileBasedImagePullManager is an implementation of the ImagePullManager. It
// tracks images pulled by the kubelet by creating records about ongoing and
// successful pulls.
// It tracks the credentials used with each successful pull in order to be able
// to distinguish tenants requesting access to an image that exists on the kubelet's
// node.
type FileBasedImagePullManager struct {
	pullingDir string
	pulledDir  string

	requireCredentialVerification ImagePullPolicyEnforcer

	imageService kubecontainer.ImageService

	intentAccessors *namedLockSet  // image -> sync.Mutex
	intentCounters  map[string]int // image -> number of current in-flight pulls

	pulledAccessors *namedLockSet // imageRef -> sync.Mutex

	encoder runtime.Encoder
	decoder runtime.Decoder
}

func NewFileBasedImagePullManager(ctx context.Context, kubeletDir string, imagePullPolicy ImagePullPolicyEnforcer, imageService kubecontainer.ImageService) (*FileBasedImagePullManager, error) {
	kubeletConfigEncoder, kubeletConfigDecoder, err := createKubeletConfigSchemeEncoderDecoder()
	if err != nil {
		return nil, err
	}

	m := &FileBasedImagePullManager{
		pullingDir: filepath.Join(kubeletDir, "image_manager", "pulling"),
		pulledDir:  filepath.Join(kubeletDir, "image_manager", "pulled"),

		requireCredentialVerification: imagePullPolicy,

		imageService: imageService,

		intentAccessors: NewNamedLockSet(),
		intentCounters:  make(map[string]int),

		pulledAccessors: NewNamedLockSet(),

		encoder: kubeletConfigEncoder,
		decoder: kubeletConfigDecoder,
	}

	if err := os.MkdirAll(m.pullingDir, 0700); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(m.pulledDir, 0700); err != nil {
		return nil, err
	}

	m.initialize(ctx)

	return m, nil
}

func (f *FileBasedImagePullManager) RecordPullIntent(image string) error {
	f.intentAccessors.Lock(image)
	defer f.intentAccessors.Unlock(image)

	intent := kubeletconfigv1alpha1.ImagePullIntent{
		Image: image,
	}

	intentBytes := bytes.NewBuffer([]byte{})
	if err := f.encoder.Encode(&intent, intentBytes); err != nil {
		return err
	}

	if err := writeFile(f.pullingDir, cacheFilename(image), intentBytes.Bytes()); err != nil {
		return err
	}

	f.intentCounters[image]++
	return nil
}

func (f *FileBasedImagePullManager) RecordImagePulled(image, imageRef string, credentials *kubeletconfiginternal.ImagePullCredentials) {
	if err := f.writePulledRecord(image, imageRef, credentials); err != nil {
		klog.ErrorS(err, "failed to write image pulled record", "imageRef", imageRef)
		return
	}

	f.decrementImagePullIntent(image)
}

// writePulledRecord writes an ImagePulledRecord into the f.pulledDir directory.
// `image` is an image from a container of a Pod object.
// `imageRef` is a reference to the `image“ as used by the CRI.
// `credentials` is a set of credentials that should be written to a new/merged into
// an existing record.
//
// If `credentials` is nil, it marks a situation where an image was pulled under
// unknown circumstances. We should record the image as tracked but no credentials
// should be written in order to force credential verification when the image is
// accessed the next time.
func (f *FileBasedImagePullManager) writePulledRecord(image, imageRef string, credentials *kubeletconfiginternal.ImagePullCredentials) error {
	f.pulledAccessors.Lock(imageRef)
	defer f.pulledAccessors.Unlock(imageRef)

	var pulledRecord *kubeletconfiginternal.ImagePulledRecord
	var pulledRecordChanged bool

	sanitizedImage, err := trimImageTagDigest(image)
	if err != nil {
		return fmt.Errorf("invalidate image name %q: %w", image, err)
	}

	pulledRecordPath := filepath.Join(f.pulledDir, cacheFilename(imageRef))
	pulledFile, err := os.ReadFile(pulledRecordPath)
	if os.IsNotExist(err) {
		pulledRecordChanged = true
		pulledRecord = &kubeletconfiginternal.ImagePulledRecord{
			LastUpdatedTime:   metav1.Time{Time: time.Now()},
			ImageRef:          imageRef,
			CredentialMapping: make(map[string]kubeletconfiginternal.ImagePullCredentials),
		}
		if credentials != nil {
			pulledRecord.CredentialMapping[sanitizedImage] = *credentials
		}
	} else if err == nil {
		pulledRecord, err = decodePulledRecord(f.decoder, pulledFile)
		if err != nil {
			return fmt.Errorf("failed to decode ImagePulledRecord: %w", err)
		}

		pulledRecord, pulledRecordChanged = pulledRecordMergeNewCreds(pulledRecord, sanitizedImage, credentials)
	} else {
		return fmt.Errorf("failed to open %q for reading: %w", pulledRecordPath, err)
	}

	if !pulledRecordChanged {
		return nil
	}
	recordBytes := bytes.NewBuffer([]byte{})
	if err := f.encoder.Encode(pulledRecord, recordBytes); err != nil {
		return fmt.Errorf("failed to serialize ImagePulledRecord: %w", err)
	}

	if err := writeFile(f.pulledDir, cacheFilename(imageRef), recordBytes.Bytes()); err != nil {
		return fmt.Errorf("failed to write ImagePulledRecord file: %w", err)
	}

	return nil

}

func (f *FileBasedImagePullManager) RecordImagePullFailed(image string) {
	f.decrementImagePullIntent(image)
}

// decrementImagePullIntent decreses the number of how many times image pull
// intent for a given `image` was requested, and removes the ImagePullIntent file
// if the reference counter for the image reaches zero.
func (f *FileBasedImagePullManager) decrementImagePullIntent(image string) {
	f.intentAccessors.Lock(image)
	defer f.intentAccessors.Unlock(image)

	f.intentCounters[image]--
	if f.intentCounters[image] > 0 {
		return
	}
	delete(f.intentCounters, image)

	intentRecordPath := filepath.Join(f.pullingDir, cacheFilename(image))
	if err := os.Remove(intentRecordPath); err != nil && !os.IsNotExist(err) {
		klog.ErrorS(err, "failed to remove file", "filePath", intentRecordPath)
	}
}

func (f *FileBasedImagePullManager) MustAttemptImagePull(image, imageRef string, podSecrets []kubeletconfiginternal.ImagePullSecret) bool {
	if len(imageRef) == 0 {
		return true
	}

	pulledFilePath := filepath.Join(f.pulledDir, cacheFilename(imageRef))

	var imagePulledByKubelet bool
	var pulledFile []byte

	err := func() error {
		// don't allow changes to the files we're using for our decision
		f.pulledAccessors.Lock(imageRef)
		defer f.pulledAccessors.Unlock(imageRef)
		f.intentAccessors.Lock(image)
		defer f.intentAccessors.Unlock(image)

		var err error
		pulledFile, err = os.ReadFile(pulledFilePath)
		if err == nil {
			imagePulledByKubelet = true
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to open %q for reading: %w", pulledFilePath, err)
		} else {
			pullIntentPath := filepath.Join(f.pullingDir, cacheFilename(image))
			_, err := os.ReadFile(pullIntentPath)
			if err == nil {
				imagePulledByKubelet = true
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("failed to open %q for reading: %w", pullIntentPath, err)

			}
		}
		return nil
	}()

	if err != nil {
		klog.ErrorS(err, "Unable to read files in directories containing records about image pulls")
		return true
	}

	if !f.requireCredentialVerification(image, len(imageRef) > 0, imagePulledByKubelet) {
		return false
	}

	if len(pulledFile) == 0 {
		// we have no proper records of the image being pulled in the past, we can short-circuit here
		return true
	}

	pulledDeserialized, err := decodePulledRecord(f.decoder, pulledFile)
	if err != nil {
		klog.ErrorS(err, "failed to decode ImagePulledRecord")
		return true
	}

	sanitizedImage, err := trimImageTagDigest(image)
	if err != nil {
		klog.ErrorS(err, "failed to parse image name, forcing image credentials reverification", "image", sanitizedImage)
		return true
	}

	cachedCreds, ok := pulledDeserialized.CredentialMapping[sanitizedImage]
	if !ok {
		return true
	}

	if cachedCreds.NodePodsAccessible {
		// anyone on this node can access the image
		return false
	}

	if len(cachedCreds.KubernetesSecrets) == 0 {
		return true
	}

	for _, podSecret := range podSecrets {
		for _, cachedSecret := range cachedCreds.KubernetesSecrets {

			// we need to check hash len in case hashing failed while storing the record in the keyring
			if len(cachedSecret.CredentialHash) > 0 && podSecret.CredentialHash == cachedSecret.CredentialHash {
				// TODO: should we record the new secret in case it has different coordinates? If the secret rotates, we will pull unnecessarily otherwise
				return false
			}

			if podSecret.UID == cachedSecret.UID &&
				podSecret.Namespace == cachedSecret.Namespace &&
				podSecret.Name == cachedSecret.Name {
				// TODO: should we record the new creds in this case so that we don't pull if these are present in a different secret?
				return false
			}
		}
	}

	return true
}

func (f *FileBasedImagePullManager) PruneUnknownRecords(imageList []string, until time.Time) {
	f.pulledAccessors.GlobalLock()
	defer f.pulledAccessors.GlobalUnlock()

	var imageRecords []string
	err := processDirFiles(f.pulledDir,
		func(filePath string, fileContent []byte) error {
			pullRecord, err := decodePulledRecord(f.decoder, fileContent)
			if err != nil {
				klog.V(5).InfoS("failed to deserialize, skipping file", "filePath", filePath, "error", err)
				return nil
			}

			if pullRecord.LastUpdatedTime.Time.Before(until) {
				imageRecords = append(imageRecords, pullRecord.ImageRef)
			}
			return nil
		})

	if err != nil {
		klog.ErrorS(err, "failed to garbage collect ImagePulledRecord files")
		return
	}

	imagesInUse := sets.New(imageList...)
	for _, ir := range imageRecords {
		if imagesInUse.Has(ir) {
			continue
		}

		recordToRemovePath := filepath.Join(f.pulledDir, cacheFilename(ir))
		if err := os.Remove(recordToRemovePath); err != nil {
			klog.ErrorS(err, "failed to remove an ImagePulledRecord file", "filePath", recordToRemovePath)
		}
	}

}

// initialize gathers all the images from pull intent records that exist
// from the previous kubelet runs.
// If the CRI reports any of the above images as already pulled, we turn the
// pull intent into a pulled record and the original pull intent is deleted.
//
// This method is not thread-safe and it should only be called upon the creation
// of the FileBasedImagePullManager.
func (f *FileBasedImagePullManager) initialize(ctx context.Context) {
	inFlightPulls := make(map[string]string) // image -> path

	// walk the pulling directory for any pull intent records
	err := processDirFiles(f.pullingDir,
		func(filePath string, fileContent []byte) error {
			intent, err := decodeIntent(f.decoder, fileContent)
			if err != nil {
				klog.V(4).InfoS("deleting file, failed to deserialize to ImagePullIntent", "filePath", filePath, "err", err)
				if err := os.Remove(filePath); err != nil {
					klog.ErrorS(err, "failed to remove ImagePullIntent file", "filePath", filePath)
				}
				return nil
			}

			inFlightPulls[intent.Image] = filePath
			return nil
		})
	if err != nil {
		klog.ErrorS(err, "there was an error walking the directory", "directoryPath", f.pullingDir)
		return
	}

	if len(inFlightPulls) == 0 {
		return
	}

	images, err := f.imageService.ListImages(ctx)
	if err != nil {
		klog.ErrorS(err, "failed to list images")
	}

	// Each of the images known to the CRI might consist of multiple tags and digests,
	// which is what we track in the ImagePullIntent - we need to go through all of these
	// for each image.
	for _, image := range images {
		imageName := searchForExistingTagDigest(inFlightPulls, image)
		if len(imageName) == 0 {
			continue
		}

		if err := f.writePulledRecord(imageName, image.ID, nil); err != nil {
			klog.ErrorS(err, "failed to write an image pull record", "imageRef", image.ID)
			continue
		}
		removePath := inFlightPulls[imageName]
		if err := os.Remove(removePath); err != nil {
			klog.V(2).InfoS("failed to remove image pull intent file", "filePath", removePath, "error", err)
		}

	}
}

var _ ImagePullManager = &NoopImagePullManager{}

type NoopImagePullManager struct{}

func (m *NoopImagePullManager) RecordPullIntent(_ string) error { return nil }
func (m *NoopImagePullManager) RecordImagePulled(_, _ string, _ *kubeletconfiginternal.ImagePullCredentials) {
}
func (m *NoopImagePullManager) RecordImagePullFailed(image string) {}
func (m *NoopImagePullManager) MustAttemptImagePull(_, _ string, _ []kubeletconfiginternal.ImagePullSecret) bool {
	return false
}
func (m *NoopImagePullManager) PruneUnknownRecords(_ []string, _ time.Time) {}

func createKubeletConfigSchemeEncoderDecoder() (runtime.Encoder, runtime.Decoder, error) {
	const mediaType = runtime.ContentTypeJSON

	scheme := runtime.NewScheme()
	if err := kubeletconfigvint1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	if err := kubeletconfiginternal.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}

	// use the strict scheme to fail on unknown fields
	codecs := serializer.NewCodecFactory(scheme, serializer.EnableStrict)

	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), mediaType)
	if !ok {
		return nil, nil, fmt.Errorf("unable to locate encoder -- %q is not a supported media type", mediaType)
	}
	return codecs.EncoderForVersion(info.Serializer, kubeletconfigv1alpha1.SchemeGroupVersion), codecs.UniversalDecoder(), nil
}

// searchForExistingTagDigest loop through the `image` RepoDigests and RepoTags
// and tries to find a digest/tag in `trackedImages`, which is a map of
// containerImage -> pulling intent path.
func searchForExistingTagDigest(trackedImages map[string]string, image kubecontainer.Image) string {
	for _, digest := range image.RepoDigests {
		if _, ok := trackedImages[digest]; ok {
			return digest
		}
	}

	for _, tag := range image.RepoTags {
		if _, ok := trackedImages[tag]; ok {
			return tag
		}
	}

	return ""
}

type kubeSecretCoordinates struct {
	UID       string
	Namespace string
	Name      string
}

// pulledRecordMergeNewCreds merges the credentials from `newCreds` into the `orig`
// record for the `imageNoTagDigest` image.
// `imageNoTagDigest` is the content of the `image` field from a pod's container
// after any tag or digest were removed from it.
func pulledRecordMergeNewCreds(orig *kubeletconfiginternal.ImagePulledRecord, imageNoTagDigest string, newCreds *kubeletconfiginternal.ImagePullCredentials) (*kubeletconfiginternal.ImagePulledRecord, bool) {
	if newCreds == nil || (!newCreds.NodePodsAccessible && len(newCreds.KubernetesSecrets) == 0) {
		return orig, false
	}
	ret := orig.DeepCopy()
	selectedCreds, found := ret.CredentialMapping[imageNoTagDigest]
	if !found {
		if ret.CredentialMapping == nil {
			ret.CredentialMapping = make(map[string]kubeletconfiginternal.ImagePullCredentials)
		}
		ret.CredentialMapping[imageNoTagDigest] = *newCreds
	} else {
		if newCreds.NodePodsAccessible {
			selectedCreds.NodePodsAccessible = true
			selectedCreds.KubernetesSecrets = nil
		} else {
			selectedCreds.KubernetesSecrets = mergePullSecrets(selectedCreds.KubernetesSecrets, newCreds.KubernetesSecrets)
		}
		ret.CredentialMapping[imageNoTagDigest] = selectedCreds
	}

	updated := !reflect.DeepEqual(orig.CredentialMapping, ret.CredentialMapping)
	if updated {
		ret.LastUpdatedTime = metav1.Time{Time: time.Now()}
	}
	return ret, updated
}

// mergePullSecrets merges two slices of ImagePullSecret object into one while
// keeping the objects unique per `Namespace, Name, UID` key.
//
// In case an object from the `new` slice has the same `Namespace, Name, UID` combination
// as an object from `orig`, the result will use the CredentialHash value of the
// object from `new`.
//
// The returned slice is sorted by Namespace, Name and UID (in this order).
func mergePullSecrets(orig, new []kubeletconfiginternal.ImagePullSecret) []kubeletconfiginternal.ImagePullSecret {
	credSet := make(map[kubeSecretCoordinates]string)
	for _, secret := range orig {
		credSet[kubeSecretCoordinates{
			UID:       secret.UID,
			Namespace: secret.Namespace,
			Name:      secret.Name,
		}] = secret.CredentialHash
	}

	for _, s := range new {
		credSet[kubeSecretCoordinates{UID: s.UID, Namespace: s.Namespace, Name: s.Name}] = s.CredentialHash
	}

	ret := make([]kubeletconfiginternal.ImagePullSecret, 0, len(credSet))
	for coords, hash := range credSet {
		ret = append(ret, kubeletconfiginternal.ImagePullSecret{
			UID:            coords.UID,
			Namespace:      coords.Namespace,
			Name:           coords.Name,
			CredentialHash: hash,
		})
	}
	// we don't need to use the stable version because secret coordinates used for ordering are unique in the set
	slices.SortFunc(ret, imagePullSecretLess)

	return ret
}

func cacheFilename(image string) string {
	return fmt.Sprintf("sha256-%x", sha256.Sum256([]byte(image)))
}

// imagePullSecretLess is a helper function to define ordering in a slice of
// ImagePullSecret objects.
func imagePullSecretLess(a, b kubeletconfiginternal.ImagePullSecret) int {
	if a.Namespace < b.Namespace {
		return -1
	} else if a.Namespace > b.Namespace {
		return 1
	}

	if a.Name < b.Name {
		return -1
	} else if a.Name > b.Name {
		return 1
	}

	if a.UID < b.UID {
		return -1
	}
	return 1
}

// writeFile writes `content` to the file with name `filename` in directory `dir`.
// It assures write atomicity by creating a temporary file first and only after
// a successful write, it move the temp file in place of the target.
func writeFile(dir, filename string, content []byte) error {
	// create target folder if it does not exists yet
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory %q: %w", dir, err)
	}

	targetPath := filepath.Join(dir, filename)
	tmpPath := targetPath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0600); err != nil {
		return fmt.Errorf("failed to create temporary file %q: %w", tmpPath, err)
	}

	return os.Rename(tmpPath, targetPath)
}

// trimImageTagDigest removes the tag and digest from an image name
func trimImageTagDigest(containerImage string) (string, error) {
	imageName, _, _, err := parsers.ParseImageName(containerImage)
	return imageName, err
}

func processDirFiles(dirName string, fileAction func(filePath string, fileContent []byte) error) error {
	return filepath.WalkDir(dirName, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if path == dirName {
			return nil
		}

		if d.IsDir() {
			klog.V(4).InfoS("path is a directory, skipping", "path", path)
			return nil
		}

		fileContent, err := os.ReadFile(path)
		if err != nil {
			klog.ErrorS(err, "skipping file, failed to read", "filePath", path)
			return nil
		}

		if err := fileAction(path, fileContent); err != nil {
			return err
		}

		return nil
	})
}

func decodeIntent(d runtime.Decoder, objBytes []byte) (*kubeletconfiginternal.ImagePullIntent, error) {
	obj, _, err := d.Decode(objBytes, nil, nil)
	if err != nil {
		return nil, err
	}

	intentObj, ok := obj.(*kubeletconfiginternal.ImagePullIntent)
	if !ok {
		return nil, fmt.Errorf("failed to convert object to *ImagePullIntent: %v", obj)
	}

	return intentObj, nil
}

func decodePulledRecord(d runtime.Decoder, objBytes []byte) (*kubeletconfiginternal.ImagePulledRecord, error) {
	obj, _, err := d.Decode(objBytes, nil, nil)
	if err != nil {
		return nil, err
	}

	pulledRecord, ok := obj.(*kubeletconfiginternal.ImagePulledRecord)
	if !ok {
		return nil, fmt.Errorf("failed to convert object to *ImagePulledRecord: %v", obj)
	}

	return pulledRecord, nil
}

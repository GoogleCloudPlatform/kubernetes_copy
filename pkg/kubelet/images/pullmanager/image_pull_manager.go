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

package pullmanager

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/util/parsers"
)

var _ ImagePullManager = &PullManager{}

// writeRecordWhileMatchingLimit is a limit at which we stop writing yet-uncached
// records that we found when we were checking if an image pull must be attempted.
// This is to prevent unbounded writes in cases of high namespace turnover.
const writeRecordWhileMatchingLimit = 100

// PullManager is an implementation of the ImagePullManager. It
// tracks images pulled by the kubelet by creating records about ongoing and
// successful pulls.
// It tracks the credentials used with each successful pull in order to be able
// to distinguish tenants requesting access to an image that exists on the kubelet's
// node.
type PullManager struct {
	recordsAccessor PullRecordsAccessor

	requireCredentialVerification ImagePullPolicyEnforcer

	imageService kubecontainer.ImageService

	intentAccessors *namedLockSet  // image -> sync.Mutex
	intentCounters  map[string]int // image -> number of current in-flight pulls

	pulledAccessors *namedLockSet // imageRef -> sync.Mutex
}

func NewImagePullManager(ctx context.Context, recordsAccessor PullRecordsAccessor, imagePullPolicy ImagePullPolicyEnforcer, imageService kubecontainer.ImageService) (*PullManager, error) {
	m := &PullManager{
		recordsAccessor: recordsAccessor,

		requireCredentialVerification: imagePullPolicy,

		imageService: imageService,

		intentAccessors: NewNamedLockSet(),
		intentCounters:  make(map[string]int),

		pulledAccessors: NewNamedLockSet(),
	}

	m.initialize(ctx)

	return m, nil
}

func (f *PullManager) RecordPullIntent(image string) error {
	f.intentAccessors.Lock(image)
	defer f.intentAccessors.Unlock(image)

	if err := f.recordsAccessor.WriteImagePullIntent(image); err != nil {
		return fmt.Errorf("failed to record image pull intent: %w", err)
	}

	f.intentCounters[image]++
	return nil
}

func (f *PullManager) RecordImagePulled(image, imageRef string, credentials *kubeletconfiginternal.ImagePullCredentials) {
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
func (f *PullManager) writePulledRecord(image, imageRef string, credentials *kubeletconfiginternal.ImagePullCredentials) error {
	f.pulledAccessors.Lock(imageRef)
	defer f.pulledAccessors.Unlock(imageRef)

	sanitizedImage, err := trimImageTagDigest(image)
	if err != nil {
		return fmt.Errorf("invalid image name %q: %w", image, err)
	}

	pulledRecord, _, err := f.recordsAccessor.GetImagePulledRecord(imageRef)
	if err != nil {
		return err
	}

	var pulledRecordChanged bool
	if pulledRecord == nil {
		pulledRecordChanged = true
		pulledRecord = &kubeletconfiginternal.ImagePulledRecord{
			LastUpdatedTime:   metav1.Time{Time: time.Now()},
			ImageRef:          imageRef,
			CredentialMapping: make(map[string]kubeletconfiginternal.ImagePullCredentials),
		}
		if credentials != nil {
			pulledRecord.CredentialMapping[sanitizedImage] = *credentials
		}
	} else {
		pulledRecord, pulledRecordChanged = pulledRecordMergeNewCreds(pulledRecord, sanitizedImage, credentials)
	}

	if !pulledRecordChanged {
		return nil
	}

	return f.recordsAccessor.WriteImagePulledRecord(pulledRecord)
}

func (f *PullManager) RecordImagePullFailed(image string) {
	f.decrementImagePullIntent(image)
}

// decrementImagePullIntent decreses the number of how many times image pull
// intent for a given `image` was requested, and removes the ImagePullIntent file
// if the reference counter for the image reaches zero.
func (f *PullManager) decrementImagePullIntent(image string) {
	f.intentAccessors.Lock(image)
	defer f.intentAccessors.Unlock(image)

	f.intentCounters[image]--
	if f.intentCounters[image] > 0 {
		return
	}
	delete(f.intentCounters, image)

	if err := f.recordsAccessor.DeleteImagePullIntent(image); err != nil {
		klog.ErrorS(err, "failed to remove image pull intent", "image", image)
	}
}

func (f *PullManager) MustAttemptImagePull(image, imageRef string, podSecrets []kubeletconfiginternal.ImagePullSecret) bool {
	if len(imageRef) == 0 {
		return true
	}

	var imagePulledByKubelet bool
	var pulledRecord *kubeletconfiginternal.ImagePulledRecord

	err := func() error {
		// don't allow changes to the files we're using for our decision
		f.pulledAccessors.Lock(imageRef)
		defer f.pulledAccessors.Unlock(imageRef)
		f.intentAccessors.Lock(image)
		defer f.intentAccessors.Unlock(image)

		var err error
		var exists bool
		pulledRecord, exists, err = f.recordsAccessor.GetImagePulledRecord(imageRef)
		switch {
		case err == nil && pulledRecord == nil:
			if exists, err := f.recordsAccessor.ImagePullIntentExists(image); err != nil {
				return fmt.Errorf("failed to check existence of an image pull intent: %w", err)
			} else if exists {
				imagePulledByKubelet = true
			}
		case err == nil && pulledRecord != nil:
			imagePulledByKubelet = true
		case err != nil && exists: // record exists but has invalid format
		default:
			return fmt.Errorf("failed to retrieve image pulled record: %w", err)
		}

		return nil
	}()

	if err != nil {
		klog.ErrorS(err, "Unable to access cache records about image pulls")
		return true
	}

	if !f.requireCredentialVerification(image, len(imageRef) > 0, imagePulledByKubelet) {
		return false
	}

	if pulledRecord == nil {
		// we have no proper records of the image being pulled in the past, we can short-circuit here
		return true
	}

	sanitizedImage, err := trimImageTagDigest(image)
	if err != nil {
		klog.ErrorS(err, "failed to parse image name, forcing image credentials reverification", "image", sanitizedImage)
		return true
	}

	cachedCreds, ok := pulledRecord.CredentialMapping[sanitizedImage]
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
			hashesMatch := len(cachedSecret.CredentialHash) > 0 && podSecret.CredentialHash == cachedSecret.CredentialHash
			secretCoordinatesMatch := podSecret.UID == cachedSecret.UID &&
				podSecret.Namespace == cachedSecret.Namespace &&
				podSecret.Name == cachedSecret.Name

			if hashesMatch {
				if !secretCoordinatesMatch && len(cachedCreds.KubernetesSecrets) < writeRecordWhileMatchingLimit {
					// While we're only matching at this point, we want to ensure this secret is considered valid in the future
					// and so we make an additional write to the cache.
					// writePulledRecord() is a noop in case the secret with the update hash already appears in the cache.
					if err := f.writePulledRecord(image, imageRef, &kubeletconfiginternal.ImagePullCredentials{KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{podSecret}}); err != nil {
						klog.ErrorS(err, "failed to write an image pulled record", "image", image, "imageRef", imageRef)
					}
				}
				return false
			}

			if secretCoordinatesMatch {
				if len(cachedCreds.KubernetesSecrets) < writeRecordWhileMatchingLimit { // we already know the hashes did not match, no need to check again
					// While we're only matching at this point, we want to ensure the updated credentials are considered valid in the future
					// and so we make an additional write to the cache.
					// writePulledRecord() is a noop in case the hash got updated in the meantime.
					if err := f.writePulledRecord(image, imageRef, &kubeletconfiginternal.ImagePullCredentials{KubernetesSecrets: []kubeletconfiginternal.ImagePullSecret{podSecret}}); err != nil {
						klog.ErrorS(err, "failed to write an image pulled record", "image", image, "imageRef", imageRef)
					}
					return false
				}
			}
		}
	}

	return true
}

func (f *PullManager) PruneUnknownRecords(imageList []string, until time.Time) {
	f.pulledAccessors.GlobalLock()
	defer f.pulledAccessors.GlobalUnlock()

	pulledRecords, err := f.recordsAccessor.ListImagePulledRecords()
	if err != nil {
		klog.ErrorS(err, "failed to garbage collect ImagePulledRecords")
		return
	}

	imagesInUse := sets.New(imageList...)
	for _, ir := range pulledRecords {
		if !ir.LastUpdatedTime.Time.Before(until) {
			// the image record was only update after the GC started
			continue
		}

		if imagesInUse.Has(ir.ImageRef) {
			continue
		}

		if err := f.recordsAccessor.DeleteImagePulledRecord(ir.ImageRef); err != nil {
			klog.ErrorS(err, "failed to remove an ImagePulledRecord", "imageRef", ir.ImageRef)
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
func (f *PullManager) initialize(ctx context.Context) {
	pullIntents, err := f.recordsAccessor.ListImagePullIntents()
	if err != nil {
		klog.ErrorS(err, "there was an error listing ImagePullIntents")
		return
	}

	if len(pullIntents) == 0 {
		return
	}

	images, err := f.imageService.ListImages(ctx)
	if err != nil {
		klog.ErrorS(err, "failed to list images")
	}

	inFlightPulls := sets.New[string]()
	for _, intent := range pullIntents {
		inFlightPulls.Insert(intent.Image)
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

		if err := f.recordsAccessor.DeleteImagePullIntent(imageName); err != nil {
			klog.V(2).InfoS("failed to remove image pull intent file", "imageName", imageName, "error", err)
		}

	}
}

// searchForExistingTagDigest loop through the `image` RepoDigests and RepoTags
// and tries to find a digest/tag in `trackedImages`, which is a map of
// containerImage -> pulling intent path.
func searchForExistingTagDigest(trackedImages sets.Set[string], image kubecontainer.Image) string {
	for _, digest := range image.RepoDigests {
		if ok := trackedImages.Has(digest); ok {
			return digest
		}
	}

	for _, tag := range image.RepoTags {
		if ok := trackedImages.Has(tag); ok {
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

// trimImageTagDigest removes the tag and digest from an image name
func trimImageTagDigest(containerImage string) (string, error) {
	imageName, _, _, err := parsers.ParseImageName(containerImage)
	return imageName, err
}

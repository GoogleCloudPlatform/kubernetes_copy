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
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/errors"
	kubeletconfigv1alpha1 "k8s.io/kubelet/config/v1alpha1"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletconfigvint1alpha1 "k8s.io/kubernetes/pkg/kubelet/apis/config/v1alpha1"
)

const (
	cacheFilesSHA256Prefix = "sha256-"
	tmpFilesSuffix         = ".tmp"
)

var _ PullRecordsAccessor = &fsPullRecordsAccessor{}

// fsPullRecordsAccessor uses the filesystem to read/write ImagePullIntent/ImagePulledRecord
// records.
type fsPullRecordsAccessor struct {
	pullingDir string
	pulledDir  string

	encoder runtime.Encoder
	decoder runtime.Decoder
}

// NewFSPullRecordsAccessor returns an accessor for the ImagePullIntent/ImagePulledRecord
// records with a filesystem as the backing database.
func NewFSPullRecordsAccessor(kubeletDir string) (*fsPullRecordsAccessor, error) {
	kubeletConfigEncoder, kubeletConfigDecoder, err := createKubeletConfigSchemeEncoderDecoder()
	if err != nil {
		return nil, err
	}

	accessor := &fsPullRecordsAccessor{
		pullingDir: filepath.Join(kubeletDir, "image_manager", "pulling"),
		pulledDir:  filepath.Join(kubeletDir, "image_manager", "pulled"),

		encoder: kubeletConfigEncoder,
		decoder: kubeletConfigDecoder,
	}

	if err := os.MkdirAll(accessor.pullingDir, 0700); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(accessor.pulledDir, 0700); err != nil {
		return nil, err
	}

	return accessor, nil
}

func (f *fsPullRecordsAccessor) WriteImagePullIntent(image string) error {
	intent := kubeletconfiginternal.ImagePullIntent{
		Image: image,
	}

	intentBytes := bytes.NewBuffer([]byte{})
	if err := f.encoder.Encode(&intent, intentBytes); err != nil {
		return err
	}

	return writeFile(f.pullingDir, cacheFilename(image), intentBytes.Bytes())
}

func (f *fsPullRecordsAccessor) ListImagePullIntents() ([]*kubeletconfiginternal.ImagePullIntent, error) {
	var intents []*kubeletconfiginternal.ImagePullIntent
	// walk the pulling directory for any pull intent records
	err := processDirFiles(f.pullingDir,
		func(filePath string, fileContent []byte) error {
			intent, err := decodeIntent(f.decoder, fileContent)
			if err != nil {
				return fmt.Errorf("failed to deserialize content of file %q into ImagePullIntent: %w", filePath, err)
			}
			intents = append(intents, intent)

			return nil
		})
	return intents, err
}

func (f *fsPullRecordsAccessor) ImagePullIntentExists(image string) (bool, error) {
	intentRecordPath := filepath.Join(f.pullingDir, cacheFilename(image))
	intentBytes, err := os.ReadFile(intentRecordPath)
	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	intent, err := decodeIntent(f.decoder, intentBytes)
	if err != nil {
		return false, err
	}

	return intent.Image == image, nil
}

func (f *fsPullRecordsAccessor) DeleteImagePullIntent(image string) error {
	err := os.Remove(filepath.Join(f.pullingDir, cacheFilename(image)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (f *fsPullRecordsAccessor) GetImagePulledRecord(imageRef string) (*kubeletconfiginternal.ImagePulledRecord, bool, error) {
	recordBytes, err := os.ReadFile(filepath.Join(f.pulledDir, cacheFilename(imageRef)))
	if os.IsNotExist(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}

	pulledRecord, err := decodePulledRecord(f.decoder, recordBytes)
	if err != nil {
		return nil, true, err
	}
	if pulledRecord.ImageRef != imageRef {
		return nil, false, nil
	}
	return pulledRecord, true, err
}

func (f *fsPullRecordsAccessor) ListImagePulledRecords() ([]*kubeletconfiginternal.ImagePulledRecord, error) {
	var pullRecords []*kubeletconfiginternal.ImagePulledRecord
	err := processDirFiles(f.pulledDir,
		func(filePath string, fileContent []byte) error {
			pullRecord, err := decodePulledRecord(f.decoder, fileContent)
			if err != nil {
				return fmt.Errorf("failed to deserialize content of file %q into ImagePulledRecord: %w", filePath, err)
			}
			pullRecords = append(pullRecords, pullRecord)
			return nil
		})

	return pullRecords, err
}

func (f *fsPullRecordsAccessor) WriteImagePulledRecord(pulledRecord *kubeletconfiginternal.ImagePulledRecord) error {
	recordBytes := bytes.NewBuffer([]byte{})
	if err := f.encoder.Encode(pulledRecord, recordBytes); err != nil {
		return fmt.Errorf("failed to serialize ImagePulledRecord: %w", err)
	}

	return writeFile(f.pulledDir, cacheFilename(pulledRecord.ImageRef), recordBytes.Bytes())
}

func (f *fsPullRecordsAccessor) DeleteImagePulledRecord(imageRef string) error {
	err := os.Remove(filepath.Join(f.pulledDir, cacheFilename(imageRef)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func cacheFilename(image string) string {
	return fmt.Sprintf("%s%x", cacheFilesSHA256Prefix, sha256.Sum256([]byte(image)))
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
	tmpPath := targetPath + tmpFilesSuffix
	if err := os.WriteFile(tmpPath, content, 0600); err != nil {
		_ = os.Remove(tmpPath) // attempt a delete in case the file was at least partially written
		return fmt.Errorf("failed to create temporary file %q: %w", tmpPath, err)
	}

	return os.Rename(tmpPath, targetPath)
}

// processDirFiles reads files in a given directory and peforms `fileAction` action on those.
func processDirFiles(dirName string, fileAction func(filePath string, fileContent []byte) error) error {
	var walkErrors []error
	err := filepath.WalkDir(dirName, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			walkErrors = append(walkErrors, err)
			return nil
		}

		if path == dirName {
			return nil
		}

		if d.IsDir() {
			return filepath.SkipDir
		}

		// skip files we didn't write or .tmp files
		if filename := d.Name(); !strings.HasPrefix(filename, cacheFilesSHA256Prefix) || strings.HasSuffix(filename, tmpFilesSuffix) {
			return nil
		}

		fileContent, err := os.ReadFile(path)
		if err != nil {
			walkErrors = append(walkErrors, fmt.Errorf("failed to read %q: %w", path, err))
			return nil
		}

		if err := fileAction(path, fileContent); err != nil {
			walkErrors = append(walkErrors, err)
			return nil
		}

		return nil
	})
	if err != nil {
		walkErrors = append(walkErrors, err)
	}
	if len(walkErrors) > 0 {
		return errors.NewAggregate(walkErrors)
	}

	return nil
}

// createKubeletCOnfigSchemeEncoderDecoder creates strict-encoding encoder and
// decoder for the internal and alpha kubelet config APIs.
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

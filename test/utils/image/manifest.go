/*
Copyright 2017 The Kubernetes Authors.

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

package image

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	yaml "sigs.k8s.io/yaml/goyaml.v2"
)

// RegistryList holds public and private image registries
type RegistryList struct {
	GcAuthenticatedRegistry  string `yaml:"gcAuthenticatedRegistry"`
	PromoterE2eRegistry      string `yaml:"promoterE2eRegistry"`
	BuildImageRegistry       string `yaml:"buildImageRegistry"`
	InvalidRegistry          string `yaml:"invalidRegistry"`
	GcEtcdRegistry           string `yaml:"gcEtcdRegistry"`
	GcRegistry               string `yaml:"gcRegistry"`
	SigStorageRegistry       string `yaml:"sigStorageRegistry"`
	PrivateRegistry          string `yaml:"privateRegistry"`
	DockerLibraryRegistry    string `yaml:"dockerLibraryRegistry"`
	CloudProviderGcpRegistry string `yaml:"cloudProviderGcpRegistry"`
}

// Config holds an images registry, name, and version
type Config struct {
	registry string
	name     string
	version  string
}

// SetRegistry sets an image registry in a Config struct
func (i *Config) SetRegistry(registry string) {
	i.registry = registry
}

// SetName sets an image name in a Config struct
func (i *Config) SetName(name string) {
	i.name = name
}

// SetVersion sets an image version in a Config struct
func (i *Config) SetVersion(version string) {
	i.version = version
}

func Init(repoList string) {
	registry, imageConfigs, originalImageConfigs = readRepoList(repoList)
}

func readRepoList(repoList string) (RegistryList, map[ImageID]Config, map[ImageID]Config) {
	registry := initRegistry

	if repoList == "" {
		imageConfigs, originalImageConfigs := initImageConfigs(registry)
		return registry, imageConfigs, originalImageConfigs
	}

	var fileContent []byte
	var err error
	if strings.HasPrefix(repoList, "https://") || strings.HasPrefix(repoList, "http://") {
		var b bytes.Buffer
		err = readFromURL(repoList, bufio.NewWriter(&b))
		if err != nil {
			panic(fmt.Errorf("error reading '%v' url contents: %v", repoList, err))
		}
		fileContent = b.Bytes()
	} else {
		fileContent, err = os.ReadFile(repoList)
		if err != nil {
			panic(fmt.Errorf("error reading '%v' file contents: %v", repoList, err))
		}
	}

	err = yaml.Unmarshal(fileContent, &registry)
	if err != nil {
		panic(fmt.Errorf("error unmarshalling '%v' YAML file: %v", repoList, err))
	}

	imageConfigs, originalImageConfigs := initImageConfigs(registry)

	return registry, imageConfigs, originalImageConfigs

}

// Essentially curl url | writer
func readFromURL(url string, writer io.Writer) error {
	httpTransport := new(http.Transport)
	httpTransport.Proxy = http.ProxyFromEnvironment

	c := &http.Client{Transport: httpTransport}
	r, err := c.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode >= 400 {
		return fmt.Errorf("%v returned %d", url, r.StatusCode)
	}
	_, err = io.Copy(writer, r.Body)
	if err != nil {
		return err
	}
	return nil
}

var (
	initRegistry = RegistryList{
		GcAuthenticatedRegistry:  "gcr.io/authenticated-image-pulling",
		PromoterE2eRegistry:      "registry.k8s.io/e2e-test-images",
		BuildImageRegistry:       "registry.k8s.io/build-image",
		InvalidRegistry:          "invalid.registry.k8s.io/invalid",
		GcEtcdRegistry:           "registry.k8s.io",
		GcRegistry:               "registry.k8s.io",
		SigStorageRegistry:       "registry.k8s.io/sig-storage",
		PrivateRegistry:          "gcr.io/k8s-authenticated-test",
		DockerLibraryRegistry:    "docker.io/library",
		CloudProviderGcpRegistry: "registry.k8s.io/cloud-provider-gcp",
	}

	registry, imageConfigs, originalImageConfigs = readRepoList(os.Getenv("KUBE_TEST_REPO_LIST"))
)

type ImageID int

const (
	// None is to be used for unset/default images
	None ImageID = iota
	// Agnhost image
	Agnhost
	// AgnhostPrivate image
	AgnhostPrivate
	// APIServer image
	APIServer
	// AppArmorLoader image
	AppArmorLoader
	// AuthenticatedAlpine image
	AuthenticatedAlpine
	// AuthenticatedWindowsNanoServer image
	AuthenticatedWindowsNanoServer
	// BusyBox image
	BusyBox
	// DistrolessIptables Image
	DistrolessIptables
	// Etcd image
	Etcd
	// Httpd image
	Httpd
	// HttpdNew image
	HttpdNew
	// InvalidRegistryImage image
	InvalidRegistryImage
	// IpcUtils image
	IpcUtils
	// JessieDnsutils image
	JessieDnsutils
	// Kitten image
	Kitten
	// Nautilus image
	Nautilus
	// NFSProvisioner image
	NFSProvisioner
	// Nginx image
	Nginx
	// NginxNew image
	NginxNew
	// NodePerfNpbEp image
	NodePerfNpbEp
	// NodePerfNpbIs image
	NodePerfNpbIs
	// NodePerfTfWideDeep image
	NodePerfTfWideDeep
	// Nonewprivs image
	Nonewprivs
	// NonRoot runs with a default user of 1234
	NonRoot
	// Pause - when these values are updated, also update cmd/kubelet/app/options/container_runtime.go
	// Pause image
	Pause
	// Perl image
	Perl
	// Redis image
	Redis
	// RegressionIssue74839 image
	RegressionIssue74839
	// ResourceConsumer image
	ResourceConsumer
	// VolumeNFSServer image
	VolumeNFSServer
	// VolumeISCSIServer image
	VolumeISCSIServer
)

func initImageConfigs(list RegistryList) (map[ImageID]Config, map[ImageID]Config) {
	configs := map[ImageID]Config{}
	configs[Agnhost] = Config{list.PromoterE2eRegistry, "agnhost", "2.53"}
	configs[AgnhostPrivate] = Config{list.PrivateRegistry, "agnhost", "2.6"}
	configs[AuthenticatedAlpine] = Config{list.GcAuthenticatedRegistry, "alpine", "3.7"}
	configs[AuthenticatedWindowsNanoServer] = Config{list.GcAuthenticatedRegistry, "windows-nanoserver", "v1"}
	configs[APIServer] = Config{list.PromoterE2eRegistry, "sample-apiserver", "1.29.2"}
	configs[AppArmorLoader] = Config{list.PromoterE2eRegistry, "apparmor-loader", "1.4"}
	configs[BusyBox] = Config{list.PromoterE2eRegistry, "busybox", "1.36.1-1"}
	configs[DistrolessIptables] = Config{list.BuildImageRegistry, "distroless-iptables", "v0.6.6"}
	configs[Etcd] = Config{list.GcEtcdRegistry, "etcd", "3.5.17-0"}
	configs[Httpd] = Config{list.PromoterE2eRegistry, "httpd", "2.4.38-4"}
	configs[HttpdNew] = Config{list.PromoterE2eRegistry, "httpd", "2.4.39-4"}
	configs[InvalidRegistryImage] = Config{list.InvalidRegistry, "alpine", "3.1"}
	configs[IpcUtils] = Config{list.PromoterE2eRegistry, "ipc-utils", "1.3"}
	configs[JessieDnsutils] = Config{list.PromoterE2eRegistry, "jessie-dnsutils", "1.7"}
	configs[Kitten] = Config{list.PromoterE2eRegistry, "kitten", "1.7"}
	configs[Nautilus] = Config{list.PromoterE2eRegistry, "nautilus", "1.7"}
	configs[NFSProvisioner] = Config{list.SigStorageRegistry, "nfs-provisioner", "v4.0.8"}
	configs[Nginx] = Config{list.PromoterE2eRegistry, "nginx", "1.14-4"}
	configs[NginxNew] = Config{list.PromoterE2eRegistry, "nginx", "1.15-4"}
	configs[NodePerfNpbEp] = Config{list.PromoterE2eRegistry, "node-perf/npb-ep", "1.2"}
	configs[NodePerfNpbIs] = Config{list.PromoterE2eRegistry, "node-perf/npb-is", "1.2"}
	configs[NodePerfTfWideDeep] = Config{list.PromoterE2eRegistry, "node-perf/tf-wide-deep", "1.3"}
	configs[Nonewprivs] = Config{list.PromoterE2eRegistry, "nonewprivs", "1.3"}
	configs[NonRoot] = Config{list.PromoterE2eRegistry, "nonroot", "1.4"}
	// Pause - when these values are updated, also update cmd/kubelet/app/options/container_runtime.go
	configs[Pause] = Config{list.GcRegistry, "pause", "3.10"}
	configs[Perl] = Config{list.PromoterE2eRegistry, "perl", "5.26"}
	configs[Redis] = Config{list.PromoterE2eRegistry, "redis", "5.0.5-3"}
	configs[RegressionIssue74839] = Config{list.PromoterE2eRegistry, "regression-issue-74839", "1.2"}
	configs[ResourceConsumer] = Config{list.PromoterE2eRegistry, "resource-consumer", "1.13"}
	configs[VolumeNFSServer] = Config{list.PromoterE2eRegistry, "volume/nfs", "1.4"}
	configs[VolumeISCSIServer] = Config{list.PromoterE2eRegistry, "volume/iscsi", "2.6"}

	// This adds more config entries. Those have no pre-defined ImageID number,
	// but will be used via ReplaceRegistryInImageURL when deploying
	// CSI drivers (test/e2e/storage/util/create.go).
	appendCSIImageConfigs(configs)

	// if requested, map all the SHAs into a known format based on the input
	originalImageConfigs := configs
	if repo := os.Getenv("KUBE_TEST_REPO"); len(repo) > 0 {
		configs = GetMappedImageConfigs(originalImageConfigs, repo)
	}

	return configs, originalImageConfigs
}

// GetMappedImageConfigs returns the images if they were mapped to the provided
// image repository.
func GetMappedImageConfigs(originalImageConfigs map[ImageID]Config, repo string) map[ImageID]Config {
	configs := make(map[ImageID]Config)
	for i, config := range originalImageConfigs {
		switch i {
		case InvalidRegistryImage, AuthenticatedAlpine,
			AuthenticatedWindowsNanoServer, AgnhostPrivate:
			// These images are special and can't be run out of the cloud - some because they
			// are authenticated, and others because they are not real images. Tests that depend
			// on these images can't be run without access to the public internet.
			configs[i] = config
			continue
		}

		// Build a new tag with the ImageID, a hash of the image spec (to be unique) and
		// shorten and make the pull spec "safe" so it will fit in the tag
		configs[i] = getRepositoryMappedConfig(i, config, repo)
	}
	return configs
}

var (
	reCharSafe = regexp.MustCompile(`[^\w]`)
	reDashes   = regexp.MustCompile(`-+`)
)

// getRepositoryMappedConfig maps an existing image to the provided repo, generating a
// tag that is unique with the input config. The tag will contain the imageID, a hash of
// the image spec (to be unique) and shorten and make the pull spec "safe" so it will
// fit in the tag to allow a human to recognize the value. If imageID is None, then no
// imageID will be added to the tag.
func getRepositoryMappedConfig(imageID ImageID, config Config, repo string) Config {
	parts := strings.SplitN(repo, "/", 2)
	registry, name := parts[0], parts[1]

	pullSpec := config.GetE2EImage()

	h := sha256.New()
	h.Write([]byte(pullSpec))
	hash := base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:16]

	shortName := reCharSafe.ReplaceAllLiteralString(pullSpec, "-")
	shortName = reDashes.ReplaceAllLiteralString(shortName, "-")
	maxLength := 127 - 16 - 6 - 10
	if len(shortName) > maxLength {
		shortName = shortName[len(shortName)-maxLength:]
	}
	var version string
	if imageID == None {
		version = fmt.Sprintf("e2e-%s-%s", shortName, hash)
	} else {
		version = fmt.Sprintf("e2e-%d-%s-%s", imageID, shortName, hash)
	}

	return Config{
		registry: registry,
		name:     name,
		version:  version,
	}
}

// GetOriginalImageConfigs returns the configuration before any mapping rules.
func GetOriginalImageConfigs() map[ImageID]Config {
	return originalImageConfigs
}

// GetImageConfigs returns the map of imageConfigs
func GetImageConfigs() map[ImageID]Config {
	return imageConfigs
}

// GetConfig returns the Config object for an image
func GetConfig(image ImageID) Config {
	return imageConfigs[image]
}

// GetE2EImage returns the fully qualified URI to an image (including version)
func GetE2EImage(image ImageID) string {
	return fmt.Sprintf("%s/%s:%s", imageConfigs[image].registry, imageConfigs[image].name, imageConfigs[image].version)
}

// GetE2EImage returns the fully qualified URI to an image (including version)
func (i *Config) GetE2EImage() string {
	return fmt.Sprintf("%s/%s:%s", i.registry, i.name, i.version)
}

// GetPauseImageName returns the pause image name with proper version
func GetPauseImageName() string {
	return GetE2EImage(Pause)
}

// ReplaceRegistryInImageURL replaces the registry in the image URL with a custom one based
// on the configured registries.
func ReplaceRegistryInImageURL(imageURL string) (string, error) {
	return replaceRegistryInImageURLWithList(imageURL, registry)
}

// replaceRegistryInImageURLWithList replaces the registry in the image URL with a custom one based
// on the given registry list.
func replaceRegistryInImageURLWithList(imageURL string, reg RegistryList) (string, error) {
	parts := strings.Split(imageURL, "/")
	countParts := len(parts)
	registryAndUser := strings.Join(parts[:countParts-1], "/")

	if repo := os.Getenv("KUBE_TEST_REPO"); len(repo) > 0 {
		imageID := None
		for i, v := range originalImageConfigs {
			if v.GetE2EImage() == imageURL {
				imageID = i
				break
			}
		}
		last := strings.SplitN(parts[countParts-1], ":", 2)
		if len(last) == 1 {
			return "", fmt.Errorf("image %q is required to be in an image:tag format", imageURL)
		}
		config := getRepositoryMappedConfig(imageID, Config{
			registry: parts[0],
			name:     strings.Join([]string{strings.Join(parts[1:countParts-1], "/"), last[0]}, "/"),
			version:  last[1],
		}, repo)
		return config.GetE2EImage(), nil
	}

	switch registryAndUser {
	case initRegistry.GcRegistry:
		registryAndUser = reg.GcRegistry
	case initRegistry.SigStorageRegistry:
		registryAndUser = reg.SigStorageRegistry
	case initRegistry.PrivateRegistry:
		registryAndUser = reg.PrivateRegistry
	case initRegistry.InvalidRegistry:
		registryAndUser = reg.InvalidRegistry
	case initRegistry.PromoterE2eRegistry:
		registryAndUser = reg.PromoterE2eRegistry
	case initRegistry.BuildImageRegistry:
		registryAndUser = reg.BuildImageRegistry
	case initRegistry.GcAuthenticatedRegistry:
		registryAndUser = reg.GcAuthenticatedRegistry
	case initRegistry.DockerLibraryRegistry:
		registryAndUser = reg.DockerLibraryRegistry
	case initRegistry.CloudProviderGcpRegistry:
		registryAndUser = reg.CloudProviderGcpRegistry
	default:
		if countParts == 1 {
			// We assume we found an image from docker hub library
			// e.g. openjdk -> docker.io/library/openjdk
			registryAndUser = reg.DockerLibraryRegistry
			break
		}

		return "", fmt.Errorf("Registry: %s is missing in test/utils/image/manifest.go, please add the registry, otherwise the test will fail on air-gapped clusters", registryAndUser)
	}

	return fmt.Sprintf("%s/%s", registryAndUser, parts[countParts-1]), nil
}

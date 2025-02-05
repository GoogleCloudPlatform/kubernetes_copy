/*
Copyright 2016 The Kubernetes Authors.

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

package options

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/options/encryptionconfig"
	encryptionconfigcontroller "k8s.io/apiserver/pkg/server/options/encryptionconfig/controller"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/metrics"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	storagefactory "k8s.io/apiserver/pkg/storage/storagebackend/factory"
	storagevalue "k8s.io/apiserver/pkg/storage/value"
	"k8s.io/klog/v2"
)

type EtcdOptions struct {
	StorageConfig                           storagebackend.Config
	EncryptionProviderConfigFilepath        string
	EncryptionProviderConfigAutomaticReload bool

	EtcdServersOverrides []string

	// To enable protobuf as storage format, it is enough
	// to set it to "application/vnd.kubernetes.protobuf".
	DefaultStorageMediaType string
	DeleteCollectionWorkers int
	EnableGarbageCollection bool

	// Set EnableWatchCache to false to disable all watch caches
	EnableWatchCache bool
	// Set DefaultWatchCacheSize to zero to disable watch caches for those resources that have no explicit cache size set
	DefaultWatchCacheSize int
	// WatchCacheSizes represents override to a given resource
	WatchCacheSizes []string

	// SkipHealthEndpoints, when true, causes the Apply methods to not set up health endpoints.
	// This allows multiple invocations of the Apply methods without duplication of said endpoints.
	SkipHealthEndpoints bool
}

var storageTypes = sets.NewString(
	storagebackend.StorageTypeETCD3,
)

func NewEtcdOptions(backendConfig *storagebackend.Config) *EtcdOptions {
	options := &EtcdOptions{
		StorageConfig:           *backendConfig,
		DefaultStorageMediaType: "application/json",
		DeleteCollectionWorkers: 1,
		EnableGarbageCollection: true,
		EnableWatchCache:        true,
		DefaultWatchCacheSize:   100,
	}
	options.StorageConfig.CountMetricPollPeriod = time.Minute
	return options
}

var storageMediaTypes = sets.New(
	runtime.ContentTypeJSON,
	runtime.ContentTypeYAML,
	runtime.ContentTypeProtobuf,
)

func (s *EtcdOptions) Validate() []error {
	if s == nil {
		return nil
	}

	allErrors := []error{}
	if len(s.StorageConfig.Transport.ServerList) == 0 {
		allErrors = append(allErrors, fmt.Errorf("--etcd-servers must be specified"))
	}

	if s.StorageConfig.Type != storagebackend.StorageTypeUnset && !storageTypes.Has(s.StorageConfig.Type) {
		allErrors = append(allErrors, fmt.Errorf("--storage-backend invalid, allowed values: %s. If not specified, it will default to 'etcd3'", strings.Join(storageTypes.List(), ", ")))
	}

	for _, override := range s.EtcdServersOverrides {
		tokens := strings.Split(override, "#")
		if len(tokens) != 2 {
			allErrors = append(allErrors, fmt.Errorf("--etcd-servers-overrides invalid, must be of format: group/resource#servers, where servers are URLs, semicolon separated"))
			continue
		}

		apiresource := strings.Split(tokens[0], "/")
		if len(apiresource) != 2 {
			allErrors = append(allErrors, fmt.Errorf("--etcd-servers-overrides invalid, must be of format: group/resource#servers, where servers are URLs, semicolon separated"))
			continue
		}

	}

	if len(s.EncryptionProviderConfigFilepath) == 0 && s.EncryptionProviderConfigAutomaticReload {
		allErrors = append(allErrors, fmt.Errorf("--encryption-provider-config-automatic-reload must be set with --encryption-provider-config"))
	}

	if s.DefaultStorageMediaType != "" && !storageMediaTypes.Has(s.DefaultStorageMediaType) {
		allErrors = append(allErrors, fmt.Errorf("--storage-media-type %q invalid, allowed values: %s", s.DefaultStorageMediaType, strings.Join(sets.List(storageMediaTypes), ", ")))
	}

	return allErrors
}

// AddFlags adds flags related to etcd storage for a specific APIServer to the specified FlagSet
func (s *EtcdOptions) AddFlags(fs *pflag.FlagSet) {
	if s == nil {
		return
	}

	fs.StringSliceVar(&s.EtcdServersOverrides, "etcd-servers-overrides", s.EtcdServersOverrides, ""+
		"Per-resource etcd servers overrides, comma separated. The individual override "+
		"format: group/resource#servers, where servers are URLs, semicolon separated. "+
		"Note that this applies only to resources compiled into this server binary. "+
		`e.g. "/pods#http://etcd4:2379;http://etcd5:2379,/events#http://etcd6:2379"`)

	fs.StringVar(&s.DefaultStorageMediaType, "storage-media-type", s.DefaultStorageMediaType, ""+
		"The media type to use to store objects in storage. "+
		"Some resources or storage backends may only support a specific media type and will ignore this setting. "+
		"Supported media types: [application/json, application/yaml, application/vnd.kubernetes.protobuf]")
	fs.IntVar(&s.DeleteCollectionWorkers, "delete-collection-workers", s.DeleteCollectionWorkers,
		"Number of workers spawned for DeleteCollection call. These are used to speed up namespace cleanup.")

	fs.BoolVar(&s.EnableGarbageCollection, "enable-garbage-collector", s.EnableGarbageCollection, ""+
		"Enables the generic garbage collector. MUST be synced with the corresponding flag "+
		"of the kube-controller-manager.")

	fs.BoolVar(&s.EnableWatchCache, "watch-cache", s.EnableWatchCache,
		"Enable watch caching in the apiserver")

	fs.IntVar(&s.DefaultWatchCacheSize, "default-watch-cache-size", s.DefaultWatchCacheSize,
		"Default watch cache size. If zero, watch cache will be disabled for resources that do not have a default watch size set.")

	fs.MarkDeprecated("default-watch-cache-size",
		"watch caches are sized automatically and this flag will be removed in a future version")

	fs.StringSliceVar(&s.WatchCacheSizes, "watch-cache-sizes", s.WatchCacheSizes, ""+
		"Watch cache size settings for some resources (pods, nodes, etc.), comma separated. "+
		"The individual setting format: resource[.group]#size, where resource is lowercase plural (no version), "+
		"group is omitted for resources of apiVersion v1 (the legacy core API) and included for others, "+
		"and size is a number. This option is only meaningful for resources built into the apiserver, "+
		"not ones defined by CRDs or aggregated from external servers, and is only consulted if the "+
		"watch-cache is enabled. The only meaningful size setting to supply here is zero, which means to "+
		"disable watch caching for the associated resource; all non-zero values are equivalent and mean "+
		"to not disable watch caching for that resource")

	fs.StringVar(&s.StorageConfig.Type, "storage-backend", s.StorageConfig.Type,
		"The storage backend for persistence. Options: 'etcd3' (default).")

	fs.StringSliceVar(&s.StorageConfig.Transport.ServerList, "etcd-servers", s.StorageConfig.Transport.ServerList,
		"List of etcd servers to connect with (scheme://ip:port), comma separated.")

	fs.StringVar(&s.StorageConfig.Prefix, "etcd-prefix", s.StorageConfig.Prefix,
		"The prefix to prepend to all resource paths in etcd.")

	fs.StringVar(&s.StorageConfig.Transport.KeyFile, "etcd-keyfile", s.StorageConfig.Transport.KeyFile,
		"SSL key file used to secure etcd communication.")

	fs.StringVar(&s.StorageConfig.Transport.CertFile, "etcd-certfile", s.StorageConfig.Transport.CertFile,
		"SSL certification file used to secure etcd communication.")

	fs.StringVar(&s.StorageConfig.Transport.TrustedCAFile, "etcd-cafile", s.StorageConfig.Transport.TrustedCAFile,
		"SSL Certificate Authority file used to secure etcd communication.")

	fs.StringVar(&s.EncryptionProviderConfigFilepath, "encryption-provider-config", s.EncryptionProviderConfigFilepath,
		"The file containing configuration for encryption providers to be used for storing secrets in etcd")

	fs.BoolVar(&s.EncryptionProviderConfigAutomaticReload, "encryption-provider-config-automatic-reload", s.EncryptionProviderConfigAutomaticReload,
		"Determines if the file set by --encryption-provider-config should be automatically reloaded if the disk contents change. "+
			"Setting this to true disables the ability to uniquely identify distinct KMS plugins via the API server healthz endpoints.")

	fs.DurationVar(&s.StorageConfig.CompactionInterval, "etcd-compaction-interval", s.StorageConfig.CompactionInterval,
		"The interval of compaction requests. If 0, the compaction request from apiserver is disabled.")

	fs.DurationVar(&s.StorageConfig.CountMetricPollPeriod, "etcd-count-metric-poll-period", s.StorageConfig.CountMetricPollPeriod, ""+
		"Frequency of polling etcd for number of resources per type. 0 disables the metric collection.")

	fs.DurationVar(&s.StorageConfig.DBMetricPollInterval, "etcd-db-metric-poll-interval", s.StorageConfig.DBMetricPollInterval,
		"The interval of requests to poll etcd and update metric. 0 disables the metric collection")

	fs.DurationVar(&s.StorageConfig.HealthcheckTimeout, "etcd-healthcheck-timeout", s.StorageConfig.HealthcheckTimeout,
		"The timeout to use when checking etcd health.")

	fs.DurationVar(&s.StorageConfig.ReadycheckTimeout, "etcd-readycheck-timeout", s.StorageConfig.ReadycheckTimeout,
		"The timeout to use when checking etcd readiness")

	fs.Int64Var(&s.StorageConfig.LeaseManagerConfig.ReuseDurationSeconds, "lease-reuse-duration-seconds", s.StorageConfig.LeaseManagerConfig.ReuseDurationSeconds,
		"The time in seconds that each lease is reused. A lower value could avoid large number of objects reusing the same lease. Notice that a too small value may cause performance problems at storage layer.")
}

// ApplyTo mutates the provided server.Config.  It must never mutate the receiver (EtcdOptions).
func (s *EtcdOptions) ApplyTo(c *server.Config) error {
	if s == nil {
		return nil
	}

	storageConfigCopy := s.StorageConfig
	if storageConfigCopy.StorageObjectCountTracker == nil {
		storageConfigCopy.StorageObjectCountTracker = c.StorageObjectCountTracker
	}

	return s.ApplyWithStorageFactoryTo(&SimpleStorageFactory{StorageConfig: storageConfigCopy}, c)
}

// ApplyWithStorageFactoryTo mutates the provided server.Config.  It must never mutate the receiver (EtcdOptions).
func (s *EtcdOptions) ApplyWithStorageFactoryTo(factory serverstorage.StorageFactory, c *server.Config) error {
	if s == nil {
		return nil
	}

	if !s.SkipHealthEndpoints {
		if err := s.addEtcdHealthEndpoint(c); err != nil {
			return err
		}
	}

	// setup encryption
	if err := s.maybeApplyResourceTransformers(c); err != nil {
		return err
	}

	metrics.SetStorageMonitorGetter(monitorGetter(factory))

	c.RESTOptionsGetter = s.CreateRESTOptionsGetter(factory, c.ResourceTransformers)
	return nil
}

func monitorGetter(factory serverstorage.StorageFactory) func() (monitors []metrics.Monitor, err error) {
	return func() (monitors []metrics.Monitor, err error) {
		defer func() {
			if err != nil {
				for _, m := range monitors {
					m.Close()
				}
			}
		}()

		var m metrics.Monitor
		for _, cfg := range factory.Configs() {
			m, err = storagefactory.CreateMonitor(cfg)
			if err != nil {
				return nil, err
			}
			monitors = append(monitors, m)
		}
		return monitors, nil
	}
}

func (s *EtcdOptions) CreateRESTOptionsGetter(factory serverstorage.StorageFactory, resourceTransformers storagevalue.ResourceTransformers) generic.RESTOptionsGetter {
	if resourceTransformers != nil {
		factory = &transformerStorageFactory{
			delegate:             factory,
			resourceTransformers: resourceTransformers,
		}
	}
	return &StorageFactoryRestOptionsFactory{Options: *s, StorageFactory: factory}
}

func (s *EtcdOptions) maybeApplyResourceTransformers(c *server.Config) (err error) {
	if c.ResourceTransformers != nil {
		return nil
	}
	if len(s.EncryptionProviderConfigFilepath) == 0 {
		return nil
	}

	ctxServer := wait.ContextForChannel(c.DrainedNotify())
	ctxTransformers, closeTransformers := context.WithCancel(ctxServer)
	defer func() {
		// in case of error, we want to close partially initialized (if any) transformers
		if err != nil {
			closeTransformers()
		}
	}()

	encryptionConfiguration, err := encryptionconfig.LoadEncryptionConfig(ctxTransformers, s.EncryptionProviderConfigFilepath, s.EncryptionProviderConfigAutomaticReload, c.APIServerID)
	if err != nil {
		return err
	}

	if s.EncryptionProviderConfigAutomaticReload {
		// with reload=true we will always have 1 health check
		if len(encryptionConfiguration.HealthChecks) != 1 {
			return fmt.Errorf("failed to start kms encryption config hot reload controller. only 1 health check should be available when reload is enabled")
		}

		// Here the dynamic transformers take ownership of the transformers and their cancellation.
		dynamicTransformers := encryptionconfig.NewDynamicTransformers(encryptionConfiguration.Transformers, encryptionConfiguration.HealthChecks[0], closeTransformers, encryptionConfiguration.KMSCloseGracePeriod)

		// add post start hook to start hot reload controller
		// adding this hook here will ensure that it gets configured exactly once
		err = c.AddPostStartHook(
			"start-encryption-provider-config-automatic-reload",
			func(_ server.PostStartHookContext) error {
				dynamicEncryptionConfigController := encryptionconfigcontroller.NewDynamicEncryptionConfiguration(
					"encryption-provider-config-automatic-reload-controller",
					s.EncryptionProviderConfigFilepath,
					dynamicTransformers,
					encryptionConfiguration.EncryptionFileContentHash,
					c.APIServerID,
				)

				go dynamicEncryptionConfigController.Run(ctxServer)

				return nil
			},
		)
		if err != nil {
			return fmt.Errorf("failed to add post start hook for kms encryption config hot reload controller: %w", err)
		}

		c.ResourceTransformers = dynamicTransformers
		if !s.SkipHealthEndpoints {
			addHealthChecksWithoutLivez(c, dynamicTransformers)
		}
	} else {
		c.ResourceTransformers = encryptionconfig.StaticTransformers(encryptionConfiguration.Transformers)
		if !s.SkipHealthEndpoints {
			addHealthChecksWithoutLivez(c, encryptionConfiguration.HealthChecks...)
		}
	}

	return nil
}

func addHealthChecksWithoutLivez(c *server.Config, healthChecks ...healthz.HealthChecker) {
	c.HealthzChecks = append(c.HealthzChecks, healthChecks...)
	c.ReadyzChecks = append(c.ReadyzChecks, healthChecks...)
}

func (s *EtcdOptions) addEtcdHealthEndpoint(c *server.Config) error {
	healthCheck, err := storagefactory.CreateHealthCheck(s.StorageConfig, c.DrainedNotify())
	if err != nil {
		return err
	}
	c.AddHealthChecks(healthz.NamedCheck("etcd", func(r *http.Request) error {
		return healthCheck()
	}))

	readyCheck, err := storagefactory.CreateReadyCheck(s.StorageConfig, c.DrainedNotify())
	if err != nil {
		return err
	}
	c.AddReadyzChecks(healthz.NamedCheck("etcd-readiness", func(r *http.Request) error {
		return readyCheck()
	}))

	return nil
}

type StorageFactoryRestOptionsFactory struct {
	Options        EtcdOptions
	StorageFactory serverstorage.StorageFactory
}

func (f *StorageFactoryRestOptionsFactory) GetRESTOptions(resource schema.GroupResource, example runtime.Object) (generic.RESTOptions, error) {
	storageConfig, err := f.StorageFactory.NewConfig(resource, example)
	if err != nil {
		return generic.RESTOptions{}, fmt.Errorf("unable to find storage destination for %v, due to %v", resource, err.Error())
	}

	ret := generic.RESTOptions{
		StorageConfig:             storageConfig,
		Decorator:                 generic.UndecoratedStorage,
		DeleteCollectionWorkers:   f.Options.DeleteCollectionWorkers,
		EnableGarbageCollection:   f.Options.EnableGarbageCollection,
		ResourcePrefix:            f.StorageFactory.ResourcePrefix(resource),
		CountMetricPollPeriod:     f.Options.StorageConfig.CountMetricPollPeriod,
		StorageObjectCountTracker: f.Options.StorageConfig.StorageObjectCountTracker,
	}

	if ret.StorageObjectCountTracker == nil {
		ret.StorageObjectCountTracker = storageConfig.StorageObjectCountTracker
	}

	if f.Options.EnableWatchCache {
		sizes, err := ParseWatchCacheSizes(f.Options.WatchCacheSizes)
		if err != nil {
			return generic.RESTOptions{}, err
		}
		size, ok := sizes[resource]
		if ok && size > 0 {
			klog.Warningf("Dropping watch-cache-size for %v - watchCache size is now dynamic", resource)
		}
		if ok && size <= 0 {
			klog.V(3).InfoS("Not using watch cache", "resource", resource)
			ret.Decorator = generic.UndecoratedStorage
		} else {
			klog.V(3).InfoS("Using watch cache", "resource", resource)
			ret.Decorator = genericregistry.StorageWithCacher()
		}
	}

	return ret, nil
}

// ParseWatchCacheSizes turns a list of cache size values into a map of group resources
// to requested sizes.
func ParseWatchCacheSizes(cacheSizes []string) (map[schema.GroupResource]int, error) {
	watchCacheSizes := make(map[schema.GroupResource]int)
	for _, c := range cacheSizes {
		tokens := strings.Split(c, "#")
		if len(tokens) != 2 {
			return nil, fmt.Errorf("invalid value of watch cache size: %s", c)
		}

		size, err := strconv.Atoi(tokens[1])
		if err != nil {
			return nil, fmt.Errorf("invalid size of watch cache size: %s", c)
		}
		if size < 0 {
			return nil, fmt.Errorf("watch cache size cannot be negative: %s", c)
		}
		watchCacheSizes[schema.ParseGroupResource(tokens[0])] = size
	}
	return watchCacheSizes, nil
}

// WriteWatchCacheSizes turns a map of cache size values into a list of string specifications.
func WriteWatchCacheSizes(watchCacheSizes map[schema.GroupResource]int) ([]string, error) {
	var cacheSizes []string

	for resource, size := range watchCacheSizes {
		if size < 0 {
			return nil, fmt.Errorf("watch cache size cannot be negative for resource %s", resource)
		}
		cacheSizes = append(cacheSizes, fmt.Sprintf("%s#%d", resource.String(), size))
	}
	return cacheSizes, nil
}

var _ serverstorage.StorageFactory = &SimpleStorageFactory{}

// SimpleStorageFactory provides a StorageFactory implementation that should be used when different
// resources essentially share the same storage config (as defined by the given storagebackend.Config).
// It assumes the resources are stored at a path that is purely based on the schema.GroupResource.
// Users that need flexibility and per resource overrides should use DefaultStorageFactory instead.
type SimpleStorageFactory struct {
	StorageConfig storagebackend.Config
}

func (s *SimpleStorageFactory) NewConfig(resource schema.GroupResource, example runtime.Object) (*storagebackend.ConfigForResource, error) {
	return s.StorageConfig.ForResource(resource), nil
}

func (s *SimpleStorageFactory) ResourcePrefix(resource schema.GroupResource) string {
	return resource.Group + "/" + resource.Resource
}

func (s *SimpleStorageFactory) Configs() []storagebackend.Config {
	return serverstorage.Configs(s.StorageConfig)
}

func (s *SimpleStorageFactory) Backends() []serverstorage.Backend {
	// nothing should ever call this method but we still provide a functional implementation
	return serverstorage.Backends(s.StorageConfig)
}

var _ serverstorage.StorageFactory = &transformerStorageFactory{}

type transformerStorageFactory struct {
	delegate             serverstorage.StorageFactory
	resourceTransformers storagevalue.ResourceTransformers
}

func (t *transformerStorageFactory) NewConfig(resource schema.GroupResource, example runtime.Object) (*storagebackend.ConfigForResource, error) {
	config, err := t.delegate.NewConfig(resource, example)
	if err != nil {
		return nil, err
	}

	configCopy := *config
	resourceConfig := configCopy.Config
	resourceConfig.Transformer = t.resourceTransformers.TransformerForResource(resource)
	configCopy.Config = resourceConfig

	return &configCopy, nil
}

func (t *transformerStorageFactory) ResourcePrefix(resource schema.GroupResource) string {
	return t.delegate.ResourcePrefix(resource)
}

func (t *transformerStorageFactory) Configs() []storagebackend.Config {
	return t.delegate.Configs()
}

func (t *transformerStorageFactory) Backends() []serverstorage.Backend {
	return t.delegate.Backends()
}

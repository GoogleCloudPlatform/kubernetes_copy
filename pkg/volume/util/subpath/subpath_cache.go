package subpath

import (
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

var (
	cache *subpathCache
)

func init() {
	cache = &subpathCache{
		cache: make(map[string]struct{}),
	}
}

type subpathCache struct {
	mutex sync.Mutex
	cache map[string]struct{}
}

func addSubpathToCache(key string) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.cache[key] = struct{}{}
}

func removeSubpathByPodDir(podVolumeDir string) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	for k := range cache.cache {
		if strings.HasPrefix(k, podVolumeDir) {
			klog.Infof("Removing subpath %q from cache", k)
			delete(cache.cache, k)
		}
	}
}

// HasSubpath checks if the given subpath oldTSDir
// (like /var/lib/kubelet/pods/${PODUID}/volumes/kubernetes.io~projected/${volumeName}/..2024_07_20_08_33_38.4283786363/data)
// is in the cache
func HasSubpath(key string) bool {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	_, ok := cache.cache[key]
	return ok
}

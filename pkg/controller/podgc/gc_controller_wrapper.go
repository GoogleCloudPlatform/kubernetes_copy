package podgc

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
)

var (
	// GCSortByFinishTime Sort GC Pod List By Container Finish Time.
	GCSortByFinishTime featuregate.Feature = "PodGCSortByFinishTime"
)

func init() {
	runtime.Must(utilfeature.DefaultMutableFeatureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		GCSortByFinishTime: {Default: false, PreRelease: featuregate.Beta},
	}))
}

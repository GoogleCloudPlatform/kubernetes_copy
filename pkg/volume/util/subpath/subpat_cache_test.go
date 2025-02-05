package subpath

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/uuid"
)

func TestHasSubpath(t *testing.T) {
	podUid1 := uuid.NewUUID()
	podUid2 := uuid.NewUUID()
	podUid3 := uuid.NewUUID()
	volume1 := fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~projected/..2024_07_20_08_33_38.4283786363/secret/cert", podUid1)
	volume2 := fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~projected/..2024_07_20_08_33_38.4283786363/secret/data", podUid2)
	volume3 := fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes/kubernetes.io~projected/..2024_07_20_08_33_38.4283786363/secret/other", podUid3)

	addSubpathToCache(volume1)
	addSubpathToCache(volume2)

	assert.Equal(t, true, HasSubpath(volume1))
	assert.Equal(t, true, HasSubpath(volume2))
	assert.Equal(t, false, HasSubpath(volume3))

	removeSubpathByPodDir(fmt.Sprintf("/var/lib/kubelet/pods/%s/volumes", podUid1))

	assert.Equal(t, false, HasSubpath(volume1))
	assert.Equal(t, true, HasSubpath(volume2))
}

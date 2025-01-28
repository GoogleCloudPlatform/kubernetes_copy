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

package plugin

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddSameName(t *testing.T) {
	// name will have a random value to avoid conflicts
	pluginName := fmt.Sprintf("dummy-plugin-%d", rand.IntN(10000))

	firstWasCancelled := false
	p := &Plugin{
		name:     pluginName,
		endpoint: "old",
		cancel:   func(err error) { firstWasCancelled = true },
	}

	// ensure the plugin we are using is registered
	require.NoError(t, draPlugins.add(p))
	defer draPlugins.remove(p.name, p.endpoint)

	assert.False(t, firstWasCancelled, "should not cancel context after the first call")

	// Same name, same endpoint -> error.
	require.Error(t, draPlugins.add(p))

	secondWasCancelled := false
	p2 := &Plugin{
		name:     pluginName,
		endpoint: "new",
		cancel:   func(err error) { secondWasCancelled = true },
	}
	require.NoError(t, draPlugins.add(p2))
	defer draPlugins.remove(p2.name, p2.endpoint)

	assert.False(t, firstWasCancelled, "should not cancel context after registering the second instance")
	assert.False(t, secondWasCancelled, "should not cancel context of a new plugin")

	// Remove old plugin.
	draPlugins.remove(p.name, p.endpoint)
	assert.True(t, firstWasCancelled, "should have canceled context after the explicit removal")
	assert.False(t, secondWasCancelled, "should not cancel context of a new plugin")
}

func TestDelete(t *testing.T) {
	pluginName := fmt.Sprintf("dummy-plugin-%d", rand.IntN(10000))

	wasCancelled := false
	p := &Plugin{
		name:   pluginName,
		cancel: func(err error) { wasCancelled = true },
	}

	// ensure the plugin we are using is registered
	draPlugins.add(p)

	draPlugins.remove(p.name, "")

	assert.True(t, wasCancelled, "should cancel context after the second call")
}

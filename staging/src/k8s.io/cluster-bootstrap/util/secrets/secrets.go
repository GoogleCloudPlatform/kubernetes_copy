/*
Copyright 2019 The Kubernetes Authors.

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

package secrets

import (
	"regexp"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cluster-bootstrap/token/api"
	legacyutil "k8s.io/cluster-bootstrap/token/util"
	"k8s.io/klog/v2"
)

var (
	secretNameRe = regexp.MustCompile(`^` + regexp.QuoteMeta(api.BootstrapTokenSecretPrefix) + `([a-z0-9]{6})$`)
)

// GetData returns the string value for the given key in the specified Secret
// If there is an error or if the key doesn't exist, an empty string is returned.
func GetData(secret *v1.Secret, key string) string {
	if secret.Data == nil {
		return ""
	}
	if val, ok := secret.Data[key]; ok {
		return string(val)
	}
	return ""
}

// HasExpired will identify whether the secret expires
func HasExpired(secret *v1.Secret, currentTime time.Time) bool {
	_, expired := GetExpiration(secret, currentTime)

	return expired
}

// GetExpiration checks if the secret expires
// isExpired indicates if the secret is already expired.
// timeRemaining indicates how long until it does expire.
// if the secret has no expiration timestamp, returns 0, false.
// if there is an error parsing the secret's expiration timestamp, returns 0, true.
func GetExpiration(secret *v1.Secret, currentTime time.Time) (timeRemaining time.Duration, isExpired bool) {
	expiration := GetData(secret, api.BootstrapTokenExpirationKey)
	if len(expiration) == 0 {
		return 0, false
	}
	expTime, err := time.Parse(time.RFC3339, expiration)
	if err != nil {
		klog.V(3).InfoS("Unparseable expiration time",
			"expirationTime", expiration, "secretNamespace", secret.Namespace, "secretName", secret.Name, "error", err)
		return 0, true
	}

	timeRemaining = expTime.Sub(currentTime)
	if timeRemaining <= 0 {
		klog.V(3).InfoS("Expired bootstrap token",
			"secretNamespace", secret.Namespace, "secretName", secret.Name, "expirationTime", expiration)
		return 0, true
	}
	return timeRemaining, false
}

// ParseName parses the name of the secret to extract the secret ID.
func ParseName(name string) (secretID string, ok bool) {
	r := secretNameRe.FindStringSubmatch(name)
	if r == nil {
		return "", false
	}
	return r[1], true
}

// GetGroups loads and validates the bootstrapapi.BootstrapTokenExtraGroupsKey
// key from the bootstrap token secret, returning a list of group names or an
// error if any of the group names are invalid.
func GetGroups(secret *v1.Secret) ([]string, error) {
	// always include the default group
	groups := sets.NewString(api.BootstrapDefaultGroup)

	// grab any extra groups and if there are none, return just the default
	extraGroupsString := GetData(secret, api.BootstrapTokenExtraGroupsKey)
	if extraGroupsString == "" {
		return groups.List(), nil
	}

	// validate the names of the extra groups
	for _, group := range strings.Split(extraGroupsString, ",") {
		if err := legacyutil.ValidateBootstrapGroupName(group); err != nil {
			return nil, err
		}
		groups.Insert(group)
	}

	// return the result as a deduplicated, sorted list
	return groups.List(), nil
}

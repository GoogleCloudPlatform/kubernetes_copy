/*
Copyright 2022 The Kubernetes Authors.

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

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"gopkg.in/go-jose/go-jose.v2/jwt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	credentialproviderv1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"
)

const (
	metadataTokenEndpoint = "http://metadata.google.internal./computeMetadata/v1/instance/service-accounts/default/token"

	pluginModeEnvVar = "PLUGIN_MODE"
)

func main() {
	if err := getCredentials(metadataTokenEndpoint, os.Stdin, os.Stdout); err != nil {
		klog.Fatalf("failed to get credentials: %v", err)
	}
}

func getCredentials(tokenEndpoint string, r io.Reader, w io.Writer) error {
	provider := &provider{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		tokenEndpoint: tokenEndpoint,
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	var authRequest credentialproviderv1.CredentialProviderRequest
	err = json.Unmarshal(data, &authRequest)
	if err != nil {
		return err
	}

	pluginUsingServiceAccount := os.Getenv(pluginModeEnvVar) == "serviceaccount"
	if pluginUsingServiceAccount {
		if len(authRequest.ServiceAccountToken) == 0 {
			return errors.New("service account token is empty")
		}
		expectedAnnotations := map[string]string{
			"domain.io/identity-id":   "123456",
			"domain.io/identity-type": "serviceaccount",
		}
		if !reflect.DeepEqual(authRequest.ServiceAccountAnnotations, expectedAnnotations) {
			return fmt.Errorf("unexpected service account annotations, want: %v, got: %v", expectedAnnotations, authRequest.ServiceAccountAnnotations)
		}
		// The service account token is not actually used for authentication by this test plugin.
		// We extract the claims from the token to validate the audience.
		// This is solely for testing assertions and is not an actual security layer.
		// Post validation in this block, we proceed with the default flow for fetching credentials.
		c, err := getClaims(authRequest.ServiceAccountToken)
		if err != nil {
			return err
		}
		// The audience in the token should match the audience configured in tokenAttributes.serviceAccountTokenAudience
		// in CredentialProviderConfig.
		if len(c.Audience) != 1 || c.Audience[0] != "test-audience" {
			return fmt.Errorf("unexpected audience: %v", c.Audience)
		}
	} else {
		if len(authRequest.ServiceAccountToken) > 0 {
			return errors.New("service account token is not expected")
		}
		if len(authRequest.ServiceAccountAnnotations) > 0 {
			return errors.New("service account annotations are not expected")
		}
	}

	auth, err := provider.Provide(authRequest.Image)
	if err != nil {
		return err
	}

	response := &credentialproviderv1.CredentialProviderResponse{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CredentialProviderResponse",
			APIVersion: "credentialprovider.kubelet.k8s.io/v1",
		},
		CacheKeyType: credentialproviderv1.RegistryPluginCacheKeyType,
		Auth:         auth,
	}

	if pluginUsingServiceAccount {
		response.CacheKeyType = credentialproviderv1.GlobalPluginCacheKeyType
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		// The error from json.Marshal is intentionally not included so as to not leak credentials into the logs
		return errors.New("error marshaling response")
	}

	return nil
}

// getClaims is used to extract claims from the service account token when the plugin is running in service account mode
// This is solely for testing assertions and is not an actual security layer.
// We get claims and validate the audience of the token (audience in the token matches the audience configured
// in tokenAttributes.serviceAccountTokenAudience in CredentialProviderConfig).
func getClaims(tokenData string) (claims, error) {
	if strings.HasPrefix(strings.TrimSpace(tokenData), "{") {
		return claims{}, errors.New("token is not a JWS")
	}
	parts := strings.Split(tokenData, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("token is not a JWS")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims{}, fmt.Errorf("error decoding token payload: %w", err)
	}

	var c claims
	d := json.NewDecoder(strings.NewReader(string(payload)))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return claims{}, fmt.Errorf("error decoding token payload: %w", err)
	}

	return c, nil
}

type claims struct {
	jwt.Claims
	privateClaims
}

// copied from https://github.com/kubernetes/kubernetes/blob/60c4c2b2521fb454ce69dee737e3eb91a25e0535/pkg/serviceaccount/claims.go#L51-L67

type privateClaims struct {
	Kubernetes kubernetes `json:"kubernetes.io,omitempty"`
}

type kubernetes struct {
	Namespace string           `json:"namespace,omitempty"`
	Svcacct   ref              `json:"serviceaccount,omitempty"`
	Pod       *ref             `json:"pod,omitempty"`
	Secret    *ref             `json:"secret,omitempty"`
	Node      *ref             `json:"node,omitempty"`
	WarnAfter *jwt.NumericDate `json:"warnafter,omitempty"`
}

type ref struct {
	Name string `json:"name,omitempty"`
	UID  string `json:"uid,omitempty"`
}

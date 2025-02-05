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

package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	//
	// Uncomment to load all auth plugins
	//_ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

func main() {
	token := flag.String("token", "-", "the JWT token to verify (defaults to stdin)")
	audience := flag.String("target-audience", "https://kubernetes.default.svc.cluster.local", "the audience to verify tokens against (passed in the TokenReview)")
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()
	if *token == "-" {
		tokenBytes, err := io.ReadAll(os.Stdin)
		if err != nil {
			panic(err)
		}
		tokenStr := string(tokenBytes)
		token = &tokenStr
	}

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	review, err := clientset.AuthenticationV1().TokenReviews().Create(context.TODO(), &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     *token,
			Audiences: []string{*audience},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		panic(err)
	}

	if !review.Status.Authenticated {
		log.Printf("Token was NOT authenticated by the apiserver due to: %v", review.Status.Error)
	} else {
		log.Printf("Token was successfully authenticated with user=%#v, audiences from token=%#v, ", review.Status.User, review.Status.Audiences)
	}
}

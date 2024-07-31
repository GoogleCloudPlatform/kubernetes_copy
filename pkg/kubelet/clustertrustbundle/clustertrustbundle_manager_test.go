/*
Copyright 2023 The Kubernetes Authors.

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

package clustertrustbundle

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func TestBeforeSynced(t *testing.T) {
	tCtx := ktesting.Init(t)
	kc := fake.NewSimpleClientset()

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kc, 0)

	ctbInformer := informerFactory.Certificates().V1beta1().ClusterTrustBundles()
	ctbManager, _ := NewInformerManager(tCtx, ctbInformer, 256, 5*time.Minute)

	_, err := ctbManager.GetTrustAnchorsByName("foo", false)
	if err == nil {
		t.Fatalf("Got nil error, wanted non-nil")
	}
}

func TestGetTrustAnchorsByName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	tCtx := ktesting.Init(t)
	defer cancel()

	ctb1 := &certificatesv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ctb1",
		},
		Spec: certificatesv1beta1.ClusterTrustBundleSpec{
			TrustBundle: mustMakeRoot(t, "root1"),
		},
	}

	ctb2 := &certificatesv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ctb2",
		},
		Spec: certificatesv1beta1.ClusterTrustBundleSpec{
			TrustBundle: mustMakeRoot(t, "root2"),
		},
	}

	kc := fake.NewSimpleClientset(ctb1, ctb2)

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kc, 0)

	ctbInformer := informerFactory.Certificates().V1beta1().ClusterTrustBundles()
	ctbManager, _ := NewInformerManager(tCtx, ctbInformer, 256, 5*time.Minute)

	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), ctbInformer.Informer().HasSynced) {
		t.Fatalf("Timed out waiting for informer to sync")
	}

	gotBundle, err := ctbManager.GetTrustAnchorsByName("ctb1", false)
	if err != nil {
		t.Fatalf("Error while calling GetTrustAnchorsByName: %v", err)
	}

	if diff := diffBundles(gotBundle, []byte(ctb1.Spec.TrustBundle)); diff != "" {
		t.Fatalf("Got bad bundle; diff (-got +want)\n%s", diff)
	}

	gotBundle, err = ctbManager.GetTrustAnchorsByName("ctb2", false)
	if err != nil {
		t.Fatalf("Error while calling GetTrustAnchorsByName: %v", err)
	}

	if diff := diffBundles(gotBundle, []byte(ctb2.Spec.TrustBundle)); diff != "" {
		t.Fatalf("Got bad bundle; diff (-got +want)\n%s", diff)
	}

	_, err = ctbManager.GetTrustAnchorsByName("not-found", false)
	if err == nil { // EQUALS nil
		t.Fatalf("While looking up nonexisting ClusterTrustBundle, got nil error, wanted non-nil")
	}

	_, err = ctbManager.GetTrustAnchorsByName("not-found", true)
	if err != nil {
		t.Fatalf("Unexpected error while calling GetTrustAnchorsByName for nonexistent CTB with allowMissing: %v", err)
	}
}

func TestGetTrustAnchorsByNameCaching(t *testing.T) {
	tCtx := ktesting.Init(t)
	ctx, cancel := context.WithTimeout(tCtx, 20*time.Second)
	defer cancel()

	ctb1 := &certificatesv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: certificatesv1beta1.ClusterTrustBundleSpec{
			TrustBundle: mustMakeRoot(t, "root1"),
		},
	}

	ctb2 := &certificatesv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Spec: certificatesv1beta1.ClusterTrustBundleSpec{
			TrustBundle: mustMakeRoot(t, "root2"),
		},
	}

	kc := fake.NewSimpleClientset(ctb1)

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kc, 0)

	ctbInformer := informerFactory.Certificates().V1beta1().ClusterTrustBundles()
	ctbManager, _ := NewInformerManager(tCtx, ctbInformer, 256, 5*time.Minute)

	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), ctbInformer.Informer().HasSynced) {
		t.Fatalf("Timed out waiting for informer to sync")
	}

	t.Run("foo should yield the first certificate", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsByName("foo", false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ctb1.Spec.TrustBundle

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("foo should still yield the first certificate", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsByName("foo", false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ctb1.Spec.TrustBundle

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	if err := kc.CertificatesV1beta1().ClusterTrustBundles().Delete(ctx, ctb1.ObjectMeta.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Error while deleting the old CTB: %v", err)
	}
	if _, err := kc.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, ctb2, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error while adding new CTB: %v", err)
	}

	// We need to sleep long enough for the informer to notice the new
	// ClusterTrustBundle, but much less than the 5 minutes of the cache TTL.
	// This shows us that the informer is properly clearing the cache.
	time.Sleep(5 * time.Second)

	t.Run("foo should yield the new certificate", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsByName("foo", false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ctb2.Spec.TrustBundle

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})
}

func TestGetTrustAnchorsBySignerName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	tCtx := ktesting.Init(t)
	defer cancel()

	ctb1 := mustMakeCTB("signer-a-label-a-1", "foo.bar/a", map[string]string{"label": "a"}, mustMakeRoot(t, "0"))
	ctb2 := mustMakeCTB("signer-a-label-a-2", "foo.bar/a", map[string]string{"label": "a"}, mustMakeRoot(t, "1"))
	ctb2dup := mustMakeCTB("signer-a-label-2-dup", "foo.bar/a", map[string]string{"label": "a"}, ctb2.Spec.TrustBundle)
	ctb3 := mustMakeCTB("signer-a-label-b-1", "foo.bar/a", map[string]string{"label": "b"}, mustMakeRoot(t, "2"))
	ctb4 := mustMakeCTB("signer-b-label-a-1", "foo.bar/b", map[string]string{"label": "a"}, mustMakeRoot(t, "3"))

	kc := fake.NewSimpleClientset(ctb1, ctb2, ctb2dup, ctb3, ctb4)

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kc, 0)

	ctbInformer := informerFactory.Certificates().V1beta1().ClusterTrustBundles()
	ctbManager, _ := NewInformerManager(tCtx, ctbInformer, 256, 5*time.Minute)

	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), ctbInformer.Informer().HasSynced) {
		t.Fatalf("Timed out waiting for informer to sync")
	}

	t.Run("big labelselector should cause error", func(t *testing.T) {
		longString := strings.Builder{}
		for i := 0; i < 63; i++ {
			longString.WriteString("v")
		}
		matchLabels := map[string]string{}
		for i := 0; i < 100*1024/63+1; i++ {
			matchLabels[fmt.Sprintf("key-%d", i)] = longString.String()
		}

		_, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/a", &metav1.LabelSelector{MatchLabels: matchLabels}, false)
		if err == nil || !strings.Contains(err.Error(), "label selector length") {
			t.Fatalf("Bad error, got %v, wanted it to contain \"label selector length\"", err)
		}
	})

	t.Run("signer-a label-a should yield two sorted certificates", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/a", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "a"}}, false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ctb1.Spec.TrustBundle + ctb2.Spec.TrustBundle

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("signer-a with nil selector should yield zero certificates", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/a", nil, true)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ""

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("signer-b with empty selector should yield one certificates", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/b", &metav1.LabelSelector{}, false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		if diff := diffBundles(gotBundle, []byte(ctb4.Spec.TrustBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("signer-a label-b should yield one certificate", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/a", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "b"}}, false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		if diff := diffBundles(gotBundle, []byte(ctb3.Spec.TrustBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("signer-b label-a should yield one certificate", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/b", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "a"}}, false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		if diff := diffBundles(gotBundle, []byte(ctb4.Spec.TrustBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("signer-b label-b allowMissing=true should yield zero certificates", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/b", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "b"}}, true)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		if diff := diffBundles(gotBundle, []byte{}); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("signer-b label-b allowMissing=false should yield zero certificates (error)", func(t *testing.T) {
		_, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/b", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "b"}}, false)
		if err == nil { // EQUALS nil
			t.Fatalf("Got nil error while calling GetTrustAnchorsBySigner, wanted non-nil")
		}
	})
}

func TestGetTrustAnchorsBySignerNameCaching(t *testing.T) {
	tCtx := ktesting.Init(t)
	ctx, cancel := context.WithTimeout(tCtx, 20*time.Second)
	defer cancel()

	ctb1 := mustMakeCTB("signer-a-label-a-1", "foo.bar/a", map[string]string{"label": "a"}, mustMakeRoot(t, "0"))
	ctb2 := mustMakeCTB("signer-a-label-a-2", "foo.bar/a", map[string]string{"label": "a"}, mustMakeRoot(t, "1"))

	kc := fake.NewSimpleClientset(ctb1)

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kc, 0)

	ctbInformer := informerFactory.Certificates().V1beta1().ClusterTrustBundles()
	ctbManager, _ := NewInformerManager(tCtx, ctbInformer, 256, 5*time.Minute)

	informerFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), ctbInformer.Informer().HasSynced) {
		t.Fatalf("Timed out waiting for informer to sync")
	}

	t.Run("signer-a label-a should yield one certificate", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/a", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "a"}}, false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ctb1.Spec.TrustBundle

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	t.Run("signer-a label-a should yield the same result when called again", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/a", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "a"}}, false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ctb1.Spec.TrustBundle

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})

	if err := kc.CertificatesV1beta1().ClusterTrustBundles().Delete(ctx, ctb1.ObjectMeta.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Error while deleting the old CTB: %v", err)
	}
	if _, err := kc.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, ctb2, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Error while adding new CTB: %v", err)
	}

	// We need to sleep long enough for the informer to notice the new
	// ClusterTrustBundle, but much less than the 5 minutes of the cache TTL.
	// This shows us that the informer is properly clearing the cache.
	time.Sleep(5 * time.Second)

	t.Run("signer-a label-a should return the new certificate", func(t *testing.T) {
		gotBundle, err := ctbManager.GetTrustAnchorsBySigner("foo.bar/a", &metav1.LabelSelector{MatchLabels: map[string]string{"label": "a"}}, false)
		if err != nil {
			t.Fatalf("Got error while calling GetTrustAnchorsBySigner: %v", err)
		}

		wantBundle := ctb2.Spec.TrustBundle

		if diff := diffBundles(gotBundle, []byte(wantBundle)); diff != "" {
			t.Fatalf("Bad bundle; diff (-got +want)\n%s", diff)
		}
	})
}

func mustMakeRoot(t *testing.T, cn string) string {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("Error while generating key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject: pkix.Name{
			CommonName: cn,
		},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	cert, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatalf("Error while making certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{
		Type:    "CERTIFICATE",
		Headers: nil,
		Bytes:   cert,
	}))
}

func mustMakeCTB(name, signerName string, labels map[string]string, bundle string) *certificatesv1beta1.ClusterTrustBundle {
	return &certificatesv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: certificatesv1beta1.ClusterTrustBundleSpec{
			SignerName:  signerName,
			TrustBundle: bundle,
		},
	}
}

func diffBundles(a, b []byte) string {
	var block *pem.Block

	aBlocks := []*pem.Block{}
	for {
		block, a = pem.Decode(a)
		if block == nil {
			break
		}
		aBlocks = append(aBlocks, block)
	}
	sort.Slice(aBlocks, func(i, j int) bool {
		if aBlocks[i].Type < aBlocks[j].Type {
			return true
		} else if aBlocks[i].Type == aBlocks[j].Type {
			comp := bytes.Compare(aBlocks[i].Bytes, aBlocks[j].Bytes)
			return comp <= 0
		} else {
			return false
		}
	})

	bBlocks := []*pem.Block{}
	for {
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		bBlocks = append(bBlocks, block)
	}
	sort.Slice(bBlocks, func(i, j int) bool {
		if bBlocks[i].Type < bBlocks[j].Type {
			return true
		} else if bBlocks[i].Type == bBlocks[j].Type {
			comp := bytes.Compare(bBlocks[i].Bytes, bBlocks[j].Bytes)
			return comp <= 0
		} else {
			return false
		}
	})

	return cmp.Diff(aBlocks, bBlocks)
}

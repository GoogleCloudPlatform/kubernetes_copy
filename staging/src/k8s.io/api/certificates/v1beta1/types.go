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

package v1beta1

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.12
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=certificates.k8s.io,v1,CertificateSigningRequest

// Describes a certificate signing request
type CertificateSigningRequest struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec contains the certificate request, and is immutable after creation.
	// Only the request, signerName, expirationSeconds, and usages fields can be set on creation.
	// Other fields are derived by Kubernetes and cannot be modified by users.
	Spec CertificateSigningRequestSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`

	// Derived information about the request.
	// +optional
	Status CertificateSigningRequestStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// CertificateSigningRequestSpec contains the certificate request.
type CertificateSigningRequestSpec struct {
	// Base64-encoded PKCS#10 CSR data
	// +listType=atomic
	Request []byte `json:"request" protobuf:"bytes,1,opt,name=request"`

	// Requested signer for the request. It is a qualified name in the form:
	// `scope-hostname.io/name`.
	// If empty, it will be defaulted:
	//  1. If it's a kubelet client certificate, it is assigned
	//     "kubernetes.io/kube-apiserver-client-kubelet".
	//  2. If it's a kubelet serving certificate, it is assigned
	//     "kubernetes.io/kubelet-serving".
	//  3. Otherwise, it is assigned "kubernetes.io/legacy-unknown".
	// Distribution of trust for signers happens out of band.
	// You can select on this field using `spec.signerName`.
	// +optional
	SignerName *string `json:"signerName,omitempty" protobuf:"bytes,7,opt,name=signerName"`

	// expirationSeconds is the requested duration of validity of the issued
	// certificate. The certificate signer may issue a certificate with a different
	// validity duration so a client must check the delta between the notBefore and
	// and notAfter fields in the issued certificate to determine the actual duration.
	//
	// The v1.22+ in-tree implementations of the well-known Kubernetes signers will
	// honor this field as long as the requested duration is not greater than the
	// maximum duration they will honor per the --cluster-signing-duration CLI
	// flag to the Kubernetes controller manager.
	//
	// Certificate signers may not honor this field for various reasons:
	//
	//   1. Old signer that is unaware of the field (such as the in-tree
	//      implementations prior to v1.22)
	//   2. Signer whose configured maximum is shorter than the requested duration
	//   3. Signer whose configured minimum is longer than the requested duration
	//
	// The minimum valid value for expirationSeconds is 600, i.e. 10 minutes.
	//
	// +optional
	ExpirationSeconds *int32 `json:"expirationSeconds,omitempty" protobuf:"varint,8,opt,name=expirationSeconds"`

	// allowedUsages specifies a set of usage contexts the key will be
	// valid for.
	// See:
	//	https://tools.ietf.org/html/rfc5280#section-4.2.1.3
	//	https://tools.ietf.org/html/rfc5280#section-4.2.1.12
	//
	// Valid values are:
	//  "signing",
	//  "digital signature",
	//  "content commitment",
	//  "key encipherment",
	//  "key agreement",
	//  "data encipherment",
	//  "cert sign",
	//  "crl sign",
	//  "encipher only",
	//  "decipher only",
	//  "any",
	//  "server auth",
	//  "client auth",
	//  "code signing",
	//  "email protection",
	//  "s/mime",
	//  "ipsec end system",
	//  "ipsec tunnel",
	//  "ipsec user",
	//  "timestamping",
	//  "ocsp signing",
	//  "microsoft sgc",
	//  "netscape sgc"
	// +listType=atomic
	Usages []KeyUsage `json:"usages,omitempty" protobuf:"bytes,5,opt,name=usages"`

	// Information about the requesting user.
	// See user.Info interface for details.
	// +optional
	Username string `json:"username,omitempty" protobuf:"bytes,2,opt,name=username"`
	// UID information about the requesting user.
	// See user.Info interface for details.
	// +optional
	UID string `json:"uid,omitempty" protobuf:"bytes,3,opt,name=uid"`
	// Group information about the requesting user.
	// See user.Info interface for details.
	// +listType=atomic
	// +optional
	Groups []string `json:"groups,omitempty" protobuf:"bytes,4,rep,name=groups"`
	// Extra information about the requesting user.
	// See user.Info interface for details.
	// +optional
	Extra map[string]ExtraValue `json:"extra,omitempty" protobuf:"bytes,6,rep,name=extra"`
}

// Built in signerName values that are honoured by kube-controller-manager.
// None of these usages are related to ServiceAccount token secrets
// `.data[ca.crt]` in any way.
const (
	// Signs certificates that will be honored as client-certs by the
	// kube-apiserver. Never auto-approved by kube-controller-manager.
	KubeAPIServerClientSignerName = "kubernetes.io/kube-apiserver-client"

	// Signs client certificates that will be honored as client-certs by the
	// kube-apiserver for a kubelet.
	// May be auto-approved by kube-controller-manager.
	KubeAPIServerClientKubeletSignerName = "kubernetes.io/kube-apiserver-client-kubelet"

	// Signs serving certificates that are honored as a valid kubelet serving
	// certificate by the kube-apiserver, but has no other guarantees.
	KubeletServingSignerName = "kubernetes.io/kubelet-serving"

	// Has no guarantees for trust at all. Some distributions may honor these
	// as client certs, but that behavior is not standard kubernetes behavior.
	LegacyUnknownSignerName = "kubernetes.io/legacy-unknown"
)

// ExtraValue masks the value so protobuf can generate
// +protobuf.nullable=true
// +protobuf.options.(gogoproto.goproto_stringer)=false
type ExtraValue []string

func (t ExtraValue) String() string {
	return fmt.Sprintf("%v", []string(t))
}

type CertificateSigningRequestStatus struct {
	// Conditions applied to the request, such as approval or denial.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []CertificateSigningRequestCondition `json:"conditions,omitempty" protobuf:"bytes,1,rep,name=conditions"`

	// If request was approved, the controller will place the issued certificate here.
	// +listType=atomic
	// +optional
	Certificate []byte `json:"certificate,omitempty" protobuf:"bytes,2,opt,name=certificate"`
}

type RequestConditionType string

// These are the possible conditions for a certificate request.
const (
	CertificateApproved RequestConditionType = "Approved"
	CertificateDenied   RequestConditionType = "Denied"
	CertificateFailed   RequestConditionType = "Failed"
)

type CertificateSigningRequestCondition struct {
	// type of the condition. Known conditions include "Approved", "Denied", and "Failed".
	Type RequestConditionType `json:"type" protobuf:"bytes,1,opt,name=type,casttype=RequestConditionType"`
	// Status of the condition, one of True, False, Unknown.
	// Approved, Denied, and Failed conditions may not be "False" or "Unknown".
	// Defaults to "True".
	// If unset, should be treated as "True".
	// +optional
	Status v1.ConditionStatus `json:"status" protobuf:"bytes,6,opt,name=status,casttype=k8s.io/api/core/v1.ConditionStatus"`
	// brief reason for the request state
	// +optional
	Reason string `json:"reason,omitempty" protobuf:"bytes,2,opt,name=reason"`
	// human readable message with details about the request state
	// +optional
	Message string `json:"message,omitempty" protobuf:"bytes,3,opt,name=message"`
	// timestamp for the last update to this condition
	// +optional
	LastUpdateTime metav1.Time `json:"lastUpdateTime,omitempty" protobuf:"bytes,4,opt,name=lastUpdateTime"`
	// lastTransitionTime is the time the condition last transitioned from one status to another.
	// If unset, when a new condition type is added or an existing condition's status is changed,
	// the server defaults this to the current time.
	// +optional
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty" protobuf:"bytes,5,opt,name=lastTransitionTime"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.12
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=certificates.k8s.io,v1,CertificateSigningRequestList

type CertificateSigningRequestList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	Items []CertificateSigningRequest `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// KeyUsages specifies valid usage contexts for keys.
// See:
//
//	https://tools.ietf.org/html/rfc5280#section-4.2.1.3
//	https://tools.ietf.org/html/rfc5280#section-4.2.1.12
type KeyUsage string

const (
	UsageSigning           KeyUsage = "signing"
	UsageDigitalSignature  KeyUsage = "digital signature"
	UsageContentCommitment KeyUsage = "content commitment"
	UsageKeyEncipherment   KeyUsage = "key encipherment"
	UsageKeyAgreement      KeyUsage = "key agreement"
	UsageDataEncipherment  KeyUsage = "data encipherment"
	UsageCertSign          KeyUsage = "cert sign"
	UsageCRLSign           KeyUsage = "crl sign"
	UsageEncipherOnly      KeyUsage = "encipher only"
	UsageDecipherOnly      KeyUsage = "decipher only"
	UsageAny               KeyUsage = "any"
	UsageServerAuth        KeyUsage = "server auth"
	UsageClientAuth        KeyUsage = "client auth"
	UsageCodeSigning       KeyUsage = "code signing"
	UsageEmailProtection   KeyUsage = "email protection"
	UsageSMIME             KeyUsage = "s/mime"
	UsageIPsecEndSystem    KeyUsage = "ipsec end system"
	UsageIPsecTunnel       KeyUsage = "ipsec tunnel"
	UsageIPsecUser         KeyUsage = "ipsec user"
	UsageTimestamping      KeyUsage = "timestamping"
	UsageOCSPSigning       KeyUsage = "ocsp signing"
	UsageMicrosoftSGC      KeyUsage = "microsoft sgc"
	UsageNetscapeSGC       KeyUsage = "netscape sgc"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:prerelease-lifecycle-gen:introduced=1.33
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ClusterTrustBundle is a cluster-scoped container for X.509 trust anchors
// (root certificates).
//
// ClusterTrustBundle objects are considered to be readable by any authenticated
// user in the cluster, because they can be mounted by pods using the
// `clusterTrustBundle` projection.  All service accounts have read access to
// ClusterTrustBundles by default.  Users who only have namespace-level access
// to a cluster can read ClusterTrustBundles by impersonating a serviceaccount
// that they have access to.
//
// It can be optionally associated with a particular assigner, in which case it
// contains one valid set of trust anchors for that signer. Signers may have
// multiple associated ClusterTrustBundles; each is an independent set of trust
// anchors for that signer. Admission control is used to enforce that only users
// with permissions on the signer can create or modify the corresponding bundle.
type ClusterTrustBundle struct {
	metav1.TypeMeta `json:",inline"`

	// metadata contains the object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec contains the signer (if any) and trust anchors.
	Spec ClusterTrustBundleSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`
}

// ClusterTrustBundleSpec contains the signer and trust anchors.
type ClusterTrustBundleSpec struct {
	// signerName indicates the associated signer, if any.
	//
	// In order to create or update a ClusterTrustBundle that sets signerName,
	// you must have the following cluster-scoped permission:
	// group=certificates.k8s.io resource=signers resourceName=<the signer name>
	// verb=attest.
	//
	// If signerName is not empty, then the ClusterTrustBundle object must be
	// named with the signer name as a prefix (translating slashes to colons).
	// For example, for the signer name `example.com/foo`, valid
	// ClusterTrustBundle object names include `example.com:foo:abc` and
	// `example.com:foo:v1`.
	//
	// If signerName is empty, then the ClusterTrustBundle object's name must
	// not have such a prefix.
	//
	// List/watch requests for ClusterTrustBundles can filter on this field
	// using a `spec.signerName=NAME` field selector.
	//
	// +optional
	SignerName string `json:"signerName,omitempty" protobuf:"bytes,1,opt,name=signerName"`

	// trustBundle contains the individual X.509 trust anchors for this
	// bundle, as PEM bundle of PEM-wrapped, DER-formatted X.509 certificates.
	//
	// The data must consist only of PEM certificate blocks that parse as valid
	// X.509 certificates.  Each certificate must include a basic constraints
	// extension with the CA bit set.  The API server will reject objects that
	// contain duplicate certificates, or that use PEM block headers.
	//
	// Users of ClusterTrustBundles, including Kubelet, are free to reorder and
	// deduplicate certificate blocks in this file according to their own logic,
	// as well as to drop PEM block headers and inter-block data.
	TrustBundle string `json:"trustBundle" protobuf:"bytes,2,opt,name=trustBundle"`
}

// +k8s:prerelease-lifecycle-gen:introduced=1.33
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ClusterTrustBundleList is a collection of ClusterTrustBundle objects
type ClusterTrustBundleList struct {
	metav1.TypeMeta `json:",inline"`

	// metadata contains the list metadata.
	//
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is a collection of ClusterTrustBundle objects
	Items []ClusterTrustBundle `json:"items" protobuf:"bytes,2,rep,name=items"`
}

/*
Copyright 2017 The Kubernetes Authors.

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

package cleaner

import (
	"context"
	"testing"
	"time"

	capi "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	expiredCert = `-----BEGIN CERTIFICATE-----
MIICIzCCAc2gAwIBAgIJAOApTlMFDOUnMA0GCSqGSIb3DQEBCwUAMG0xCzAJBgNV
BAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNVBAcMBkxvbmRvbjEYMBYGA1UE
CgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1JVCBEZXBhcnRtZW50MQowCAYD
VQQDDAEqMB4XDTE3MTAwNDIwNDgzOFoXDTE3MTAwMzIwNDgzOFowbTELMAkGA1UE
BhMCR0IxDzANBgNVBAgMBkxvbmRvbjEPMA0GA1UEBwwGTG9uZG9uMRgwFgYDVQQK
DA9HbG9iYWwgU2VjdXJpdHkxFjAUBgNVBAsMDUlUIERlcGFydG1lbnQxCjAIBgNV
BAMMASowXDANBgkqhkiG9w0BAQEFAANLADBIAkEA3Gt0KmuRXDxvqZUiX/xqAn1t
nZZX98guZvPPyxnQtV3YpA274W0sX3jL+U71Ya+3kaUstXQa4YrWBUHiXoqJnwID
AQABo1AwTjAdBgNVHQ4EFgQUtDsIpzHoUiLsO88f9fm+G0tYSPowHwYDVR0jBBgw
FoAUtDsIpzHoUiLsO88f9fm+G0tYSPowDAYDVR0TBAUwAwEB/zANBgkqhkiG9w0B
AQsFAANBADfrlKof5CUkxGlX9Rifxv/mWOk8ZuTLWfMYQH2nycBHnmOxy6sR+87W
/Mb/uRz0TXVnGVcbu5E8Bz7e/Far1ZI=
-----END CERTIFICATE-----`
	unexpiredCert = `-----BEGIN CERTIFICATE-----
MIICJTCCAc+gAwIBAgIJAIRjMToP+pPEMA0GCSqGSIb3DQEBCwUAMG0xCzAJBgNV
BAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNVBAcMBkxvbmRvbjEYMBYGA1UE
CgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1JVCBEZXBhcnRtZW50MQowCAYD
VQQDDAEqMCAXDTE3MTAwNDIwNDUyNFoYDzIxMTcwOTEwMjA0NTI0WjBtMQswCQYD
VQQGEwJHQjEPMA0GA1UECAwGTG9uZG9uMQ8wDQYDVQQHDAZMb25kb24xGDAWBgNV
BAoMD0dsb2JhbCBTZWN1cml0eTEWMBQGA1UECwwNSVQgRGVwYXJ0bWVudDEKMAgG
A1UEAwwBKjBcMA0GCSqGSIb3DQEBAQUAA0sAMEgCQQC7j9BAV5HqIJGi6r4G4YeI
ioHxH2loVu8IOKSK7xVs3v/EjR/eXbQzM+jZU7duyZqn6YjySZNLl0K0MfHCHBgX
AgMBAAGjUDBOMB0GA1UdDgQWBBTwxV40NFSNW7lpQ3eUWX7Mxs03yzAfBgNVHSME
GDAWgBTwxV40NFSNW7lpQ3eUWX7Mxs03yzAMBgNVHRMEBTADAQH/MA0GCSqGSIb3
DQEBCwUAA0EALDi9OidANHflx8q+w3p0rJo9gpA6cJcFpEtP2Lv4kvOtB1f6L0jY
MLd7MVm4cS/MNcx4L7l23UC3Hx4+nAxvIg==
-----END CERTIFICATE-----`
)

func TestCleanerWithApprovedExpiredCSR(t *testing.T) {
	testCases := []struct {
		name            string
		created         metav1.Time
		certificate     []byte
		conditions      []capi.CertificateSigningRequestCondition
		expectedActions []string
	}{
		{
			"no delete approved not passed deadline",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			[]byte(unexpiredCert),
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-50 * time.Minute)),
				},
			},
			[]string{},
		},
		{
			"no delete approved passed deadline not issued",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			nil,
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-50 * time.Minute)),
				},
			},
			[]string{},
		},
		{
			"delete approved passed deadline",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			[]byte(unexpiredCert),
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
				},
			},
			[]string{"delete"},
		},
		{
			"no delete denied not passed deadline",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			nil,
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateDenied,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-50 * time.Minute)),
				},
			},
			[]string{},
		},
		{
			"delete denied passed deadline",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			nil,
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateDenied,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
				},
			},
			[]string{"delete"},
		},
		{
			"no delete failed not passed deadline",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			nil,
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
				},
				{
					Type:           capi.CertificateFailed,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-50 * time.Minute)),
				},
			},
			[]string{},
		},
		{
			"delete failed passed deadline",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			nil,
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
				},
				{
					Type:           capi.CertificateFailed,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
				},
			},
			[]string{"delete"},
		},
		{
			"no delete pending not passed deadline",
			metav1.NewTime(time.Now().Add(-5 * time.Hour)),
			nil,
			[]capi.CertificateSigningRequestCondition{},
			[]string{},
		},
		{
			"delete pending passed deadline",
			metav1.NewTime(time.Now().Add(-25 * time.Hour)),
			nil,
			[]capi.CertificateSigningRequestCondition{},
			[]string{"delete"},
		},
		{
			"no delete approved not passed deadline unexpired",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			[]byte(unexpiredCert),
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-50 * time.Minute)),
				},
			},
			[]string{},
		},
		{
			"delete approved not passed deadline expired",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			[]byte(expiredCert),
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-50 * time.Minute)),
				},
			},
			[]string{"delete"},
		},
		{
			"delete approved passed deadline unparsable",
			metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			[]byte(`garbage`),
			[]capi.CertificateSigningRequestCondition{
				{
					Type:           capi.CertificateApproved,
					LastUpdateTime: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
				},
			},
			[]string{"delete"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			csr := &capi.CertificateSigningRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "fake-csr",
					CreationTimestamp: tc.created,
				},
				Status: capi.CertificateSigningRequestStatus{
					Certificate: tc.certificate,
					Conditions:  tc.conditions,
				},
			}

			client := fake.NewSimpleClientset(csr)
			s := &CSRCleanerController{
				csrClient: client.CertificatesV1().CertificateSigningRequests(),
			}
			ctx := context.TODO()
			err := s.handle(ctx, csr)
			if err != nil {
				t.Fatalf("failed to clean CSR: %v", err)
			}

			actions := client.Actions()
			if len(actions) != len(tc.expectedActions) {
				t.Fatalf("got %d actions, wanted %d actions", len(actions), len(tc.expectedActions))
			}
			for i := 0; i < len(actions); i++ {
				if a := actions[i]; !a.Matches(tc.expectedActions[i], "certificatesigningrequests") {
					t.Errorf("got action %#v, wanted %v", a, tc.expectedActions[i])
				}
			}
		})
	}
}

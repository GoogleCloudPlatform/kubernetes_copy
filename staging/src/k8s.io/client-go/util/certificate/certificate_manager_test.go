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

package certificate

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	watch "k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	certificatesclient "k8s.io/client-go/kubernetes/typed/certificates/v1beta1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2/ktesting"
	netutils "k8s.io/utils/net"
)

var storeCertData = newCertificateData(`-----BEGIN CERTIFICATE-----
MIICRzCCAfGgAwIBAgIJALMb7ecMIk3MMA0GCSqGSIb3DQEBCwUAMH4xCzAJBgNV
BAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNVBAcMBkxvbmRvbjEYMBYGA1UE
CgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1JVCBEZXBhcnRtZW50MRswGQYD
VQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTAwIBcNMTcwNDI2MjMyNjUyWhgPMjExNzA0
MDIyMzI2NTJaMH4xCzAJBgNVBAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNV
BAcMBkxvbmRvbjEYMBYGA1UECgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1J
VCBEZXBhcnRtZW50MRswGQYDVQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTAwXDANBgkq
hkiG9w0BAQEFAANLADBIAkEAtBMa7NWpv3BVlKTCPGO/LEsguKqWHBtKzweMY2CV
tAL1rQm913huhxF9w+ai76KQ3MHK5IVnLJjYYA5MzP2H5QIDAQABo1AwTjAdBgNV
HQ4EFgQU22iy8aWkNSxv0nBxFxerfsvnZVMwHwYDVR0jBBgwFoAU22iy8aWkNSxv
0nBxFxerfsvnZVMwDAYDVR0TBAUwAwEB/zANBgkqhkiG9w0BAQsFAANBAEOefGbV
NcHxklaW06w6OBYJPwpIhCVozC1qdxGX1dg8VkEKzjOzjgqVD30m59OFmSlBmHsl
nkVA6wyOSDYBf3o=
-----END CERTIFICATE-----`, `-----BEGIN RSA PRIVATE KEY-----
MIIBUwIBADANBgkqhkiG9w0BAQEFAASCAT0wggE5AgEAAkEAtBMa7NWpv3BVlKTC
PGO/LEsguKqWHBtKzweMY2CVtAL1rQm913huhxF9w+ai76KQ3MHK5IVnLJjYYA5M
zP2H5QIDAQABAkAS9BfXab3OKpK3bIgNNyp+DQJKrZnTJ4Q+OjsqkpXvNltPJosf
G8GsiKu/vAt4HGqI3eU77NvRI+mL4MnHRmXBAiEA3qM4FAtKSRBbcJzPxxLEUSwg
XSCcosCktbkXvpYrS30CIQDPDxgqlwDEJQ0uKuHkZI38/SPWWqfUmkecwlbpXABK
iQIgZX08DA8VfvcA5/Xj1Zjdey9FVY6POLXen6RPiabE97UCICp6eUW7ht+2jjar
e35EltCRCjoejRHTuN9TC0uCoVipAiAXaJIx/Q47vGwiw6Y8KXsNU6y54gTbOSxX
54LzHNk/+Q==
-----END RSA PRIVATE KEY-----`)
var storeTwoCertsData = newCertificateData(`-----BEGIN CERTIFICATE-----
MIIDfTCCAyegAwIBAgIUFBl4gUoqZDP/wUJDn37/VJ9upD0wDQYJKoZIhvcNAQEF
BQAwfjELMAkGA1UEBhMCR0IxDzANBgNVBAgMBkxvbmRvbjEPMA0GA1UEBwwGTG9u
ZG9uMRgwFgYDVQQKDA9HbG9iYWwgU2VjdXJpdHkxFjAUBgNVBAsMDUlUIERlcGFy
dG1lbnQxGzAZBgNVBAMMEnRlc3QtY2VydGlmaWNhdGUtMDAeFw0yMDAzMDIxOTM3
MDBaFw0yMTAzMDIxOTM3MDBaMIGIMQswCQYDVQQGEwJVUzETMBEGA1UECBMKQ2Fs
aWZvcm5pYTEWMBQGA1UEBxMNU2FuIEZyYW5jaXNjbzEdMBsGA1UEChMURXhhbXBs
ZSBDb21wYW55LCBMTEMxEzARBgNVBAsTCk9wZXJhdGlvbnMxGDAWBgNVBAMTD3d3
dy5leGFtcGxlLmNvbTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAMiR
DNpmwTICFr+P16fKDVjbNCzSjWq+MTu8vAfS6GrLpBTUEe+6zVqxUza/fZenxo8O
ucV2JTUv5J4nkT/vG6Qm/mToVJ4vQzLQ5jR2w7v/7cf3oWCwTAKUafgo6/Ga95gn
lQB3+Fd8sy96zfFr/7wDSMPPueR5kSFax+cEd30wwv5O7tWj0ro1mrxLssBlwPaR
ZlzkkvxBYTzWCqKZsWktQlXciqlFSos0ua7uvwqKN5CTxfC/xoyMxx9kfZm7BzPN
ZDqYMFw2HiWdEiLzI4jj+Gh0D5t47tnvlpUMihcX9x0jP6/+hnfcQ8GAP2jR/BXY
5YZRRY70LiCXPevlRAECAwEAAaOBqTCBpjAOBgNVHQ8BAf8EBAMCBaAwHQYDVR0l
BBYwFAYIKwYBBQUHAwEGCCsGAQUFBwMCMAwGA1UdEwEB/wQCMAAwHQYDVR0OBBYE
FOoiE+kh7gGDpyx0KZuCc1lrlTRKMB8GA1UdIwQYMBaAFNtosvGlpDUsb9JwcRcX
q37L52VTMCcGA1UdEQQgMB6CC2V4YW1wbGUuY29tgg93d3cuZXhhbXBsZS5jb20w
DQYJKoZIhvcNAQEFBQADQQAw6mxQONAD2sivfzIf1eDFd6LU7aE+MnkdlEQjjPCi
tlUITFIuO3XavISupP6V9wE0b1wTF1pTlVWArf/0YQXs
-----END CERTIFICATE-----
-----BEGIN CERTIFICATE-----
MIICRzCCAfGgAwIBAgIJALMb7ecMIk3MMA0GCSqGSIb3DQEBCwUAMH4xCzAJBgNV
BAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNVBAcMBkxvbmRvbjEYMBYGA1UE
CgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1JVCBEZXBhcnRtZW50MRswGQYD
VQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTAwIBcNMTcwNDI2MjMyNjUyWhgPMjExNzA0
MDIyMzI2NTJaMH4xCzAJBgNVBAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNV
BAcMBkxvbmRvbjEYMBYGA1UECgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1J
VCBEZXBhcnRtZW50MRswGQYDVQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTAwXDANBgkq
hkiG9w0BAQEFAANLADBIAkEAtBMa7NWpv3BVlKTCPGO/LEsguKqWHBtKzweMY2CV
tAL1rQm913huhxF9w+ai76KQ3MHK5IVnLJjYYA5MzP2H5QIDAQABo1AwTjAdBgNV
HQ4EFgQU22iy8aWkNSxv0nBxFxerfsvnZVMwHwYDVR0jBBgwFoAU22iy8aWkNSxv
0nBxFxerfsvnZVMwDAYDVR0TBAUwAwEB/zANBgkqhkiG9w0BAQsFAANBAEOefGbV
NcHxklaW06w6OBYJPwpIhCVozC1qdxGX1dg8VkEKzjOzjgqVD30m59OFmSlBmHsl
nkVA6wyOSDYBf3o=
-----END CERTIFICATE-----`, `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAyJEM2mbBMgIWv4/Xp8oNWNs0LNKNar4xO7y8B9LoasukFNQR
77rNWrFTNr99l6fGjw65xXYlNS/knieRP+8bpCb+ZOhUni9DMtDmNHbDu//tx/eh
YLBMApRp+Cjr8Zr3mCeVAHf4V3yzL3rN8Wv/vANIw8+55HmRIVrH5wR3fTDC/k7u
1aPSujWavEuywGXA9pFmXOSS/EFhPNYKopmxaS1CVdyKqUVKizS5ru6/Coo3kJPF
8L/GjIzHH2R9mbsHM81kOpgwXDYeJZ0SIvMjiOP4aHQPm3ju2e+WlQyKFxf3HSM/
r/6Gd9xDwYA/aNH8FdjlhlFFjvQuIJc96+VEAQIDAQABAoIBAQCc6R3tH8a1oPy7
EYXeNy0J/zRqfK82e2V5HsbcOByssHTF9sOxkatm8KPxiQ5wv0mQUiz0VuH1Imrx
cHMqWZ5+ZiNQPpM0zjT8ZII1OVUYl7knYIxYYJSW0BW3mAw/EMXzu8POgg1AJMbq
tmC4J44DQW6EAtej75ejSKpsCgqRXVoi3iEk9eMLHUFIHqkzl/aKEc7k/P+eKo2h
PHsDoKZdmOmZA3OKzw61xAqJICYyplRHatQcEiWJgnLer+9qvUGc4k8eqAYeDGm7
T78XcUvsXOug2GClVWGZu1quFhf7MxjzFfOjz4q9HwPex7X6nQL0IX2hzMECkaMC
iUMZGGEhAoGBAOLY1KSNOjvt54MkKznI8stHkx8V73c0Nxbz5Rj8gM0Gwk1FWVas
jgoAbKPQ2UL/RglLX1JZvztKvNuWSEeZGqggDvhzB38leiEH+OY7DZ7a0c5sWwdF
CpcT1mJb91ww5xEC09WO8Oq3i5olVBBivOl5EjwKHOQn2TUh2OSLhqf/AoGBAOJX
mxqdTEUwFU9ecsAOK9labjI7mA5so0vIq8eq1Q670NFszChfSMKJAqQ90N1LEu9z
L0f6CBXYCn7sMmOlF4CKE+u2/ieJfD1OkKq7RwEd3pi4X3xtAlcPK8F/QprmQWo0
wi33BDBb4zYkuQB6Q5RYIV2di7k+HBpoQPottBP/AoGAIB4xJUc1qoyJjeDOGfVg
ovV0WB9j8026Sw6nLj16Aw1k70nVV1dBGRtsRllomXrJMMGyMleworV3PePuQezk
gE9hrz2iHxdwTkLxs69Cw24Z7I8c6E+XK0LMxMpeoHfwD1GGKqN9as4n/uAwIc3J
D4lr0oJgCtG1iDdNnTZAD4MCgYAkOpWPCwJ8SJgAnkOLzjjij4D39WX/WRBCPxqP
2R5FP3bLLrj29Vl2GewcUfCumyeqwCsfQDwvEueLLU9bd79tSayqnB3OQklqnrq1
OUjCOv+4Pjq6ddBcEweT70S/+n8Z+tvh85nuC6cwsWwTUX6jrf+ZNnB49CIXb/yG
ju42DQKBgAPtbB/ON3+GtnSTHBSY6HwZvGJrBDicrXmr1U9zuA8yYxv8qaRXZkpn
2cpLLvO2MJutwXMYf+T3x1ZCFMkE56pOswSTGrCQWRl3hOiJayLHQyAOYHPnYeZB
78iRJPUZ0biEQUZQ62GBxWkcB0qkxa9m759h/TvLwvV0RrO5Uzd0
-----END RSA PRIVATE KEY-----`)
var expiredStoreCertData = newCertificateData(`-----BEGIN CERTIFICATE-----
MIIBFzCBwgIJALhygXnxXmN1MA0GCSqGSIb3DQEBCwUAMBMxETAPBgNVBAMMCGhv
c3QtMTIzMB4XDTE4MTEwNDIzNTc1NFoXDTE4MTEwNTIzNTc1NFowEzERMA8GA1UE
AwwIaG9zdC0xMjMwXDANBgkqhkiG9w0BAQEFAANLADBIAkEAtBMa7NWpv3BVlKTC
PGO/LEsguKqWHBtKzweMY2CVtAL1rQm913huhxF9w+ai76KQ3MHK5IVnLJjYYA5M
zP2H5QIDAQABMA0GCSqGSIb3DQEBCwUAA0EAN2DPFUtCzqnidL+5nh+46Sk6dkMI
T5DD11UuuIjZusKvThsHKVCIsyJ2bDo7cTbI+/nklLRP+FcC2wESFUgXbA==
-----END CERTIFICATE-----`, `-----BEGIN RSA PRIVATE KEY-----
MIIBUwIBADANBgkqhkiG9w0BAQEFAASCAT0wggE5AgEAAkEAtBMa7NWpv3BVlKTC
PGO/LEsguKqWHBtKzweMY2CVtAL1rQm913huhxF9w+ai76KQ3MHK5IVnLJjYYA5M
zP2H5QIDAQABAkAS9BfXab3OKpK3bIgNNyp+DQJKrZnTJ4Q+OjsqkpXvNltPJosf
G8GsiKu/vAt4HGqI3eU77NvRI+mL4MnHRmXBAiEA3qM4FAtKSRBbcJzPxxLEUSwg
XSCcosCktbkXvpYrS30CIQDPDxgqlwDEJQ0uKuHkZI38/SPWWqfUmkecwlbpXABK
iQIgZX08DA8VfvcA5/Xj1Zjdey9FVY6POLXen6RPiabE97UCICp6eUW7ht+2jjar
e35EltCRCjoejRHTuN9TC0uCoVipAiAXaJIx/Q47vGwiw6Y8KXsNU6y54gTbOSxX
54LzHNk/+Q==
-----END RSA PRIVATE KEY-----`)
var bootstrapCertData = newCertificateData(
	`-----BEGIN CERTIFICATE-----
MIICRzCCAfGgAwIBAgIJANXr+UzRFq4TMA0GCSqGSIb3DQEBCwUAMH4xCzAJBgNV
BAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNVBAcMBkxvbmRvbjEYMBYGA1UE
CgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1JVCBEZXBhcnRtZW50MRswGQYD
VQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTEwIBcNMTcwNDI2MjMyNzMyWhgPMjExNzA0
MDIyMzI3MzJaMH4xCzAJBgNVBAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNV
BAcMBkxvbmRvbjEYMBYGA1UECgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1J
VCBEZXBhcnRtZW50MRswGQYDVQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTEwXDANBgkq
hkiG9w0BAQEFAANLADBIAkEAqvbkN4RShH1rL37JFp4fZPnn0JUhVWWsrP8NOomJ
pXdBDUMGWuEQIsZ1Gf9JrCQLu6ooRyHSKRFpAVbMQ3ABJwIDAQABo1AwTjAdBgNV
HQ4EFgQUEGBc6YYheEZ/5MhwqSUYYPYRj2MwHwYDVR0jBBgwFoAUEGBc6YYheEZ/
5MhwqSUYYPYRj2MwDAYDVR0TBAUwAwEB/zANBgkqhkiG9w0BAQsFAANBAIyNmznk
5dgJY52FppEEcfQRdS5k4XFPc22SHPcz77AHf5oWZ1WG9VezOZZPp8NCiFDDlDL8
yma33a5eMyTjLD8=
-----END CERTIFICATE-----`, `-----BEGIN RSA PRIVATE KEY-----
MIIBVAIBADANBgkqhkiG9w0BAQEFAASCAT4wggE6AgEAAkEAqvbkN4RShH1rL37J
Fp4fZPnn0JUhVWWsrP8NOomJpXdBDUMGWuEQIsZ1Gf9JrCQLu6ooRyHSKRFpAVbM
Q3ABJwIDAQABAkBC2OBpGLMPHN8BJijIUDFkURakBvuOoX+/8MYiYk7QxEmfLCk6
L6r+GLNFMfXwXcBmXtMKfZKAIKutKf098JaBAiEA10azfqt3G/5owrNA00plSyT6
ZmHPzY9Uq1p/QTR/uOcCIQDLTkfBkLHm0UKeobbO/fSm6ZflhyBRDINy4FvwmZMt
wQIgYV/tmQJeIh91q3wBepFQOClFykG8CTMoDUol/YyNqUkCIHfp6Rr7fGL3JIMq
QQgf9DCK8SPZqq8DYXjdan0kKBJBAiEAyDb+07o2gpggo8BYUKSaiRCiyXfaq87f
eVqgpBq/QN4=
-----END RSA PRIVATE KEY-----`)
var apiServerCertData = newCertificateData(
	`-----BEGIN CERTIFICATE-----
MIICRzCCAfGgAwIBAgIJAIydTIADd+yqMA0GCSqGSIb3DQEBCwUAMH4xCzAJBgNV
BAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNVBAcMBkxvbmRvbjEYMBYGA1UE
CgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1JVCBEZXBhcnRtZW50MRswGQYD
VQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTIwIBcNMTcwNDI2MjMyNDU4WhgPMjExNzA0
MDIyMzI0NThaMH4xCzAJBgNVBAYTAkdCMQ8wDQYDVQQIDAZMb25kb24xDzANBgNV
BAcMBkxvbmRvbjEYMBYGA1UECgwPR2xvYmFsIFNlY3VyaXR5MRYwFAYDVQQLDA1J
VCBEZXBhcnRtZW50MRswGQYDVQQDDBJ0ZXN0LWNlcnRpZmljYXRlLTIwXDANBgkq
hkiG9w0BAQEFAANLADBIAkEAuiRet28DV68Dk4A8eqCaqgXmymamUEjW/DxvIQqH
3lbhtm8BwSnS9wUAajSLSWiq3fci2RbRgaSPjUrnbOHCLQIDAQABo1AwTjAdBgNV
HQ4EFgQU0vhI4OPGEOqT+VAWwxdhVvcmgdIwHwYDVR0jBBgwFoAU0vhI4OPGEOqT
+VAWwxdhVvcmgdIwDAYDVR0TBAUwAwEB/zANBgkqhkiG9w0BAQsFAANBALNeJGDe
nV5cXbp9W1bC12Tc8nnNXn4ypLE2JTQAvyp51zoZ8hQoSnRVx/VCY55Yu+br8gQZ
+tW+O/PoE7B3tuY=
-----END CERTIFICATE-----`, `-----BEGIN RSA PRIVATE KEY-----
MIIBVgIBADANBgkqhkiG9w0BAQEFAASCAUAwggE8AgEAAkEAuiRet28DV68Dk4A8
eqCaqgXmymamUEjW/DxvIQqH3lbhtm8BwSnS9wUAajSLSWiq3fci2RbRgaSPjUrn
bOHCLQIDAQABAkEArDR1g9IqD3aUImNikDgAngbzqpAokOGyMoxeavzpEaFOgCzi
gi7HF7yHRmZkUt8CzdEvnHSqRjFuaaB0gGA+AQIhAOc8Z1h8ElLRSqaZGgI3jCTp
Izx9HNY//U5NGrXD2+ttAiEAzhOqkqI4+nDab7FpiD7MXI6fO549mEXeVBPvPtsS
OcECIQCIfkpOm+ZBBpO3JXaJynoqK4gGI6ALA/ik6LSUiIlfPQIhAISjd9hlfZME
bDQT1r8Q3Gx+h9LRqQeHgPBQ3F5ylqqBAiBaJ0hkYvrIdWxNlcLqD3065bJpHQ4S
WQkuZUQN1M/Xvg==
-----END RSA PRIVATE KEY-----`)

type certificateData struct {
	keyPEM         []byte
	certificatePEM []byte
	certificate    *tls.Certificate
}

func newCertificateData(certificatePEM string, keyPEM string) *certificateData {
	certificate, err := tls.X509KeyPair([]byte(certificatePEM), []byte(keyPEM))
	if err != nil {
		panic(fmt.Sprintf("Unable to initialize certificate: %v", err))
	}
	certs, err := x509.ParseCertificates(certificate.Certificate[0])
	if err != nil {
		panic(fmt.Sprintf("Unable to initialize certificate leaf: %v", err))
	}
	certificate.Leaf = certs[0]
	return &certificateData{
		keyPEM:         []byte(keyPEM),
		certificatePEM: []byte(certificatePEM),
		certificate:    &certificate,
	}
}

func TestNewManagerNoRotation(t *testing.T) {
	store := &fakeStore{
		cert: storeCertData.certificate,
	}
	if _, err := NewManager(&Config{
		Template:         &x509.CertificateRequest{},
		Usages:           []certificatesv1.KeyUsage{},
		CertificateStore: store,
	}); err != nil {
		t.Fatalf("Failed to initialize the certificate manager: %v", err)
	}
}

type metricMock struct {
	calls     int
	lastValue float64
}

func (g *metricMock) Set(v float64) {
	g.calls++
	g.lastValue = v
}

func (g *metricMock) Observe(v float64) {
	g.calls++
	g.lastValue = v
}

func TestSetRotationDeadline(t *testing.T) {
	defer func(original func(float64) time.Duration) { jitteryDuration = original }(jitteryDuration)

	now := time.Now()
	testCases := []struct {
		name         string
		notBefore    time.Time
		notAfter     time.Time
		shouldRotate bool
	}{
		{"just issued, still good", now.Add(-1 * time.Hour), now.Add(99 * time.Hour), false},
		{"half way expired, still good", now.Add(-24 * time.Hour), now.Add(24 * time.Hour), false},
		{"mostly expired, still good", now.Add(-69 * time.Hour), now.Add(31 * time.Hour), false},
		{"just about expired, should rotate", now.Add(-91 * time.Hour), now.Add(9 * time.Hour), true},
		{"nearly expired, should rotate", now.Add(-99 * time.Hour), now.Add(1 * time.Hour), true},
		{"already expired, should rotate", now.Add(-10 * time.Hour), now.Add(-1 * time.Hour), true},
		{"long duration", now.Add(-6 * 30 * 24 * time.Hour), now.Add(6 * 30 * 24 * time.Hour), true},
		{"short duration", now.Add(-30 * time.Second), now.Add(30 * time.Second), true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			m := manager{
				cert: &tls.Certificate{
					Leaf: &x509.Certificate{
						NotBefore: tc.notBefore,
						NotAfter:  tc.notAfter,
					},
				},
				getTemplate: func() *x509.CertificateRequest { return &x509.CertificateRequest{} },
				now:         func() time.Time { return now },
				ctx:         ctx,
			}
			jitteryDuration = func(float64) time.Duration { return time.Duration(float64(tc.notAfter.Sub(tc.notBefore)) * 0.7) }
			lowerBound := tc.notBefore.Add(time.Duration(float64(tc.notAfter.Sub(tc.notBefore)) * 0.7))

			deadline := m.nextRotationDeadline(ctx)

			if !deadline.Equal(lowerBound) {
				t.Errorf("For notBefore %v, notAfter %v, the rotationDeadline %v should be %v.",
					tc.notBefore,
					tc.notAfter,
					deadline,
					lowerBound)
			}
		})
	}
}

func TestCertSatisfiesTemplate(t *testing.T) {
	testCases := []struct {
		name          string
		cert          *x509.Certificate
		template      *x509.CertificateRequest
		shouldSatisfy bool
	}{
		{
			name:          "No certificate, no template",
			cert:          nil,
			template:      nil,
			shouldSatisfy: false,
		},
		{
			name:          "No certificate",
			cert:          nil,
			template:      &x509.CertificateRequest{},
			shouldSatisfy: false,
		},
		{
			name: "No template",
			cert: &x509.Certificate{
				Subject: pkix.Name{
					CommonName: "system:node:fake-node-name",
				},
			},
			template:      nil,
			shouldSatisfy: true,
		},
		{
			name: "Mismatched common name",
			cert: &x509.Certificate{
				Subject: pkix.Name{
					CommonName: "system:node:fake-node-name-2",
				},
			},
			template: &x509.CertificateRequest{
				Subject: pkix.Name{
					CommonName: "system:node:fake-node-name",
				},
			},
			shouldSatisfy: false,
		},
		{
			name: "Missing orgs in certificate",
			cert: &x509.Certificate{
				Subject: pkix.Name{
					Organization: []string{"system:nodes"},
				},
			},
			template: &x509.CertificateRequest{
				Subject: pkix.Name{
					Organization: []string{"system:nodes", "foobar"},
				},
			},
			shouldSatisfy: false,
		},
		{
			name: "Extra orgs in certificate",
			cert: &x509.Certificate{
				Subject: pkix.Name{
					Organization: []string{"system:nodes", "foobar"},
				},
			},
			template: &x509.CertificateRequest{
				Subject: pkix.Name{
					Organization: []string{"system:nodes"},
				},
			},
			shouldSatisfy: true,
		},
		{
			name: "Missing DNS names in certificate",
			cert: &x509.Certificate{
				Subject:  pkix.Name{},
				DNSNames: []string{"foo.example.com"},
			},
			template: &x509.CertificateRequest{
				Subject:  pkix.Name{},
				DNSNames: []string{"foo.example.com", "bar.example.com"},
			},
			shouldSatisfy: false,
		},
		{
			name: "Extra DNS names in certificate",
			cert: &x509.Certificate{
				Subject:  pkix.Name{},
				DNSNames: []string{"foo.example.com", "bar.example.com"},
			},
			template: &x509.CertificateRequest{
				Subject:  pkix.Name{},
				DNSNames: []string{"foo.example.com"},
			},
			shouldSatisfy: true,
		},
		{
			name: "Missing IP addresses in certificate",
			cert: &x509.Certificate{
				Subject:     pkix.Name{},
				IPAddresses: []net.IP{netutils.ParseIPSloppy("192.168.1.1")},
			},
			template: &x509.CertificateRequest{
				Subject:     pkix.Name{},
				IPAddresses: []net.IP{netutils.ParseIPSloppy("192.168.1.1"), netutils.ParseIPSloppy("192.168.1.2")},
			},
			shouldSatisfy: false,
		},
		{
			name: "Extra IP addresses in certificate",
			cert: &x509.Certificate{
				Subject:     pkix.Name{},
				IPAddresses: []net.IP{netutils.ParseIPSloppy("192.168.1.1"), netutils.ParseIPSloppy("192.168.1.2")},
			},
			template: &x509.CertificateRequest{
				Subject:     pkix.Name{},
				IPAddresses: []net.IP{netutils.ParseIPSloppy("192.168.1.1")},
			},
			shouldSatisfy: true,
		},
		{
			name: "Matching certificate",
			cert: &x509.Certificate{
				Subject: pkix.Name{
					CommonName:   "system:node:fake-node-name",
					Organization: []string{"system:nodes"},
				},
				DNSNames:    []string{"foo.example.com"},
				IPAddresses: []net.IP{netutils.ParseIPSloppy("192.168.1.1")},
			},
			template: &x509.CertificateRequest{
				Subject: pkix.Name{
					CommonName:   "system:node:fake-node-name",
					Organization: []string{"system:nodes"},
				},
				DNSNames:    []string{"foo.example.com"},
				IPAddresses: []net.IP{netutils.ParseIPSloppy("192.168.1.1")},
			},
			shouldSatisfy: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			var tlsCert *tls.Certificate

			if tc.cert != nil {
				tlsCert = &tls.Certificate{
					Leaf: tc.cert,
				}
			}

			m := manager{
				cert:        tlsCert,
				getTemplate: func() *x509.CertificateRequest { return tc.template },
				now:         time.Now,
				ctx:         ctx,
			}

			result := m.certSatisfiesTemplate(ctx)
			if result != tc.shouldSatisfy {
				t.Errorf("cert: %+v, template: %+v, certSatisfiesTemplate returned %v, want %v", m.cert, tc.template, result, tc.shouldSatisfy)
			}
		})
	}
}

func TestRotateCertCreateCSRError(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	now := time.Now()
	m := manager{
		cert: &tls.Certificate{
			Leaf: &x509.Certificate{
				NotBefore: now.Add(-2 * time.Hour),
				NotAfter:  now.Add(-1 * time.Hour),
			},
		},
		getTemplate: func() *x509.CertificateRequest { return &x509.CertificateRequest{} },
		clientsetFn: func(_ *tls.Certificate) (clientset.Interface, error) {
			return newClientset(fakeClient{failureType: createError}), nil
		},
		now: func() time.Time { return now },
		ctx: ctx,
	}

	if success, err := m.rotateCerts(ctx); success {
		t.Errorf("Got success from 'rotateCerts', wanted failure")
	} else if err != nil {
		t.Errorf("Got error %v from 'rotateCerts', wanted no error.", err)
	}
}

func TestRotateCertWaitingForResultError(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	now := time.Now()
	m := manager{
		cert: &tls.Certificate{
			Leaf: &x509.Certificate{
				NotBefore: now.Add(-2 * time.Hour),
				NotAfter:  now.Add(-1 * time.Hour),
			},
		},
		getTemplate: func() *x509.CertificateRequest { return &x509.CertificateRequest{} },
		clientsetFn: func(_ *tls.Certificate) (clientset.Interface, error) {
			return newClientset(fakeClient{failureType: watchError}), nil
		},
		now: func() time.Time { return now },
		ctx: ctx,
	}

	defer func(t time.Duration) { certificateWaitTimeout = t }(certificateWaitTimeout)
	certificateWaitTimeout = 1 * time.Millisecond
	if success, err := m.rotateCerts(ctx); success {
		t.Errorf("Got success from 'rotateCerts', wanted failure.")
	} else if err != nil {
		t.Errorf("Got error %v from 'rotateCerts', wanted no error.", err)
	}
}

func TestNewManagerBootstrap(t *testing.T) {
	store := &fakeStore{}

	var cm Manager
	cm, err := NewManager(&Config{
		Template:                &x509.CertificateRequest{},
		Usages:                  []certificatesv1.KeyUsage{},
		CertificateStore:        store,
		BootstrapCertificatePEM: bootstrapCertData.certificatePEM,
		BootstrapKeyPEM:         bootstrapCertData.keyPEM,
	})
	if err != nil {
		t.Fatalf("Failed to initialize the certificate manager: %v", err)
	}

	cert := cm.Current()

	if cert == nil {
		t.Errorf("Certificate was nil, expected something.")
	}
	if m, ok := cm.(*manager); !ok {
		t.Errorf("Expected a '*manager' from 'NewManager'")
	} else if !m.forceRotation {
		t.Errorf("Expected rotation should happen during bootstrap, but it won't.")
	}
}

func TestNewManagerNoBootstrap(t *testing.T) {
	now := time.Now()
	cert, err := tls.X509KeyPair(storeCertData.certificatePEM, storeCertData.keyPEM)
	if err != nil {
		t.Fatalf("Unable to initialize a certificate: %v", err)
	}
	cert.Leaf = &x509.Certificate{
		NotBefore: now.Add(-24 * time.Hour),
		NotAfter:  now.Add(24 * time.Hour),
	}
	store := &fakeStore{
		cert: &cert,
	}

	cm, err := NewManager(&Config{
		Template:                &x509.CertificateRequest{},
		Usages:                  []certificatesv1.KeyUsage{},
		CertificateStore:        store,
		BootstrapCertificatePEM: bootstrapCertData.certificatePEM,
		BootstrapKeyPEM:         bootstrapCertData.keyPEM,
	})

	if err != nil {
		t.Fatalf("Failed to initialize the certificate manager: %v", err)
	}

	currentCert := cm.Current()

	if currentCert == nil {
		t.Errorf("Certificate was nil, expected something.")
	}
	if m, ok := cm.(*manager); !ok {
		t.Errorf("Expected a '*manager' from 'NewManager'")
	} else {
		if m.forceRotation {
			t.Errorf("Expected rotation should not happen during bootstrap, but it won't.")
		}
	}
}

func TestGetCurrentCertificateOrBootstrap(t *testing.T) {
	testCases := []struct {
		description          string
		storeCert            *tls.Certificate
		bootstrapCertData    []byte
		bootstrapKeyData     []byte
		expectedCert         *tls.Certificate
		expectedShouldRotate bool
		expectedErrMsg       string
	}{
		{
			"return cert from store",
			storeCertData.certificate,
			nil,
			nil,
			storeCertData.certificate,
			false,
			"",
		},
		{
			"no cert in store and no bootstrap cert",
			nil,
			nil,
			nil,
			nil,
			true,
			"",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			store := &fakeStore{
				cert: tc.storeCert,
			}

			certResult, shouldRotate, err := getCurrentCertificateOrBootstrap(
				logger,
				store,
				tc.bootstrapCertData,
				tc.bootstrapKeyData)
			if certResult == nil || certResult.Certificate == nil || tc.expectedCert == nil {
				if certResult != nil && tc.expectedCert != nil {
					t.Errorf("Got certificate %v, wanted %v", certResult, tc.expectedCert)
				}
			} else {
				if !certificatesEqual(certResult, tc.expectedCert) {
					t.Errorf("Got certificate %v, wanted %v", certResult, tc.expectedCert)
				}
			}
			if shouldRotate != tc.expectedShouldRotate {
				t.Errorf("Got shouldRotate %t, wanted %t", shouldRotate, tc.expectedShouldRotate)
			}
			if err == nil {
				if tc.expectedErrMsg != "" {
					t.Errorf("Got err %v, wanted %q", err, tc.expectedErrMsg)
				}
			} else {
				if tc.expectedErrMsg == "" || !strings.Contains(err.Error(), tc.expectedErrMsg) {
					t.Errorf("Got err %v, wanted %q", err, tc.expectedErrMsg)
				}
			}
		})
	}
}

func TestInitializeCertificateSigningRequestClient(t *testing.T) {
	var nilCertificate = &certificateData{}
	testCases := []struct {
		description             string
		storeCert               *certificateData
		bootstrapCert           *certificateData
		apiCert                 *certificateData
		noV1                    bool
		noV1beta1               bool
		expectedCertBeforeStart *certificateData
		expectedCertAfterStart  *certificateData
	}{
		{
			description:             "No current certificate, no bootstrap certificate",
			storeCert:               nilCertificate,
			bootstrapCert:           nilCertificate,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: nilCertificate,
			expectedCertAfterStart:  apiServerCertData,
		},
		{
			description:             "No current certificate, no bootstrap certificate, no v1 API",
			storeCert:               nilCertificate,
			bootstrapCert:           nilCertificate,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: nilCertificate,
			expectedCertAfterStart:  apiServerCertData,
			noV1:                    true,
		},
		{
			description:             "No current certificate, no bootstrap certificate, no v1beta1 API",
			storeCert:               nilCertificate,
			bootstrapCert:           nilCertificate,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: nilCertificate,
			expectedCertAfterStart:  apiServerCertData,
			noV1beta1:               true,
		},
		{
			description:             "No current certificate, bootstrap certificate",
			storeCert:               nilCertificate,
			bootstrapCert:           bootstrapCertData,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: bootstrapCertData,
			expectedCertAfterStart:  apiServerCertData,
		},
		{
			description:             "Current certificate, no bootstrap certificate",
			storeCert:               storeCertData,
			bootstrapCert:           nilCertificate,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: storeCertData,
			expectedCertAfterStart:  storeCertData,
		},
		{
			description:             "Current certificate, bootstrap certificate",
			storeCert:               storeCertData,
			bootstrapCert:           bootstrapCertData,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: storeCertData,
			expectedCertAfterStart:  storeCertData,
		},
		{
			description:             "Current certificate expired, no bootstrap certificate",
			storeCert:               expiredStoreCertData,
			bootstrapCert:           nilCertificate,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: nil,
			expectedCertAfterStart:  apiServerCertData,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			certificateStore := &fakeStore{
				cert: tc.storeCert.certificate,
			}

			certificateManager, err := NewManager(&Config{
				Template: &x509.CertificateRequest{
					Subject: pkix.Name{
						Organization: []string{"system:nodes"},
						CommonName:   "system:node:fake-node-name",
					},
				},
				SignerName: certificatesv1.KubeAPIServerClientSignerName,
				Usages: []certificatesv1.KeyUsage{
					certificatesv1.UsageDigitalSignature,
					certificatesv1.UsageKeyEncipherment,
					certificatesv1.UsageClientAuth,
				},
				CertificateStore:        certificateStore,
				BootstrapCertificatePEM: tc.bootstrapCert.certificatePEM,
				BootstrapKeyPEM:         tc.bootstrapCert.keyPEM,
				ClientsetFn: func(_ *tls.Certificate) (clientset.Interface, error) {
					return newClientset(fakeClient{
						noV1:           tc.noV1,
						noV1beta1:      tc.noV1beta1,
						certificatePEM: tc.apiCert.certificatePEM,
					}), nil
				},
				Ctx: &ctx,
			})
			if err != nil {
				t.Errorf("Got %v, wanted no error.", err)
			}

			certificate := certificateManager.Current()
			if tc.expectedCertBeforeStart == nil {
				if certificate != nil {
					t.Errorf("Expected certificate to be nil, was %s", certificate.Leaf.NotAfter)
				}
			} else {
				if !certificatesEqual(certificate, tc.expectedCertBeforeStart.certificate) {
					t.Errorf("Got %v, wanted %v", certificateString(certificate), certificateString(tc.expectedCertBeforeStart.certificate))
				}
			}

			if m, ok := certificateManager.(*manager); !ok {
				t.Errorf("Expected a '*manager' from 'NewManager'")
			} else {
				if m.forceRotation {
					if success, err := m.rotateCerts(ctx); !success {
						t.Errorf("Got failure from 'rotateCerts', wanted success.")
					} else if err != nil {
						t.Errorf("Got error %v, expected none.", err)
					}
				}
			}

			certificate = certificateManager.Current()
			if tc.expectedCertAfterStart == nil {
				if certificate != nil {
					t.Errorf("Expected certificate to be nil, was %s", certificate.Leaf.NotAfter)
				}
				return
			}
			if !certificatesEqual(certificate, tc.expectedCertAfterStart.certificate) {
				t.Errorf("Got %v, wanted %v", certificateString(certificate), certificateString(tc.expectedCertAfterStart.certificate))
			}
		})
	}
}

func TestInitializeOtherRESTClients(t *testing.T) {
	var nilCertificate = &certificateData{}
	testCases := []struct {
		description             string
		storeCert               *certificateData
		bootstrapCert           *certificateData
		apiCert                 *certificateData
		expectedCertBeforeStart *certificateData
		expectedCertAfterStart  *certificateData
	}{
		{
			description:             "No current certificate, no bootstrap certificate",
			storeCert:               nilCertificate,
			bootstrapCert:           nilCertificate,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: nilCertificate,
			expectedCertAfterStart:  apiServerCertData,
		},
		{
			description:             "No current certificate, bootstrap certificate",
			storeCert:               nilCertificate,
			bootstrapCert:           bootstrapCertData,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: bootstrapCertData,
			expectedCertAfterStart:  apiServerCertData,
		},
		{
			description:             "Current certificate, no bootstrap certificate",
			storeCert:               storeCertData,
			bootstrapCert:           nilCertificate,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: storeCertData,
			expectedCertAfterStart:  storeCertData,
		},
		{
			description:             "Current certificate, bootstrap certificate",
			storeCert:               storeCertData,
			bootstrapCert:           bootstrapCertData,
			apiCert:                 apiServerCertData,
			expectedCertBeforeStart: storeCertData,
			expectedCertAfterStart:  storeCertData,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			certificateStore := &fakeStore{
				cert: tc.storeCert.certificate,
			}

			certificateManager, err := NewManager(&Config{
				Template: &x509.CertificateRequest{
					Subject: pkix.Name{
						Organization: []string{"system:nodes"},
						CommonName:   "system:node:fake-node-name",
					},
				},
				Usages: []certificatesv1.KeyUsage{
					certificatesv1.UsageDigitalSignature,
					certificatesv1.UsageKeyEncipherment,
					certificatesv1.UsageClientAuth,
				},
				CertificateStore:        certificateStore,
				BootstrapCertificatePEM: tc.bootstrapCert.certificatePEM,
				BootstrapKeyPEM:         tc.bootstrapCert.keyPEM,
				ClientsetFn: func(_ *tls.Certificate) (clientset.Interface, error) {
					return newClientset(fakeClient{
						certificatePEM: tc.apiCert.certificatePEM,
					}), nil
				},
			})
			if err != nil {
				t.Errorf("Got %v, wanted no error.", err)
			}

			certificate := certificateManager.Current()
			if !certificatesEqual(certificate, tc.expectedCertBeforeStart.certificate) {
				t.Errorf("Got %v, wanted %v", certificateString(certificate), certificateString(tc.expectedCertBeforeStart.certificate))
			}

			if m, ok := certificateManager.(*manager); !ok {
				t.Errorf("Expected a '*manager' from 'NewManager'")
			} else {
				if m.forceRotation {
					success, err := certificateManager.(*manager).rotateCerts(ctx)
					if err != nil {
						t.Errorf("Got error %v, expected none.", err)
						return
					}
					if !success {
						t.Errorf("Unexpected response 'rotateCerts': %t", success)
						return
					}
				}
			}

			certificate = certificateManager.Current()
			if !certificatesEqual(certificate, tc.expectedCertAfterStart.certificate) {
				t.Errorf("Got %v, wanted %v", certificateString(certificate), certificateString(tc.expectedCertAfterStart.certificate))
			}
		})
	}
}

func TestServerHealth(t *testing.T) {
	type certs struct {
		storeCert               *certificateData
		bootstrapCert           *certificateData
		apiCert                 *certificateData
		expectedCertBeforeStart *certificateData
		expectedCertAfterStart  *certificateData
	}

	updatedCerts := certs{
		storeCert:               storeCertData,
		bootstrapCert:           bootstrapCertData,
		apiCert:                 apiServerCertData,
		expectedCertBeforeStart: storeCertData,
		expectedCertAfterStart:  apiServerCertData,
	}

	currentCerts := certs{
		storeCert:               storeCertData,
		bootstrapCert:           bootstrapCertData,
		apiCert:                 apiServerCertData,
		expectedCertBeforeStart: storeCertData,
		expectedCertAfterStart:  storeCertData,
	}

	testCases := []struct {
		description string
		certs

		failureType fakeClientFailureType
		clientErr   error

		expectRotateFail bool
		expectHealthy    bool
	}{
		{
			description:   "Current certificate, bootstrap certificate",
			certs:         updatedCerts,
			expectHealthy: true,
		},
		{
			description: "Generic error on create",
			certs:       currentCerts,

			failureType:      createError,
			expectRotateFail: true,
		},
		{
			description: "Unauthorized error on create",
			certs:       currentCerts,

			failureType:      createError,
			clientErr:        apierrors.NewUnauthorized("unauthorized"),
			expectRotateFail: true,
			expectHealthy:    true,
		},
		{
			description: "Generic unauthorized error on create",
			certs:       currentCerts,

			failureType:      createError,
			clientErr:        apierrors.NewGenericServerResponse(401, "POST", schema.GroupResource{}, "", "", 0, true),
			expectRotateFail: true,
			expectHealthy:    true,
		},
		{
			description: "Generic not found error on create",
			certs:       currentCerts,

			failureType:      createError,
			clientErr:        apierrors.NewGenericServerResponse(404, "POST", schema.GroupResource{}, "", "", 0, true),
			expectRotateFail: true,
			expectHealthy:    false,
		},
		{
			description: "Not found error on create",
			certs:       currentCerts,

			failureType:      createError,
			clientErr:        apierrors.NewGenericServerResponse(404, "POST", schema.GroupResource{}, "", "", 0, false),
			expectRotateFail: true,
			expectHealthy:    true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			certificateStore := &fakeStore{
				cert: tc.storeCert.certificate,
			}

			certificateManager, err := NewManager(&Config{
				Template: &x509.CertificateRequest{
					Subject: pkix.Name{
						Organization: []string{"system:nodes"},
						CommonName:   "system:node:fake-node-name",
					},
				},
				Usages: []certificatesv1.KeyUsage{
					certificatesv1.UsageDigitalSignature,
					certificatesv1.UsageKeyEncipherment,
					certificatesv1.UsageClientAuth,
				},
				CertificateStore:        certificateStore,
				BootstrapCertificatePEM: tc.bootstrapCert.certificatePEM,
				BootstrapKeyPEM:         tc.bootstrapCert.keyPEM,
				ClientsetFn: func(_ *tls.Certificate) (clientset.Interface, error) {
					return newClientset(fakeClient{
						certificatePEM: tc.apiCert.certificatePEM,
						failureType:    tc.failureType,
						err:            tc.clientErr,
					}), nil
				},
			})
			if err != nil {
				t.Errorf("Got %v, wanted no error.", err)
			}

			certificate := certificateManager.Current()
			if !certificatesEqual(certificate, tc.expectedCertBeforeStart.certificate) {
				t.Errorf("Got %v, wanted %v", certificateString(certificate), certificateString(tc.expectedCertBeforeStart.certificate))
			}

			if _, ok := certificateManager.(*manager); !ok {
				t.Errorf("Expected a '*manager' from 'NewManager'")
			} else {
				success, err := certificateManager.(*manager).rotateCerts(ctx)
				if err != nil {
					t.Errorf("Got error %v, expected none.", err)
					return
				}
				if !success != tc.expectRotateFail {
					t.Errorf("Unexpected response 'rotateCerts': %t", success)
					return
				}
				if actual := certificateManager.(*manager).ServerHealthy(); actual != tc.expectHealthy {
					t.Errorf("Unexpected manager server health: %t", actual)
				}
			}

			certificate = certificateManager.Current()
			if !certificatesEqual(certificate, tc.expectedCertAfterStart.certificate) {
				t.Errorf("Got %v, wanted %v", certificateString(certificate), certificateString(tc.expectedCertAfterStart.certificate))
			}
		})
	}
}

func TestRotationLogsDuration(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	h := metricMock{}
	now := time.Now()
	certIss := now.Add(-2 * time.Hour)
	m := manager{
		cert: &tls.Certificate{
			Leaf: &x509.Certificate{
				NotBefore: certIss,
				NotAfter:  now.Add(-1 * time.Hour),
			},
		},
		certStore:   &fakeStore{cert: expiredStoreCertData.certificate},
		getTemplate: func() *x509.CertificateRequest { return &x509.CertificateRequest{} },
		clientsetFn: func(_ *tls.Certificate) (clientset.Interface, error) {
			return newClientset(fakeClient{
				certificatePEM: apiServerCertData.certificatePEM,
			}), nil
		},
		certificateRotation: &h,
		now:                 func() time.Time { return now },
		ctx:                 ctx,
	}
	ok, err := m.rotateCerts(ctx)
	if err != nil || !ok {
		t.Errorf("failed to rotate certs: %v", err)
	}
	if h.calls != 1 {
		t.Errorf("rotation metric was not called")
	}
	if h.lastValue != now.Sub(certIss).Seconds() {
		t.Errorf("rotation metric did not record the right value got: %f; want %f", h.lastValue, now.Sub(certIss).Seconds())
	}

}

type fakeClientFailureType int

const (
	none fakeClientFailureType = iota
	createError
	watchError
	certificateSigningRequestDenied
)

type fakeClient struct {
	noV1      bool
	noV1beta1 bool
	certificatesclient.CertificateSigningRequestInterface
	failureType    fakeClientFailureType
	certificatePEM []byte
	err            error
}

func newClientset(opts fakeClient) *fake.Clientset {
	f := fake.NewSimpleClientset()
	switch opts.failureType {
	case createError:
		f.PrependReactor("create", "certificatesigningrequests", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			if opts.err != nil {
				return true, nil, opts.err
			}
			return true, nil, fmt.Errorf("create error")
		})
	case watchError:
		f.PrependReactor("list", "certificatesigningrequests", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			if opts.err != nil {
				return true, nil, opts.err
			}
			return true, nil, fmt.Errorf("watch error")
		})
		f.PrependWatchReactor("certificatesigningrequests", func(action clienttesting.Action) (handled bool, ret watch.Interface, err error) {
			if opts.err != nil {
				return true, nil, opts.err
			}
			return true, nil, fmt.Errorf("watch error")
		})
	default:
		f.PrependReactor("create", "certificatesigningrequests", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			switch action.GetResource().Version {
			case "v1":
				if opts.noV1 {
					return true, nil, apierrors.NewNotFound(certificatesv1.Resource("certificatesigningrequests"), "")
				}
				return true, &certificatesv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{UID: "fake-uid"}}, nil
			case "v1beta1":
				if opts.noV1beta1 {
					return true, nil, apierrors.NewNotFound(certificatesv1.Resource("certificatesigningrequests"), "")
				}
				return true, &certificatesv1beta1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{UID: "fake-uid"}}, nil
			default:
				return false, nil, nil
			}
		})
		f.PrependReactor("list", "certificatesigningrequests", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			switch action.GetResource().Version {
			case "v1":
				if opts.noV1 {
					return true, nil, apierrors.NewNotFound(certificatesv1.Resource("certificatesigningrequests"), "")
				}
				return true, &certificatesv1.CertificateSigningRequestList{Items: []certificatesv1.CertificateSigningRequest{{ObjectMeta: metav1.ObjectMeta{UID: "fake-uid"}}}}, nil
			case "v1beta1":
				if opts.noV1beta1 {
					return true, nil, apierrors.NewNotFound(certificatesv1.Resource("certificatesigningrequests"), "")
				}
				return true, &certificatesv1beta1.CertificateSigningRequestList{Items: []certificatesv1beta1.CertificateSigningRequest{{ObjectMeta: metav1.ObjectMeta{UID: "fake-uid"}}}}, nil
			default:
				return false, nil, nil
			}
		})
		f.PrependWatchReactor("certificatesigningrequests", func(action clienttesting.Action) (handled bool, ret watch.Interface, err error) {
			switch action.GetResource().Version {
			case "v1":
				if opts.noV1 {
					return true, nil, apierrors.NewNotFound(certificatesv1.Resource("certificatesigningrequests"), "")
				}
				return true, &fakeWatch{
					version:        action.GetResource().Version,
					failureType:    opts.failureType,
					certificatePEM: opts.certificatePEM,
				}, nil

			case "v1beta1":
				if opts.noV1beta1 {
					return true, nil, apierrors.NewNotFound(certificatesv1.Resource("certificatesigningrequests"), "")
				}
				return true, &fakeWatch{
					version:        action.GetResource().Version,
					failureType:    opts.failureType,
					certificatePEM: opts.certificatePEM,
				}, nil
			default:
				return false, nil, nil
			}
		})
	}
	return f
}

type fakeWatch struct {
	version        string
	failureType    fakeClientFailureType
	certificatePEM []byte
}

func (w *fakeWatch) Stop() {
}

func (w *fakeWatch) ResultChan() <-chan watch.Event {
	var csr runtime.Object

	switch w.version {
	case "v1":
		var condition certificatesv1.CertificateSigningRequestCondition
		if w.failureType == certificateSigningRequestDenied {
			condition = certificatesv1.CertificateSigningRequestCondition{
				Type: certificatesv1.CertificateDenied,
			}
		} else {
			condition = certificatesv1.CertificateSigningRequestCondition{
				Type: certificatesv1.CertificateApproved,
			}
		}

		csr = &certificatesv1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{UID: "fake-uid"},
			Status: certificatesv1.CertificateSigningRequestStatus{
				Conditions: []certificatesv1.CertificateSigningRequestCondition{
					condition,
				},
				Certificate: []byte(w.certificatePEM),
			},
		}

	case "v1beta1":
		var condition certificatesv1beta1.CertificateSigningRequestCondition
		if w.failureType == certificateSigningRequestDenied {
			condition = certificatesv1beta1.CertificateSigningRequestCondition{
				Type: certificatesv1beta1.CertificateDenied,
			}
		} else {
			condition = certificatesv1beta1.CertificateSigningRequestCondition{
				Type: certificatesv1beta1.CertificateApproved,
			}
		}

		csr = &certificatesv1beta1.CertificateSigningRequest{
			ObjectMeta: metav1.ObjectMeta{UID: "fake-uid"},
			Status: certificatesv1beta1.CertificateSigningRequestStatus{
				Conditions: []certificatesv1beta1.CertificateSigningRequestCondition{
					condition,
				},
				Certificate: []byte(w.certificatePEM),
			},
		}
	}

	c := make(chan watch.Event, 1)
	c <- watch.Event{
		Type:   watch.Added,
		Object: csr,
	}
	return c
}

type fakeStore struct {
	cert *tls.Certificate
}

func (s *fakeStore) Current() (*tls.Certificate, error) {
	if s.cert == nil {
		noKeyErr := NoCertKeyError("")
		return nil, &noKeyErr
	}
	return s.cert, nil
}

// Accepts the PEM data for the cert/key pair and makes the new cert/key
// pair the 'current' pair, that will be returned by future calls to
// Current().
func (s *fakeStore) Update(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	// In order to make the mocking work, whenever a cert/key pair is passed in
	// to be updated in the mock store, assume that the certificate manager
	// generated the key, and then asked the mock CertificateSigningRequest API
	// to sign it, then the faked API returned a canned response. The canned
	// signing response will not match the generated key. In order to make
	// things work out, search here for the correct matching key and use that
	// instead of the passed in key. That way this file of test code doesn't
	// have to implement an actual certificate signing process.
	for _, tc := range []*certificateData{storeCertData, bootstrapCertData, apiServerCertData} {
		if bytes.Equal(tc.certificatePEM, certPEM) {
			keyPEM = tc.keyPEM
		}
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s.cert = &cert
	s.cert.Leaf = &x509.Certificate{
		NotBefore: now.Add(-24 * time.Hour),
		NotAfter:  now.Add(24 * time.Hour),
	}
	return s.cert, nil
}

func certificatesEqual(c1 *tls.Certificate, c2 *tls.Certificate) bool {
	if c1 == nil || c2 == nil {
		return c1 == c2
	}
	if len(c1.Certificate) != len(c2.Certificate) {
		return false
	}
	for i := 0; i < len(c1.Certificate); i++ {
		if !bytes.Equal(c1.Certificate[i], c2.Certificate[i]) {
			return false
		}
	}
	return true
}

func certificateString(c *tls.Certificate) string {
	if c == nil {
		return "certificate == nil"
	}
	if c.Leaf == nil {
		return "certificate.Leaf == nil"
	}
	return c.Leaf.Subject.CommonName
}

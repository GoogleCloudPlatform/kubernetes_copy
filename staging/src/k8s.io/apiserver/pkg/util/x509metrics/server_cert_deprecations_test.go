/*
Copyright 2020 The Kubernetes Authors.

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

package x509metrics

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	auditapi "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
)

// taken from pkg/util/webhook/certs_test.go
var caCert = []byte(`-----BEGIN CERTIFICATE-----
MIIDGTCCAgGgAwIBAgIUealQGELTHLUVcpsNNwz8XexiWvswDQYJKoZIhvcNAQEL
BQAwGzEZMBcGA1UEAwwQd2ViaG9va190ZXN0c19jYTAgFw0yMjAzMjUxNTMzMjla
GA8yMjk2MDEwODE1MzMyOVowGzEZMBcGA1UEAwwQd2ViaG9va190ZXN0c19jYTCC
ASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAKeW0Jkq6ViZkyhaLCUgbqsN
7+6HLwfZK/ljy/KnZ7W7QlJ65Q2tkptL0fY4DPumT7JgVTGnXyTXJ35Ss5A4yhm9
yyMH8pNVR19udK1fU74YVmbXJkc0nP7n+AXX9lD2Yy9pDvtaq1E+erN2nR1XaCS9
n0ph4C/fB1Rh7mIv/u7WW7/aRz/rJjBBZIbg7hgZPwFsukEifGi0U4uitVYR6MWp
jHj++e1G38+4JrZR9vhBoHtBJ1DBpmjAQaAtkSZAxXJnma4awE0Bv0Q4lxkUeY+D
th8OxPXxgTbwTKDaguHlWLTapppygA8FnKqmcUkwHZO5OVldi/ZKOUm2YCuJlfEC
AwEAAaNTMFEwHQYDVR0OBBYEFJGX6zVDm0Ur4gJJR+PKlgGdwhPfMB8GA1UdIwQY
MBaAFJGX6zVDm0Ur4gJJR+PKlgGdwhPfMA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZI
hvcNAQELBQADggEBAJaRryxp0iYWGfLiZ0uIdOiYRVZUtmpqUSqT9/y29ffDAnCS
5labJS8FjaiQdlyaH+E9gzo0+nkO9NyfemJRLTEsU4Mz9AAvxs/NuWucqiyF0Y6d
JSYt7+2liGK5WvJMbHfW3jloWlv3oX+qL4iGFkJN+L9G+vf0GnKZCxgNIOqM4otv
cviCA9ouPwnSfnCeTsBoaUJqhLMizvUx7avvUyZTuV+t9+aN/qH4V//hTBqz9CNq
koodzngbUuyaGFI8QISUAhU+pbt6npb69LlhJHC0icDXCc4uTsd+SDsBOorZDuUL
HsNKnE0CSPZ65ENVNfJjB1fVEYbbpjr8kizOCDE=
-----END CERTIFICATE-----`)

var caCertInter = []byte(`-----BEGIN CERTIFICATE-----
MIIDMzCCAhugAwIBAgIUTJoqwFusJcupNCs/u39LBFrkZEIwDQYJKoZIhvcNAQEL
BQAwGzEZMBcGA1UEAwwQd2ViaG9va190ZXN0c19jYTAgFw0yMjAzMjUxNTMzMjla
GA8yMjk2MDEwODE1MzMyOVowKDEmMCQGA1UEAwwdd2ViaG9va190ZXN0c19pbnRl
cm1lZGlhdGVfY2EwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDT9DIt
uNXhesrh8XtPXK4pR7xGReEsIlgLpMYf11PtFln9eV0HXvUO2CG/YvMxwgyd6Yoq
EfzD4rjmXvl5bQPMygmxf5GN1PM7ef7gVYuHfDgsQ4a82u1JFhKvuOrXn3QRfRg4
M4uYND7J4+Bg6J8oaA0yXIiMCpBi+XwEufo0RvgxM6mT+CeJ82hmlTKVhQJZZ9ZT
al1C4dTR2XeH5TLiIAvm+egBmSZhtCVn14rGk/PcHOWV7hdCxaFhSm7dSC+dR4zK
SxNleJ4Y+tZgoMfvgP/xHZEjbBzxnxyasES/Nc4nTgylcr6aqEX/fbcF0QzHpL9Z
ibkt1cBExU9zHuFJAgMBAAGjYDBeMB0GA1UdDgQWBBTfgUwjHsTOey7WqL4f3oFD
bmY77TAfBgNVHSMEGDAWgBSRl+s1Q5tFK+ICSUfjypYBncIT3zAPBgNVHRMBAf8E
BTADAQH/MAsGA1UdDwQEAwIBBjANBgkqhkiG9w0BAQsFAAOCAQEARYbIpIidgAVb
5ra9zd7F902+xC13/nmlrKL/dRMrRdZxk1kVVww3FbXSp7k7oHsih42KUCVDBevw
0ZZiolZlLneU57dEKKiTMkuPdVbNbIBPXIQpHLrXpVIR5BRRdRZ5OJZY24hYCvce
50XV8ITIU0R/U4sQ6NFHv8NJ5BB+2u1M3HF2LSKZFLnfP5FBcTCg84Jd6gEmTU2j
wZELnHy1AVdQnKMP9VrdAr9Wn6omWxAfO/PSb9YeKhGH5vtX+Bpb9bSPQIpXeBdB
LLCkme0M+1UsF7xua0KVi4DSuJc+RBl4aOH0ZvKmrIWzLzZhRS0vaO/fPArVCvvI
VrUba0E3WQ==
-----END CERTIFICATE-----`)

var caCertInterSHA1 = []byte(`-----BEGIN CERTIFICATE-----
MIIDMzCCAhugAwIBAgIUTJoqwFusJcupNCs/u39LBFrkZEMwDQYJKoZIhvcNAQEF
BQAwGzEZMBcGA1UEAwwQd2ViaG9va190ZXN0c19jYTAgFw0yMjAzMjUxNTMzMjla
GA8yMjk2MDEwODE1MzMyOVowKDEmMCQGA1UEAwwdd2ViaG9va190ZXN0c19pbnRl
cm1lZGlhdGVfY2EwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDT9DIt
uNXhesrh8XtPXK4pR7xGReEsIlgLpMYf11PtFln9eV0HXvUO2CG/YvMxwgyd6Yoq
EfzD4rjmXvl5bQPMygmxf5GN1PM7ef7gVYuHfDgsQ4a82u1JFhKvuOrXn3QRfRg4
M4uYND7J4+Bg6J8oaA0yXIiMCpBi+XwEufo0RvgxM6mT+CeJ82hmlTKVhQJZZ9ZT
al1C4dTR2XeH5TLiIAvm+egBmSZhtCVn14rGk/PcHOWV7hdCxaFhSm7dSC+dR4zK
SxNleJ4Y+tZgoMfvgP/xHZEjbBzxnxyasES/Nc4nTgylcr6aqEX/fbcF0QzHpL9Z
ibkt1cBExU9zHuFJAgMBAAGjYDBeMB0GA1UdDgQWBBTfgUwjHsTOey7WqL4f3oFD
bmY77TAfBgNVHSMEGDAWgBSRl+s1Q5tFK+ICSUfjypYBncIT3zAPBgNVHRMBAf8E
BTADAQH/MAsGA1UdDwQEAwIBBjANBgkqhkiG9w0BAQUFAAOCAQEAAhMQTwrpAeIQ
nShHfTERiwg/tx3dL971d3pFS5wi4kEIbbYCUGpzkmK/FTw4hfUnLpwcjjAbOWkk
45glOmrLJXM4RvH5PQF3GZmZvxv8Dl4zuhH1QvWbJHUiC+gyrBWI0moyLSmNiutZ
d3TZGEehZGwivMdHHuhgiyFM4i33EQTW1vdMdOvdu8yNpAeXM2h1PcJwbEML9PO3
LONzVKhz/RsyEwv7PkX1gdmi6eyAE61BWJGwzxE4K0xIYmcr6iOjsJhEf/5Qc93P
IGSHsG/HjWwZ47gbobtv7L+8uKP/0ky+k1cE4nIB1gKYey+SYwvkQTsj24oG9xcL
XhgnIl+qDw==
-----END CERTIFICATE-----`)

var serverKey = []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA52b4byIJpUDyTKo5FiCa5Ekiy7CCd8UoleSomQjh5zZGsIbg
z9RqjaTMLF0jqbzh9ix2DQSnY+w32LqPM2sOK1+/atmeCa8m5bvZrRoDxP2T3pQH
Tye0C9WI7jqosmMquRFakaY1ODxDRPWF9CRghFF62NcHfnztW2rMiEYtuDRkZGsp
JL/B5OAzEgLr4iP8TKMlqSuhaQhi52dEZo0pJ1Ie4up8xxXKeoqfO3WSVDeRpj/n
0tYSALhOCdGn0RMav5wfZmxdfZpKUhBrcxFfeiDB5c7xdlnziHrY5lqSQPCHxAQb
S1jMaV4adhxDzF56t5RU6/5eWPZ4IvlTRtYmNwIDAQABAoIBAAb0r28XxNZ011O6
ojCqFj3afPNGgQV8pbWrw+2luLSsiv9vbn6Q0gsj8wc6XYISrXAq8fl+NFHqndsj
8H4JL8nZ/PUHSZrc6vxo4ygy6f4X6UP9iyKz/NOGPbF7jeqe1H/vp5tNNbhVB2ih
QL+QAF653El8XTtOIgxnb3KBOYqZ6e0rWvC5XlZrfT4EGqpokW4zQ6ROQUbnWyCk
LC4CtQpcLLd7fdGfA2cB2xDdGJ3Er8gAnU/X+tAtcghWanoNARKGU6opyGpwhipe
+31CivIUhtASWdbS73ay5QaDQSlgNM1/2hk5Beal7D9bGfKtwT/VGDSpKc4EKP8j
ktQSE0ECgYEA/jHMLQyvJ2VuqBdMI5hbaw5jzUAQwaJof5iQNuvFrbtWDeotAf+6
HomwoqzZ9+juiil4PHLQJzkArHwMWXbc+3FAznN1foS+YlOmIgJrjKa+EP+sz/X2
GxuyH3RD9+TH4EGd4TbeDr0eZOnIbKVybj4ueE+um7jtdLzYW2Y8iCcCgYEA6Qu6
x5WOQaPaEOQwEP5AqVBZnZl1ogeEVanlPYl6amPFFnlc41+M61p3ebwRqikaC9Dv
hePiOcTTJyt4h7qzgd5rJTjy5bNYDx9F61NGagF0xJLQiMnXM/TsoFABVWetLepG
DTzgvCf7wmB9QTgdLct7KyG4suDBJlEAvr70q3ECgYEAxx4pC0z5U4oAMYn2aZeq
XOUrxpcdySC4bOMMbQkpk1rBIStEUGGK4OsIw5VVNP5xBSdQ+UESzva3EWYmoloa
5pgjpNUKv62qGQnfhJqStt3S2yv8qfbI7xk14a/IokHDVGbyDn5VWgRI79G1322G
gtcQvcvlQjSNRbm8XXRrjFcCgYA66x1Awl3h2IQUSyyfzzgX1lmhz59+5HmfksGD
SlOpvCmi4fILBihBhHC6VUL+C0ArhppX9mJGiq17tLDXV+t0RQA/u+MlEa+MuzJZ
KYee21ljLV8NhkIjP6Pnb/K2XezZs+YcCK0kxNMQtIZWS9KMtmogYHkquEn83vPa
Rbrj8QKBgHifm6Z9F4qTz2b2RsYPoMHOdsX0DrZ8xQOH8jioTAy/Xi2hrL5Klp7h
zaLifWtOdtckFxIk+6D/zLLn1icC7cc8n4TMwQ1ikY+9IPnkTXVx4b/r/NSbAVxZ
J821mkhGdqKJGAzk6uh/Sn4rNGubH+I1x2Xa9hWbARCLsj8tp6TX
-----END RSA PRIVATE KEY-----`)

var serverCert = []byte(`-----BEGIN CERTIFICATE-----
MIIDHzCCAgegAwIBAgIUTJoqwFusJcupNCs/u39LBFrkZEQwDQYJKoZIhvcNAQEL
BQAwGzEZMBcGA1UEAwwQd2ViaG9va190ZXN0c19jYTAgFw0yMjAzMjUxNTMzMjla
GA8yMjk2MDEwODE1MzMyOVowHzEdMBsGA1UEAwwUd2ViaG9va190ZXN0c19zZXJ2
ZXIwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQDnZvhvIgmlQPJMqjkW
IJrkSSLLsIJ3xSiV5KiZCOHnNkawhuDP1GqNpMwsXSOpvOH2LHYNBKdj7DfYuo8z
aw4rX79q2Z4Jryblu9mtGgPE/ZPelAdPJ7QL1YjuOqiyYyq5EVqRpjU4PENE9YX0
JGCEUXrY1wd+fO1basyIRi24NGRkaykkv8Hk4DMSAuviI/xMoyWpK6FpCGLnZ0Rm
jSknUh7i6nzHFcp6ip87dZJUN5GmP+fS1hIAuE4J0afRExq/nB9mbF19mkpSEGtz
EV96IMHlzvF2WfOIetjmWpJA8IfEBBtLWMxpXhp2HEPMXnq3lFTr/l5Y9ngi+VNG
1iY3AgMBAAGjVTBTMAkGA1UdEwQCMAAwCwYDVR0PBAQDAgXgMB0GA1UdJQQWMBQG
CCsGAQUFBwMCBggrBgEFBQcDATAaBgNVHREEEzARhwR/AAABgglsb2NhbGhvc3Qw
DQYJKoZIhvcNAQELBQADggEBAAeUHlNJiGfvhi8ts96javP8tO5gPkN7uErIMpzA
N1rf5Kdy7/LsxM6Uvwn0ns+p1vxANAjR/c0nfu0eIO1t5fKVDD0s9+ohKA/6phrm
xChTyl21mDZlFKjq0sjSwzBcUHPJjzUW9+AMDvS7pOjR5h4nD21LlMIkBzinl5KT
uo2Pm/OZqepPdM5XH9DaW0T0tjXKvRFe4FklJSKGD7f+T1whtmyziyA84YjYVa/6
gF+gpIOmPruJI9UoFqEncNpLfh5vKu2Vxv+maztFRhb+9gOg+nVBq1pxmMZV0PuM
L+tz0avIZEO2+KhgVGF3AF8HSZQHYcaskGFSGc8FxDKcDjM=
-----END CERTIFICATE-----`)

var serverCertNoSAN = []byte(`-----BEGIN CERTIFICATE-----
MIIC+DCCAeCgAwIBAgIUTJoqwFusJcupNCs/u39LBFrkZEUwDQYJKoZIhvcNAQEL
BQAwGzEZMBcGA1UEAwwQd2ViaG9va190ZXN0c19jYTAgFw0yMjAzMjUxNTMzMjla
GA8yMjk2MDEwODE1MzMyOVowFDESMBAGA1UEAwwJbG9jYWxob3N0MIIBIjANBgkq
hkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA52b4byIJpUDyTKo5FiCa5Ekiy7CCd8Uo
leSomQjh5zZGsIbgz9RqjaTMLF0jqbzh9ix2DQSnY+w32LqPM2sOK1+/atmeCa8m
5bvZrRoDxP2T3pQHTye0C9WI7jqosmMquRFakaY1ODxDRPWF9CRghFF62NcHfnzt
W2rMiEYtuDRkZGspJL/B5OAzEgLr4iP8TKMlqSuhaQhi52dEZo0pJ1Ie4up8xxXK
eoqfO3WSVDeRpj/n0tYSALhOCdGn0RMav5wfZmxdfZpKUhBrcxFfeiDB5c7xdlnz
iHrY5lqSQPCHxAQbS1jMaV4adhxDzF56t5RU6/5eWPZ4IvlTRtYmNwIDAQABozkw
NzAJBgNVHRMEAjAAMAsGA1UdDwQEAwIF4DAdBgNVHSUEFjAUBggrBgEFBQcDAgYI
KwYBBQUHAwEwDQYJKoZIhvcNAQELBQADggEBAGfCa0eCws/7+NYLJwVsdd7C/QHT
qbPw6w8oGnlXELMPwC701VFOcadhhengYCY1Kwa/KVu1ucFODDgp1ncvRoMVVWvD
/q6V07zu+aV/aW64zU27f+TzxTVXyCgfCSFUELJYBsBFWLw0K57ZDZdN2KJD+zD5
BAU0ghmy1DB+WSFMTqQ2iQaNX8oZh5jTZV3JtRNncqEqqIh4Nv7YYYZ02rgE7P2o
btVFYLBXHW7VYqnWpWM1pBfZpfGzMpGdR+1feST/88gUZh7ze15Ib4BlyU13v+0l
/BjuUsSWiITKWb2fqTiAkrqVbkOrC7Orz8yvgjuih4lEinQV1+KJUtcMmng=
-----END CERTIFICATE-----`)

var sha1ServerCertInter = []byte(`-----BEGIN CERTIFICATE-----
MIIDITCCAgmgAwIBAgIUaVjtC++/JGFoZRtkpo/j5q1nQ/4wDQYJKoZIhvcNAQEF
BQAwKDEmMCQGA1UEAwwdd2ViaG9va190ZXN0c19pbnRlcm1lZGlhdGVfY2EwIBcN
MjIwMzI1MTUzMzI5WhgPMjI5NjAxMDgxNTMzMjlaMBQxEjAQBgNVBAMMCWxvY2Fs
aG9zdDCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAOdm+G8iCaVA8kyq
ORYgmuRJIsuwgnfFKJXkqJkI4ec2RrCG4M/Uao2kzCxdI6m84fYsdg0Ep2PsN9i6
jzNrDitfv2rZngmvJuW72a0aA8T9k96UB08ntAvViO46qLJjKrkRWpGmNTg8Q0T1
hfQkYIRRetjXB3587VtqzIhGLbg0ZGRrKSS/weTgMxIC6+Ij/EyjJakroWkIYudn
RGaNKSdSHuLqfMcVynqKnzt1klQ3kaY/59LWEgC4TgnRp9ETGr+cH2ZsXX2aSlIQ
a3MRX3ogweXO8XZZ84h62OZakkDwh8QEG0tYzGleGnYcQ8xeereUVOv+Xlj2eCL5
U0bWJjcCAwEAAaNVMFMwCQYDVR0TBAIwADALBgNVHQ8EBAMCBeAwHQYDVR0lBBYw
FAYIKwYBBQUHAwIGCCsGAQUFBwMBMBoGA1UdEQQTMBGHBH8AAAGCCWxvY2FsaG9z
dDANBgkqhkiG9w0BAQUFAAOCAQEATpiJFBwcRFIfZ9ffvS1WDzHqNElEnvocv/ul
3KVtoX4gmKRoOy344s3oJ5APPHYWUFuZVc3uofjW265r2uOW1Cb4P9yAtNc4htBS
+hYsdS3MQlzZCS9rItaT25R6Ieq5TbHGRCof387jzvo1NNhcAQ5akQlQKI87km77
VzoEBdAw68Q0ZE+X34Q9eAA44oCcLAgCpGvs6hQuUSInribSR3vtsjuaLjdJ5F1f
GCu2QGM4cVLaezmoa1J54ETZggT2xFw2IyWJ2g/kXFpo+HnoyaDrPthud3Pe5xEt
JMzX0s3jPSjfeAv34Pr37s0Or18r1bS1hrgxE0SV2vk31fsImg==
-----END CERTIFICATE-----`)

var serverCertInterSHA1 = []byte(`-----BEGIN CERTIFICATE-----
MIIDITCCAgmgAwIBAgIUfSzzygvth1xlpa9DtyGpHuY2V+swDQYJKoZIhvcNAQEL
BQAwKDEmMCQGA1UEAwwdd2ViaG9va190ZXN0c19pbnRlcm1lZGlhdGVfY2EwIBcN
MjIwMzI1MTUzMzI5WhgPMjI5NjAxMDgxNTMzMjlaMBQxEjAQBgNVBAMMCWxvY2Fs
aG9zdDCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBAOdm+G8iCaVA8kyq
ORYgmuRJIsuwgnfFKJXkqJkI4ec2RrCG4M/Uao2kzCxdI6m84fYsdg0Ep2PsN9i6
jzNrDitfv2rZngmvJuW72a0aA8T9k96UB08ntAvViO46qLJjKrkRWpGmNTg8Q0T1
hfQkYIRRetjXB3587VtqzIhGLbg0ZGRrKSS/weTgMxIC6+Ij/EyjJakroWkIYudn
RGaNKSdSHuLqfMcVynqKnzt1klQ3kaY/59LWEgC4TgnRp9ETGr+cH2ZsXX2aSlIQ
a3MRX3ogweXO8XZZ84h62OZakkDwh8QEG0tYzGleGnYcQ8xeereUVOv+Xlj2eCL5
U0bWJjcCAwEAAaNVMFMwCQYDVR0TBAIwADALBgNVHQ8EBAMCBeAwHQYDVR0lBBYw
FAYIKwYBBQUHAwIGCCsGAQUFBwMBMBoGA1UdEQQTMBGHBH8AAAGCCWxvY2FsaG9z
dDANBgkqhkiG9w0BAQsFAAOCAQEAwe/JUeIiJ5ugiO4tM0ZtvgHuFC3hK+ZWndRE
z4JfVXTW9soxpa/cOU9QdJhZzouIu9yqZasY4zSEerC1e6grBYP95vMbN6xUAown
wNzrQzyJ6yP526txiIdOkKf+yVNdz0OWNHMPtwTWIr8kKGK23ABF94aUa0VlkErp
Qrd8NQ3guIPI+/upuxirJCFdhE+U3U0pLHpGaGvhkOytfnLYiINwR9norVCDGbQG
ITH0tOz8gVWWWwxa9s5CmbqTnasgUMDh1jHa5xOo+riX8H5lwQUaItKU1JM+QMIR
6Z+M0Isdw647A6tmX7DqNcmHlBKxPN1GDcVXalwYJUoXwTb9Hw==
-----END CERTIFICATE-----`)

// Test_checkForHostnameError tests that the upstream message for remote server
// certificate's hostname hasn't changed when no SAN extension is present and that
// the metrics counter increases properly when such an error is encountered
//
// Requires GODEBUG=x509ignoreCN=0 to not be set in the environment
func TestCheckForHostnameError(t *testing.T) {
	tests := []struct {
		name            string
		serverCert      []byte
		counterIncrease bool
	}{
		{
			name:            "no SAN",
			serverCert:      serverCertNoSAN,
			counterIncrease: true,
		},
		{
			name:       "with SAN",
			serverCert: serverCert,
		},
	}

	// register the test metrics
	x509MissingSANCounter := metrics.NewCounter(&metrics.CounterOpts{Name: "Test_checkForHostnameError"})
	registry := testutil.NewFakeKubeRegistry("0.0.0")
	registry.MustRegister(x509MissingSANCounter)
	sanChecker := NewSANDeprecatedChecker(x509MissingSANCounter)

	var lastCounterVal int
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsServer, serverURL := testServer(t, tt.serverCert)
			defer tlsServer.Close()

			client := tlsServer.Client()
			req, err := http.NewRequest(http.MethodGet, serverURL.String(), nil)
			if err != nil {
				t.Fatalf("failed to create an http request: %v", err)
			}
			req = req.WithContext(audit.WithAuditContext(req.Context()))
			auditCtx := audit.AuditContextFrom(req.Context())
			auditCtx.SetEventLevel(auditapi.LevelMetadata)

			_, err = client.Transport.RoundTrip(req)

			if sanChecker.CheckRoundTripError(err) {
				sanChecker.IncreaseMetricsCounter(req)
				annotations := auditCtx.GetEventAnnotations()
				if len(annotations["missing-san.invalid-cert.kubernetes.io/"+req.URL.Hostname()]) == 0 {
					t.Errorf("expected audit annotations, got %#v", annotations)
				}
			}

			errorCounterVal := getSingleCounterValueFromRegistry(t, registry, "Test_checkForHostnameError")
			if errorCounterVal == -1 {
				t.Fatalf("failed to get the error counter from the registry")
			}

			if tt.counterIncrease && errorCounterVal != lastCounterVal+1 {
				t.Errorf("expected the Test_checkForHostnameError metrics to increase by 1 from %d, but it is %d", lastCounterVal, errorCounterVal)
			}

			if !tt.counterIncrease && errorCounterVal != lastCounterVal {
				t.Errorf("expected the Test_checkForHostnameError metrics to stay the same (%d), but it is %d", lastCounterVal, errorCounterVal)
			}

			lastCounterVal = errorCounterVal
		})
	}
}

func TestCheckRespForNoSAN(t *testing.T) {
	tests := []struct {
		name            string
		serverCert      []byte
		counterIncrease bool
	}{
		{
			name:            "no SAN",
			serverCert:      serverCertNoSAN,
			counterIncrease: true,
		},
		{
			name:       "with SAN",
			serverCert: serverCert,
		},
	}

	// register the test metrics
	x509MissingSANCounter := metrics.NewCounter(&metrics.CounterOpts{Name: "Test_checkRespForNoSAN"})
	registry := testutil.NewFakeKubeRegistry("0.0.0")
	registry.MustRegister(x509MissingSANCounter)
	sanChecker := NewSANDeprecatedChecker(x509MissingSANCounter)

	var lastCounterVal int
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tlsConnectionState *tls.ConnectionState
			if tt.serverCert != nil {
				block, _ := pem.Decode([]byte(tt.serverCert))
				if block == nil {
					t.Fatal("failed to parse certificate PEM")
				}

				serverCert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					t.Fatalf("failed to parse certificate: %v", err)
				}

				tlsConnectionState = &tls.ConnectionState{
					PeerCertificates: []*x509.Certificate{serverCert},
				}
			}

			resp := &http.Response{
				TLS: tlsConnectionState,
			}

			if sanChecker.CheckPeerCertificates(resp.TLS.PeerCertificates) {
				sanChecker.IncreaseMetricsCounter(nil)
			}

			errorCounterVal := getSingleCounterValueFromRegistry(t, registry, "Test_checkRespForNoSAN")
			if errorCounterVal == -1 {
				t.Fatalf("failed to get the error counter from the registry")
			}

			if tt.counterIncrease && errorCounterVal != lastCounterVal+1 {
				t.Errorf("expected the Test_checkRespForNoSAN metrics to increase by 1 from %d, but it is %d", lastCounterVal, errorCounterVal)
			}

			if !tt.counterIncrease && errorCounterVal != lastCounterVal {
				t.Errorf("expected the Test_checkRespForNoSAN metrics to stay the same (%d), but it is %d", lastCounterVal, errorCounterVal)
			}

			lastCounterVal = errorCounterVal
		})
	}
}

func TestCheckForInsecureAlgorithmError(t *testing.T) {
	tests := []struct {
		name            string
		serverCert      []byte
		counterIncrease bool
	}{
		{
			name:            "server cert sha1-signed",
			serverCert:      append(append(sha1ServerCertInter, byte('\n')), caCertInter...),
			counterIncrease: true,
		},
		{
			name:            "intermediate CA cert sha1-signed",
			serverCert:      append(append(serverCertInterSHA1, byte('\n')), caCertInterSHA1...),
			counterIncrease: true,
		},
		{
			name:       "different error - cert untrusted, intermediate not in returned chain",
			serverCert: serverCertInterSHA1,
		},
		{
			name:       "properly signed",
			serverCert: serverCert,
		},
	}

	// register the test metrics
	x509SHA1SignatureCounter := metrics.NewCounter(&metrics.CounterOpts{Name: "Test_checkForInsecureAlgorithmError"})
	registry := testutil.NewFakeKubeRegistry("0.0.0")
	registry.MustRegister(x509SHA1SignatureCounter)
	sha1checker := NewSHA1SignatureDeprecatedChecker(x509SHA1SignatureCounter)

	var lastCounterVal int
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlsServer, serverURL := testServer(t, tt.serverCert)
			defer tlsServer.Close()

			req, err := http.NewRequest(http.MethodGet, serverURL.String(), nil)
			if err != nil {
				t.Fatalf("failed to create an http request: %v", err)
			}
			req = req.WithContext(audit.WithAuditContext(req.Context()))
			auditCtx := audit.AuditContextFrom(req.Context())
			auditCtx.SetEventLevel(auditapi.LevelMetadata)

			// can't use tlsServer.Client() as it contains the server certificate
			// in tls.Config.Certificates. The signatures are, however, only checked
			// during building a candidate verification certificate chain and
			// if the content of tls.Config.Certificates matches the certificate
			// returned by the server, this short-circuits and the signature verification is
			// never performed.
			caPool := x509.NewCertPool()
			require.True(t, caPool.AppendCertsFromPEM(caCert))

			client := &http.Client{
				Transport: &http.Transport{
					Proxy: http.ProxyFromEnvironment,
					TLSClientConfig: &tls.Config{
						RootCAs: caPool,
					},
				},
			}

			_, err = client.Transport.RoundTrip(req)

			if sha1checker.CheckRoundTripError(err) {
				sha1checker.IncreaseMetricsCounter(req)
				annotations := auditCtx.GetEventAnnotations()
				if len(annotations["insecure-sha1.invalid-cert.kubernetes.io/"+req.URL.Hostname()]) == 0 {
					t.Errorf("expected audit annotations, got %#v", annotations)
				}
			}

			errorCounterVal := getSingleCounterValueFromRegistry(t, registry, "Test_checkForInsecureAlgorithmError")
			if errorCounterVal == -1 {
				t.Fatalf("failed to get the error counter from the registry")
			}

			if tt.counterIncrease && errorCounterVal != lastCounterVal+1 {
				t.Errorf("expected the Test_checkForInsecureAlgorithmError metrics to increase by 1 from %d, but it is %d", lastCounterVal, errorCounterVal)
			}

			if !tt.counterIncrease && errorCounterVal != lastCounterVal {
				t.Errorf("expected the Test_checkForInsecureAlgorithmError metrics to stay the same (%d), but it is %d", lastCounterVal, errorCounterVal)
			}

			lastCounterVal = errorCounterVal
		})
	}
}

func TestCheckRespSHA1SignedCert(t *testing.T) {
	tests := []struct {
		name            string
		serverCert      []byte
		counterIncrease bool
	}{
		{
			name:            "server cert sha1-signed",
			serverCert:      append(append(sha1ServerCertInter, byte('\n')), caCertInter...),
			counterIncrease: true,
		},
		{
			name:            "intermediate CA cert sha1-signed",
			serverCert:      append(append(serverCertInterSHA1, byte('\n')), caCertInterSHA1...),
			counterIncrease: true,
		},
		{
			name:       "properly signed",
			serverCert: serverCert,
		},
	}

	// register the test metrics
	x509SHA1SignatureCounter := metrics.NewCounter(&metrics.CounterOpts{Name: "Test_checkRespSHA1SignedCert"})
	registry := testutil.NewFakeKubeRegistry("0.0.0")
	registry.MustRegister(x509SHA1SignatureCounter)
	sha1checker := NewSHA1SignatureDeprecatedChecker(x509SHA1SignatureCounter)

	var lastCounterVal int
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tlsConnectionState *tls.ConnectionState
			if tt.serverCert != nil {
				peerCerts := []*x509.Certificate{}
				for block, rest := pem.Decode([]byte(tt.serverCert)); block != nil; block, rest = pem.Decode(rest) {
					cert, err := x509.ParseCertificate(block.Bytes)
					if err != nil {
						t.Fatalf("failed to parse certificate: %v", err)
					}

					peerCerts = append(peerCerts, cert)
				}

				tlsConnectionState = &tls.ConnectionState{
					PeerCertificates: peerCerts,
				}
			}

			resp := &http.Response{
				TLS: tlsConnectionState,
			}

			if sha1checker.CheckPeerCertificates(resp.TLS.PeerCertificates) {
				sha1checker.IncreaseMetricsCounter(nil)
			}

			errorCounterVal := getSingleCounterValueFromRegistry(t, registry, "Test_checkRespSHA1SignedCert")
			if errorCounterVal == -1 {
				t.Fatalf("failed to get the error counter from the registry")
			}

			if tt.counterIncrease && errorCounterVal != lastCounterVal+1 {
				t.Errorf("expected the Test_checkRespSHA1SignedCert metrics to increase by 1 from %d, but it is %d", lastCounterVal, errorCounterVal)
			}

			if !tt.counterIncrease && errorCounterVal != lastCounterVal {
				t.Errorf("expected the Test_checkRespSHA1SignedCert metrics to stay the same (%d), but it is %d", lastCounterVal, errorCounterVal)
			}

			lastCounterVal = errorCounterVal
		})
	}
}

func Test_x509DeprecatedCertificateMetricsRTWrapper_RoundTrip(t *testing.T) {
	// register the test metrics
	testCounter := metrics.NewCounter(&metrics.CounterOpts{Name: "testCounter"})
	registry := testutil.NewFakeKubeRegistry("0.0.0")
	registry.MustRegister(testCounter)

	tests := []struct {
		name            string
		checkers        []deprecatedCertificateAttributeChecker
		resp            *http.Response
		err             error
		counterIncrease bool
	}{
		{
			name:     "no error, resp w/ cert, no counter increase",
			checkers: []deprecatedCertificateAttributeChecker{&testNegativeChecker{counterRaiser{testCounter, "", ""}}},
			resp:     httpResponseWithCert(),
		},
		{
			name:     "no error, resp w/o cert, no counter increase",
			checkers: []deprecatedCertificateAttributeChecker{&testPositiveChecker{counterRaiser{testCounter, "", ""}}},
			resp:     httpResponseNoCert(),
		},
		{
			name:            "no error, resp w/ cert, counter increase",
			checkers:        []deprecatedCertificateAttributeChecker{&testPositiveChecker{counterRaiser{testCounter, "", ""}}},
			resp:            httpResponseWithCert(),
			counterIncrease: true,
		},
		{
			name:     "unrelated error, no resp, no counter increase",
			checkers: []deprecatedCertificateAttributeChecker{&testNegativeChecker{counterRaiser{testCounter, "", ""}}},
			err:      fmt.Errorf("error"),
		},
		{
			name:            "related error, no resp,  counter increase",
			checkers:        []deprecatedCertificateAttributeChecker{&testPositiveChecker{counterRaiser{testCounter, "", ""}}},
			err:             fmt.Errorf("error"),
			counterIncrease: true,
		},
	}

	var lastCounterVal int
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &x509DeprecatedCertificateMetricsRTWrapper{
				rt:       newTestRoundTripper(tt.resp, tt.err),
				checkers: tt.checkers,
			}
			got, err := w.RoundTrip(&http.Request{})
			if err != tt.err {
				t.Errorf("x509DeprecatedCertificateMetricsRTWrapper.RoundTrip() should not mutate the error. Got %v, want %v", err, tt.err)
				return
			}
			if !reflect.DeepEqual(got, tt.resp) {
				t.Errorf("x509DeprecatedCertificateMetricsRTWrapper.RoundTrip() = should not mutate the response. Got %v, want %v", got, tt.resp)
			}

			errorCounterVal := getSingleCounterValueFromRegistry(t, registry, "testCounter")
			if errorCounterVal == -1 {
				t.Fatalf("failed to get the error counter from the registry")
			}

			if tt.counterIncrease && errorCounterVal != lastCounterVal+1 {
				t.Errorf("expected the testCounter metrics to increase by 1 from %d, but it is %d", lastCounterVal, errorCounterVal)
			}

			if !tt.counterIncrease && errorCounterVal != lastCounterVal {
				t.Errorf("expected the testCounter metrics to stay the same (%d), but it is %d", lastCounterVal, errorCounterVal)
			}

			lastCounterVal = errorCounterVal
		})
	}
}

type testRoundTripper func(req *http.Request) (*http.Response, error)

func (rt testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return rt(req)
}

func newTestRoundTripper(resp *http.Response, err error) testRoundTripper {
	return func(_ *http.Request) (*http.Response, error) {
		return resp, err
	}
}

type testPositiveChecker struct {
	counterRaiser
}

func (c *testPositiveChecker) CheckRoundTripError(_ error) bool {
	return true
}

func (c *testPositiveChecker) CheckPeerCertificates(_ []*x509.Certificate) bool {
	return true
}

type testNegativeChecker struct {
	counterRaiser
}

func (c *testNegativeChecker) CheckRoundTripError(_ error) bool {
	return false
}

func (c *testNegativeChecker) CheckPeerCertificates(_ []*x509.Certificate) bool {
	return false
}

func httpResponseWithCert() *http.Response {
	return &http.Response{
		TLS: &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{
				{Issuer: pkix.Name{CommonName: "a name"}},
			},
		},
	}
}

func httpResponseNoCert() *http.Response {
	return &http.Response{}
}

func testServer(t *testing.T, serverCert []byte) (*httptest.Server, *url.URL) {
	rootCAs := x509.NewCertPool()
	rootCAs.AppendCertsFromPEM(caCert)

	cert, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatalf("failed to init x509 cert/key pair: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      rootCAs,
	}

	tlsServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))

	tlsServer.TLS = tlsConfig
	tlsServer.StartTLS()

	serverURL, err := url.Parse(tlsServer.URL)
	if err != nil {
		tlsServer.Close()
		t.Fatalf("failed to parse the testserver URL: %v", err)
	}
	serverURL.Host = net.JoinHostPort("localhost", serverURL.Port())

	return tlsServer, serverURL
}

func getSingleCounterValueFromRegistry(t *testing.T, r metrics.Gatherer, name string) int {
	mfs, err := r.Gather()
	if err != nil {
		t.Logf("failed to gather local registry metrics: %v", err)
		return -1
	}

	for _, mf := range mfs {
		if mf.Name != nil && *mf.Name == name {
			mfMetric := mf.GetMetric()
			for _, m := range mfMetric {
				if m.GetCounter() != nil {
					return int(m.GetCounter().GetValue())
				}
			}
		}
	}

	return -1
}

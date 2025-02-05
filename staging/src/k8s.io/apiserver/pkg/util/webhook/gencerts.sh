#!/usr/bin/env bash

# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e

# gencerts.sh generates the certificates for the webhook tests.
#
# It is not expected to be run often (there is no go generate rule), and mainly
# exists for documentation purposes.

CN_BASE="webhook_tests"

cat > intermediate_ca.conf << EOF
[ v3_ca ]
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid:always,issuer
basicConstraints = critical,CA:true
keyUsage = cRLSign, keyCertSign
EOF

cat > server.conf << EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth, serverAuth
subjectAltName = @alt_names
[alt_names]
IP.1 = 127.0.0.1
DNS.1 = localhost
EOF

cat > server_no_san.conf << EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth, serverAuth
EOF

cat > client.conf << EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth, serverAuth
subjectAltName = @alt_names
[alt_names]
IP.1 = 127.0.0.1
EOF

# Create a certificate authority
openssl genrsa -out caKey.pem 2048
openssl req -x509 -new -nodes -key caKey.pem -days 100000 -out caCert.pem -subj "/CN=${CN_BASE}_ca"

# Create a second certificate authority
openssl genrsa -out badCAKey.pem 2048
openssl req -x509 -new -nodes -key badCAKey.pem -days 100000 -out badCACert.pem -subj "/CN=${CN_BASE}_ca"

# Create an intermediate certificate authority
openssl genrsa -out caKeyInter.pem 2048
openssl req -new -nodes -key caKeyInter.pem -days 100000 -out caCertInter.csr -subj "/CN=${CN_BASE}_intermediate_ca"
openssl x509 -req -in caCertInter.csr -CA caCert.pem -CAkey caKey.pem -CAcreateserial -out caCertInter.pem -days 100000 -extensions v3_ca -extfile intermediate_ca.conf

# Create an intermediate certificate authority with sha1 signature
openssl req -new -nodes -key caKeyInter.pem -days 100000 -out caCertInterSHA1.csr -subj "/CN=${CN_BASE}_intermediate_ca"
openssl x509 -sha1 -req -in caCertInterSHA1.csr -CA caCert.pem -CAkey caKey.pem -CAcreateserial -out caCertInterSHA1.pem -days 100000 -extensions v3_ca -extfile intermediate_ca.conf

# Create a server certificate
openssl genrsa -out serverKey.pem 2048
openssl req -new -key serverKey.pem -out server.csr -subj "/CN=${CN_BASE}_server" -config server.conf
openssl x509 -req -in server.csr -CA caCert.pem -CAkey caKey.pem -CAcreateserial -out serverCert.pem -days 100000 -extensions v3_req -extfile server.conf

# Create a server certificate w/o SAN
openssl req -new -key serverKey.pem -out serverNoSAN.csr -subj "/CN=localhost" -config server_no_san.conf
openssl x509 -req -in serverNoSAN.csr -CA caCert.pem -CAkey caKey.pem -CAcreateserial -out serverCertNoSAN.pem -days 100000 -extensions v3_req -extfile server_no_san.conf

# Create a server certificate with SHA1 signature signed by OK intermediate CA
openssl req -new -key serverKey.pem -out serverSHA1.csr -subj "/CN=localhost" -config server.conf
openssl x509 -sha1 -req -in serverSHA1.csr -CA caCertInter.pem -CAkey caKeyInter.pem -CAcreateserial -out sha1ServerCertInter.pem -days 100000 -extensions v3_req -extfile server.conf

# Create a server certificate signed by SHA1-signed intermediate CA
openssl req -new -key serverKey.pem -out serverInterSHA1.csr -subj "/CN=localhost" -config server.conf
openssl x509 -req -in serverInterSHA1.csr -CA caCertInterSHA1.pem -CAkey caKeyInter.pem -CAcreateserial -out serverCertInterSHA1.pem -days 100000 -extensions v3_req -extfile server.conf

# Create a client certificate
openssl genrsa -out clientKey.pem 2048
openssl req -new -key clientKey.pem -out client.csr -subj "/CN=${CN_BASE}_client" -config client.conf
openssl x509 -req -in client.csr -CA caCert.pem -CAkey caKey.pem -CAcreateserial -out clientCert.pem -days 100000 -extensions v3_req -extfile client.conf

outfile=certs_test.go

cat > $outfile << EOF
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

// This file was generated using openssl by the gencerts.sh script
// and holds raw certificates for the webhook tests.

package webhook
EOF

for file in caKey caCert badCAKey badCACert caCertInter caCertInterSHA1 serverKey serverCert serverCertNoSAN clientKey clientCert sha1ServerCertInter serverCertInterSHA1; do
	data=$(cat ${file}.pem)
	echo "" >> $outfile
	echo "var $file = []byte(\`$data\`)" >> $outfile
done

# Clean up after we're done.
rm ./*.pem
rm ./*.csr
rm ./*.srl
rm ./*.conf

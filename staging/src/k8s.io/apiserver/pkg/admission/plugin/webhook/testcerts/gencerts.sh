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

# gencerts.sh generates the certificates for the generic webhook admission plugin tests.
#
# It is not expected to be run often (there is no go generate rule), and mainly
# exists for documentation purposes.

CN_BASE="generic_webhook_admission_plugin_tests"

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
DNS.1 = webhook-test.default.svc
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
DNS.1 = webhook-test.default.svc
EOF

# Create a certificate authority
openssl genrsa -out CAKey.pem 2048
openssl req -x509 -new -nodes -key CAKey.pem -days 100000 -out CACert.pem -subj "/CN=${CN_BASE}_ca"

# Create a second certificate authority
openssl genrsa -out BadCAKey.pem 2048
openssl req -x509 -new -nodes -key BadCAKey.pem -days 100000 -out BadCACert.pem -subj "/CN=${CN_BASE}_ca"

# Create a server certificate
openssl genrsa -out ServerKey.pem 2048
openssl req -new -key ServerKey.pem -out server.csr -subj "/CN=webhook-test.default.svc" -config server.conf
openssl x509 -req -in server.csr -CA CACert.pem -CAkey CAKey.pem -CAcreateserial -out ServerCert.pem -days 100000 -extensions v3_req -extfile server.conf

# Create a client certificate
openssl genrsa -out ClientKey.pem 2048
openssl req -new -key ClientKey.pem -out client.csr -subj "/CN=${CN_BASE}_client" -config client.conf
openssl x509 -req -in client.csr -CA CACert.pem -CAkey CAKey.pem -CAcreateserial -out ClientCert.pem -days 100000 -extensions v3_req -extfile client.conf

outfile=certs.go

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

package testcerts
EOF

for file in CAKey CACert BadCAKey BadCACert ServerKey ServerCert ClientKey ClientCert; do
	data=$(cat ${file}.pem)
	echo "" >> $outfile
	echo "var $file = []byte(\`$data\`)" >> $outfile
done

# Clean up after we're done.
rm ./*.pem
rm ./*.csr
rm ./*.srl
rm ./*.conf

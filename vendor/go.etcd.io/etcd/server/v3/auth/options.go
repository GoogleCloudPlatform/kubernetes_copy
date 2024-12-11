// Copyright 2018 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"fmt"
	"os"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
)

const (
	optSignMethod = "sign-method"
	optPublicKey  = "pub-key"
	optPrivateKey = "priv-key"
	optTTL        = "ttl"
)

var knownOptions = map[string]bool{
	optSignMethod: true,
	optPublicKey:  true,
	optPrivateKey: true,
	optTTL:        true,
}

var (
	// DefaultTTL will be used when a 'ttl' is not specified
	DefaultTTL = 5 * time.Minute
)

type jwtOptions struct {
	SignMethod jwt.SigningMethod
	PublicKey  []byte
	PrivateKey []byte
	TTL        time.Duration
}

// ParseWithDefaults will load options from the specified map or set defaults where appropriate
func (opts *jwtOptions) ParseWithDefaults(optMap map[string]string) error {
	if opts.TTL == 0 && optMap[optTTL] == "" {
		opts.TTL = DefaultTTL
	}

	return opts.Parse(optMap)
}

// Parse will load options from the specified map
func (opts *jwtOptions) Parse(optMap map[string]string) error {
	var err error
	if ttl := optMap[optTTL]; ttl != "" {
		opts.TTL, err = time.ParseDuration(ttl)
		if err != nil {
			return err
		}
	}

	if file := optMap[optPublicKey]; file != "" {
		opts.PublicKey, err = os.ReadFile(file)
		if err != nil {
			return err
		}
	}

	if file := optMap[optPrivateKey]; file != "" {
		opts.PrivateKey, err = os.ReadFile(file)
		if err != nil {
			return err
		}
	}

	// signing method is a required field
	method := optMap[optSignMethod]
	opts.SignMethod = jwt.GetSigningMethod(method)
	if opts.SignMethod == nil {
		return ErrInvalidAuthMethod
	}

	return nil
}

// Key will parse and return the appropriately typed key for the selected signature method
func (opts *jwtOptions) Key() (any, error) {
	switch opts.SignMethod.(type) {
	case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
		return opts.rsaKey()
	case *jwt.SigningMethodECDSA:
		return opts.ecKey()
	case *jwt.SigningMethodEd25519:
		return opts.edKey()
	case *jwt.SigningMethodHMAC:
		return opts.hmacKey()
	default:
		return nil, fmt.Errorf("unsupported signing method: %T", opts.SignMethod)
	}
}

func (opts *jwtOptions) hmacKey() (any, error) {
	if len(opts.PrivateKey) == 0 {
		return nil, ErrMissingKey
	}
	return opts.PrivateKey, nil
}

func (opts *jwtOptions) rsaKey() (any, error) {
	var (
		priv *rsa.PrivateKey
		pub  *rsa.PublicKey
		err  error
	)

	if len(opts.PrivateKey) > 0 {
		priv, err = jwt.ParseRSAPrivateKeyFromPEM(opts.PrivateKey)
		if err != nil {
			return nil, err
		}
	}

	if len(opts.PublicKey) > 0 {
		pub, err = jwt.ParseRSAPublicKeyFromPEM(opts.PublicKey)
		if err != nil {
			return nil, err
		}
	}

	if priv == nil {
		if pub == nil {
			// Neither key given
			return nil, ErrMissingKey
		}
		// Public key only, can verify tokens
		return pub, nil
	}

	// both keys provided, make sure they match
	if pub != nil && !pub.Equal(priv.Public()) {
		return nil, ErrKeyMismatch
	}

	return priv, nil
}

func (opts *jwtOptions) ecKey() (any, error) {
	var (
		priv *ecdsa.PrivateKey
		pub  *ecdsa.PublicKey
		err  error
	)

	if len(opts.PrivateKey) > 0 {
		priv, err = jwt.ParseECPrivateKeyFromPEM(opts.PrivateKey)
		if err != nil {
			return nil, err
		}
	}

	if len(opts.PublicKey) > 0 {
		pub, err = jwt.ParseECPublicKeyFromPEM(opts.PublicKey)
		if err != nil {
			return nil, err
		}
	}

	if priv == nil {
		if pub == nil {
			// Neither key given
			return nil, ErrMissingKey
		}
		// Public key only, can verify tokens
		return pub, nil
	}

	// both keys provided, make sure they match
	if pub != nil && !pub.Equal(priv.Public()) {
		return nil, ErrKeyMismatch
	}

	return priv, nil
}

func (opts *jwtOptions) edKey() (any, error) {
	var (
		priv ed25519.PrivateKey
		pub  ed25519.PublicKey
		err  error
	)

	if len(opts.PrivateKey) > 0 {
		var privKey crypto.PrivateKey
		privKey, err = jwt.ParseEdPrivateKeyFromPEM(opts.PrivateKey)
		if err != nil {
			return nil, err
		}
		priv = privKey.(ed25519.PrivateKey)
	}

	if len(opts.PublicKey) > 0 {
		var pubKey crypto.PublicKey
		pubKey, err = jwt.ParseEdPublicKeyFromPEM(opts.PublicKey)
		if err != nil {
			return nil, err
		}
		pub = pubKey.(ed25519.PublicKey)
	}

	if priv == nil {
		if pub == nil {
			// Neither key given
			return nil, ErrMissingKey
		}
		// Public key only, can verify tokens
		return pub, nil
	}

	// both keys provided, make sure they match
	if pub != nil && !pub.Equal(priv.Public()) {
		return nil, ErrKeyMismatch
	}

	return priv, nil
}

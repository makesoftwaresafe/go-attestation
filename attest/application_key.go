// Copyright 2021 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package attest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"fmt"
	"io"
)

type key interface {
	close(tpmBase) error
	marshal() ([]byte, error)
	certificationParameters() CertificationParameters
	sign(tpmBase, []byte, crypto.PublicKey, crypto.SignerOpts) ([]byte, error)
	decrypt(tpmBase, []byte) ([]byte, error)
	blobs() ([]byte, []byte, error)
}

// Key represents a key which can be used for signing and decrypting
// outside-TPM objects.
type Key struct {
	key key
	pub crypto.PublicKey
	tpm tpmBase
}

// signer implements crypto.Signer returned by Key.Private().
type signer struct {
	key key
	pub crypto.PublicKey
	tpm tpmBase
}

// Sign signs digest with the TPM-stored private signing key.
func (s *signer) Sign(r io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	return s.key.sign(s.tpm, digest, s.pub, opts)
}

// Public returns the public key corresponding to the private signing key.
func (s *signer) Public() crypto.PublicKey {
	return s.pub
}

// Algorithm indicates an asymmetric algorithm to be used.
type Algorithm string

// Algorithm types supported.
const (
	ECDSA Algorithm = "ECDSA"
	RSA   Algorithm = "RSA"

	// Windows specific ECDSA CNG algorithm identifiers.
	// NOTE: Using ECDSA will default to P256.
	// Ref: https://learn.microsoft.com/en-us/windows/win32/SecCNG/cng-algorithm-identifiers
	P256 Algorithm = "ECDSA_P256"
	P384 Algorithm = "ECDSA_P384"
	P521 Algorithm = "ECDSA_P521"
)

// KeyConfig encapsulates parameters for minting keys.
type KeyConfig struct {
	// Algorithm to be used, either RSA or ECDSA.
	Algorithm Algorithm
	// Size is used to specify the bit size of the key or elliptic curve. For
	// example, '256' is used to specify curve P-256.
	Size int
	// Parent describes the Storage Root Key that will be used as a parent.
	// If nil, the default SRK (i.e. RSA with handle 0x81000001) is assumed.
	// Supported only by TPM 2.0 on Linux.
	Parent *ParentKeyConfig
	// QualifyingData is an optional data that will be included into
	// a TPM-generated signature of the minted key.
	// It may contain any data chosen by the caller.
	QualifyingData []byte
}

// defaultConfig is used when no other configuration is specified.
var defaultConfig = &KeyConfig{
	Algorithm: ECDSA,
	Size:      256,
}

// Size returns the bit size associated with an algorithm.
func (a Algorithm) Size() int {
	switch a {
	case RSA:
		return 2048
	case ECDSA:
		return 256
	case P256:
		return 256
	case P384:
		return 384
	case P521:
		return 521
	default:
		return 0
	}
}

// Public returns the public key corresponding to the private key.
func (k *Key) Public() crypto.PublicKey {
	return k.pub
}

// Private returns an object allowing to use the TPM-backed private key.
// For now it implements only crypto.Signer.
func (k *Key) Private(pub crypto.PublicKey) (crypto.PrivateKey, error) {
	switch pub.(type) {
	case *rsa.PublicKey:
		if _, ok := k.pub.(*rsa.PublicKey); !ok {
			return nil, fmt.Errorf("incompatible public key types: %T != %T", pub, k.pub)
		}
	case *ecdsa.PublicKey:
		if _, ok := k.pub.(*ecdsa.PublicKey); !ok {
			return nil, fmt.Errorf("incompatible public key types: %T != %T", pub, k.pub)
		}
	default:
		return nil, fmt.Errorf("unsupported public key type: %T", pub)
	}
	return &signer{k.key, k.pub, k.tpm}, nil
}

// Close unloads the key from the system.
func (k *Key) Close() error {
	return k.key.close(k.tpm)
}

// Marshal encodes the key in a format that can be loaded with tpm.LoadKey().
// This method exists to allow consumers to store the key persistently and load
// it as a later time. Users SHOULD NOT attempt to interpret or extract values
// from this blob.
func (k *Key) Marshal() ([]byte, error) {
	return k.key.marshal()
}

// CertificationParameters returns information about the key required to
// verify key certification.
func (k *Key) CertificationParameters() CertificationParameters {
	return k.key.certificationParameters()
}

// Blobs returns public and private blobs to be used by tpm2.Load().
func (k *Key) Blobs() (pub, priv []byte, err error) {
	return k.key.blobs()
}

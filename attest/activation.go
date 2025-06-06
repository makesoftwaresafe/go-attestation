package attest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"

	"github.com/google/go-tpm/legacy/tpm2"
	tpm1 "github.com/google/go-tpm/tpm"

	// TODO(jsonp): Move activation generation code to internal package.
	"github.com/google/go-tpm/legacy/tpm2/credactivation"
	"github.com/google/go-tspi/verification"
)

const (
	// minRSABits is the minimum accepted bit size of an RSA key.
	minRSABits = 2048
	// minECCBits is the minimum accepted bit size of an ECC key.
	minECCBits = 256
	// activationSecretLen is the size in bytes of the generated secret
	// which is generated for credential activation.
	activationSecretLen = 32
	// symBlockSize is the block size used for symmetric ciphers used
	// when generating the credential activation challenge.
	symBlockSize = 16
	// tpm20GeneratedMagic is a magic tag when can only be present on a
	// TPM structure if the structure was generated wholly by the TPM.
	tpm20GeneratedMagic = 0xff544347
)

// ActivationParameters encapsulates the inputs for activating an AK.
type ActivationParameters struct {
	// TPMVersion holds the version of the TPM, either 1.2 or 2.0.
	TPMVersion TPMVersion

	// EK, the endorsement key, describes an asymmetric key whose
	// private key is permanently bound to the TPM.
	//
	// Activation will verify that the provided EK is held on the same
	// TPM as the AK. However, it is the caller's responsibility to
	// ensure the EK they provide corresponds to the the device which
	// they are trying to associate the AK with.
	EK crypto.PublicKey

	// AK, the Attestation Key, describes the properties of
	// an asymmetric key (managed by the TPM) which signs attestation
	// structures.
	// The values from this structure can be obtained by calling
	// Parameters() on an attest.AK.
	AK AttestationParameters

	// Rand is a source of randomness to generate a seed and secret for the
	// challenge.
	//
	// If nil, this defaults to crypto.Rand.
	Rand io.Reader
}

// CheckAKParameters examines properties of an AK and a creation
// attestation, to determine if it is suitable for use as an attestation key.
func (p *ActivationParameters) CheckAKParameters() error {
	switch p.TPMVersion {
	case TPMVersion12:
		return p.checkTPM12AKParameters()

	case TPMVersion20:
		return p.checkTPM20AKParameters()

	default:
		return fmt.Errorf("TPM version %d not supported", p.TPMVersion)
	}
}

func (p *ActivationParameters) checkTPM12AKParameters() error {
	// TODO(jsonp): Implement helper to parse public blobs, ie:
	//   func ParsePublic(publicBlob []byte) (crypto.Public, error)

	pub, err := tpm1.UnmarshalPubRSAPublicKey(p.AK.Public)
	if err != nil {
		return fmt.Errorf("unmarshalling public key: %v", err)
	}
	if bits := pub.Size() * 8; bits < minRSABits {
		return fmt.Errorf("attestation key too small: must be at least %d bits but was %d bits", minRSABits, bits)
	}
	return nil
}

func (p *ActivationParameters) checkTPM20AKParameters() error {
	if len(p.AK.CreateSignature) < 8 {
		return fmt.Errorf("signature is too short to be valid: only %d bytes", len(p.AK.CreateSignature))
	}

	pub, err := tpm2.DecodePublic(p.AK.Public)
	if err != nil {
		return fmt.Errorf("DecodePublic() failed: %v", err)
	}
	_, err = tpm2.DecodeCreationData(p.AK.CreateData)
	if err != nil {
		return fmt.Errorf("DecodeCreationData() failed: %v", err)
	}
	att, err := tpm2.DecodeAttestationData(p.AK.CreateAttestation)
	if err != nil {
		return fmt.Errorf("DecodeAttestationData() failed: %v", err)
	}
	if att.Type != tpm2.TagAttestCreation {
		return fmt.Errorf("attestation does not apply to creation data, got tag %x", att.Type)
	}

	// TODO: Support ECC AKs.
	switch pub.Type {
	case tpm2.AlgRSA:
		if pub.RSAParameters.KeyBits < minRSABits {
			return fmt.Errorf("attestation key too small: must be at least %d bits but was %d bits", minRSABits, pub.RSAParameters.KeyBits)
		}
	case tpm2.AlgECC:
		if len(pub.ECCParameters.Point.XRaw)*8 < minECCBits {
			return fmt.Errorf("attestation key too small: must be at least %d bits but was %d bits", minECCBits, len(pub.ECCParameters.Point.XRaw)*8)
		} else if len(pub.ECCParameters.Point.YRaw)*8 < minECCBits {
			return fmt.Errorf("attestation key too small: must be at least %d bits but was %d bits", minECCBits, len(pub.ECCParameters.Point.YRaw)*8)
		}
	default:
		return fmt.Errorf("public key of alg 0x%x not supported", pub.Type)
	}

	// Compute & verify that the creation data matches the digest in the
	// attestation structure.
	nameHash, err := pub.NameAlg.Hash()
	if err != nil {
		return fmt.Errorf("HashConstructor() failed: %v", err)
	}
	h := nameHash.New()
	h.Write(p.AK.CreateData)
	if !bytes.Equal(att.AttestedCreationInfo.OpaqueDigest, h.Sum(nil)) {
		return errors.New("attestation refers to different public key")
	}

	// Make sure the AK has sane key parameters (Attestation can be faked if an AK
	// can be used for arbitrary signatures).
	// We verify the following:
	// - Key is TPM backed.
	// - Key is TPM generated.
	// - Key is a restricted key (means it cannot do arbitrary signing/decrypt ops).
	// - Key cannot be duplicated.
	// - Key was generated by a call to TPM_Create*.
	if att.Magic != tpm20GeneratedMagic {
		return errors.New("creation attestation was not produced by a TPM")
	}
	if (pub.Attributes & tpm2.FlagFixedTPM) == 0 {
		return errors.New("AK is exportable")
	}
	if ((pub.Attributes & tpm2.FlagRestricted) == 0) || ((pub.Attributes & tpm2.FlagFixedParent) == 0) || ((pub.Attributes & tpm2.FlagSensitiveDataOrigin) == 0) {
		return errors.New("provided key is not limited to attestation")
	}

	// Verify the attested creation name matches what is computed from
	// the public key.
	match, err := att.AttestedCreationInfo.Name.MatchesPublic(pub)
	if err != nil {
		return err
	}
	if !match {
		return errors.New("creation attestation refers to a different key")
	}

	// Check the signature over the attestation data verifies correctly.
	switch pub.Type {
	case tpm2.AlgRSA:
		return verifyRSASignature(pub, p)
	case tpm2.AlgECC:
		return verifyECDSASignature(pub, p)
	default:
		return fmt.Errorf("public key of alg 0x%x not supported", pub.Type)
	}
}

func verifyRSASignature(pub tpm2.Public, p *ActivationParameters) error {
	pk := rsa.PublicKey{E: int(pub.RSAParameters.Exponent()), N: pub.RSAParameters.Modulus()}
	signHash, err := pub.RSAParameters.Sign.Hash.Hash()
	if err != nil {
		return err
	}
	hsh := signHash.New()
	hsh.Write(p.AK.CreateAttestation)
	verifyHash, err := pub.RSAParameters.Sign.Hash.Hash()
	if err != nil {
		return err
	}

	if len(p.AK.CreateSignature) < 8 {
		return fmt.Errorf("signature invalid: length of %d is shorter than 8", len(p.AK.CreateSignature))
	}

	sig, err := tpm2.DecodeSignature(bytes.NewBuffer(p.AK.CreateSignature))
	if err != nil {
		return fmt.Errorf("DecodeSignature() failed: %v", err)
	}

	if err := rsa.VerifyPKCS1v15(&pk, verifyHash, hsh.Sum(nil), sig.RSA.Signature); err != nil {
		return fmt.Errorf("could not verify attestation: %v", err)
	}

	return nil
}

func verifyECDSASignature(pub tpm2.Public, p *ActivationParameters) error {
	key, err := pub.Key()
	if err != nil {
		return nil
	}
	pk, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("expected *ecdsa.PublicKey, got %T", key)
	}
	signHash, err := pub.ECCParameters.Sign.Hash.Hash()
	if err != nil {
		return err
	}
	hsh := signHash.New()
	_, err = hsh.Write(p.AK.CreateAttestation)
	if err != nil {
		return err
	}

	if len(p.AK.CreateSignature) < 8 {
		return fmt.Errorf("signature invalid: length of %d is shorter than 8", len(p.AK.CreateSignature))
	}

	sig, err := tpm2.DecodeSignature(bytes.NewBuffer(p.AK.CreateSignature))
	if err != nil {
		return fmt.Errorf("DecodeSignature() failed: %v", err)
	}

	if !ecdsa.Verify(pk, hsh.Sum(nil), sig.ECC.R, sig.ECC.S) {
		return fmt.Errorf("unable to verify attestation for ecdsa credential")
	}
	return nil
}

// Generate returns a credential activation challenge, which can be provided
// to the TPM to verify the AK parameters given are authentic & the AK
// is present on the same TPM as the EK.
//
// It will call CheckAKParameters() at the beginning and return the error if
// there is any.
//
// The caller is expected to verify the secret returned from the TPM as
// as result of calling ActivateCredential() matches the secret returned here.
// The caller should use subtle.ConstantTimeCompare to avoid potential
// timing attack vectors.
func (p *ActivationParameters) Generate() (secret []byte, ec *EncryptedCredential, err error) {
	if err := p.CheckAKParameters(); err != nil {
		return nil, nil, err
	}

	if p.EK == nil {
		return nil, nil, errors.New("no EK provided")
	}

	rnd, secret := p.Rand, make([]byte, activationSecretLen)
	if rnd == nil {
		rnd = rand.Reader
	}
	if _, err = io.ReadFull(rnd, secret); err != nil {
		return nil, nil, fmt.Errorf("error generating activation secret: %v", err)
	}

	switch p.TPMVersion {
	case TPMVersion12:
		ec, err = p.generateChallengeTPM12(rnd, secret)
	case TPMVersion20:
		ec, err = p.generateChallengeTPM20(secret)
	default:
		return nil, nil, fmt.Errorf("unrecognised TPM version: %v", p.TPMVersion)
	}

	if err != nil {
		return nil, nil, err
	}
	return secret, ec, nil
}

func (p *ActivationParameters) generateChallengeTPM20(secret []byte) (*EncryptedCredential, error) {
	att, err := tpm2.DecodeAttestationData(p.AK.CreateAttestation)
	if err != nil {
		return nil, fmt.Errorf("DecodeAttestationData() failed: %v", err)
	}
	if att.AttestedCreationInfo == nil {
		return nil, fmt.Errorf("attestation was not for a creation event")
	}
	if att.AttestedCreationInfo.Name.Digest == nil {
		return nil, fmt.Errorf("attestation creation info name has no digest")
	}
	cred, encSecret, err := credactivation.Generate(att.AttestedCreationInfo.Name.Digest, p.EK, symBlockSize, secret)
	if err != nil {
		return nil, fmt.Errorf("credactivation.Generate() failed: %v", err)
	}

	return &EncryptedCredential{
		Credential: cred,
		Secret:     encSecret,
	}, nil
}

func (p *ActivationParameters) generateChallengeTPM12(rand io.Reader, secret []byte) (*EncryptedCredential, error) {
	pk, ok := p.EK.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("got EK of type %T, want an RSA key", p.EK)
	}

	var (
		cred, encSecret []byte
		err             error
	)
	if p.AK.UseTCSDActivationFormat {
		cred, encSecret, err = verification.GenerateChallengeEx(pk, p.AK.Public, secret)
	} else {
		cred, encSecret, err = generateChallenge12(rand, pk, p.AK.Public, secret)
	}

	if err != nil {
		return nil, fmt.Errorf("challenge generation failed: %v", err)
	}
	return &EncryptedCredential{
		Credential: cred,
		Secret:     encSecret,
	}, nil
}

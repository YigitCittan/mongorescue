package mongotls

import (
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
)

// ErrKDFTooExpensive is returned for an encrypted key whose key derivation
// parameters exceed the limits below: opening it would cost a denial of service
// (CPU or memory) on every save and connection.
var ErrKDFTooExpensive = errors.New("mongotls: the client key's encryption parameters are too expensive; re-encrypt it with 'openssl pkcs8 -topk8 -v2 aes256' (PBKDF2, at most 1,000,000 iterations) or scrypt with N at most 2^20")

// ErrUnsupportedKeyEncryption is returned for an encrypted key that does not use
// PBES2 with PBKDF2 or scrypt.
var ErrUnsupportedKeyEncryption = errors.New("mongotls: the client key must be encrypted with PBES2 (PBKDF2 or scrypt); re-encrypt it with 'openssl pkcs8 -topk8 -v2 aes256'")

// Limits of the key derivation of an encrypted PKCS#8 key.
const (
	// MaxPBKDF2Iterations bounds the PBKDF2 iteration count.
	MaxPBKDF2Iterations = 1_000_000
	// MaxScryptN bounds the scrypt cost parameter N.
	MaxScryptN = 1 << 20
	// MaxScryptR and MaxScryptP bound the scrypt block size and parallelization,
	// and MaxScryptRP their product.
	MaxScryptR  = 32
	MaxScryptP  = 16
	MaxScryptRP = 64
	// MaxScryptMemory bounds the memory scrypt needs, 128·N·r bytes.
	MaxScryptMemory = 256 << 20
)

// Object identifiers of PKCS#5 v2 and scrypt (RFC 8018, RFC 7914).
var (
	oidPBES2  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidScrypt = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11591, 4, 11}
)

// encryptedPrivateKeyInfo is the PKCS#8 EncryptedPrivateKeyInfo.
type encryptedPrivateKeyInfo struct {
	Algorithm     pkix.AlgorithmIdentifier
	EncryptedData []byte
}

// pbes2Params are the PBES2 parameters.
type pbes2Params struct {
	KeyDerivationFunc pkix.AlgorithmIdentifier
	EncryptionScheme  pkix.AlgorithmIdentifier
}

// pbkdf2Params are the PBKDF2 parameters.
type pbkdf2Params struct {
	Salt           []byte
	IterationCount int
	KeyLength      int                      `asn1:"optional"`
	PRF            pkix.AlgorithmIdentifier `asn1:"optional"`
}

// scryptParams are the scrypt parameters.
type scryptParams struct {
	Salt                     []byte
	CostParameter            int
	BlockSize                int
	ParallelizationParameter int
	KeyLength                int `asn1:"optional"`
}

// checkKDF checks the key derivation parameters of the DER EncryptedPrivateKeyInfo
// der before anything is derived: PBES2 with PBKDF2 (at most MaxPBKDF2Iterations)
// or scrypt (within the scrypt limits).
func checkKDF(der []byte) error {
	var info encryptedPrivateKeyInfo
	if rest, err := asn1.Unmarshal(der, &info); err != nil || len(rest) != 0 {
		return ErrInvalidClientCert
	}
	if !info.Algorithm.Algorithm.Equal(oidPBES2) {
		return ErrUnsupportedKeyEncryption
	}
	var params pbes2Params
	if rest, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &params); err != nil || len(rest) != 0 {
		return ErrInvalidClientCert
	}
	kdf := params.KeyDerivationFunc
	switch {
	case kdf.Algorithm.Equal(oidPBKDF2):
		var p pbkdf2Params
		if rest, err := asn1.Unmarshal(kdf.Parameters.FullBytes, &p); err != nil || len(rest) != 0 {
			return ErrInvalidClientCert
		}
		if p.IterationCount < 1 || p.IterationCount > MaxPBKDF2Iterations {
			return fmt.Errorf("%w (PBKDF2 iterations %d)", ErrKDFTooExpensive, p.IterationCount)
		}
	case kdf.Algorithm.Equal(oidScrypt):
		var p scryptParams
		if rest, err := asn1.Unmarshal(kdf.Parameters.FullBytes, &p); err != nil || len(rest) != 0 {
			return ErrInvalidClientCert
		}
		n, r, par := p.CostParameter, p.BlockSize, p.ParallelizationParameter
		switch {
		case n < 2 || n&(n-1) != 0 || r < 1 || par < 1:
			return ErrInvalidClientCert
		case n > MaxScryptN || r > MaxScryptR || par > MaxScryptP || r*par > MaxScryptRP || int64(128)*int64(n)*int64(r) > MaxScryptMemory:
			return fmt.Errorf("%w (scrypt N=%d r=%d p=%d)", ErrKDFTooExpensive, n, r, par)
		}
	default:
		return ErrUnsupportedKeyEncryption
	}
	return nil
}

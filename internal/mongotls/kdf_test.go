package mongotls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/youmark/pkcs8"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

// encryptedDER returns a key encrypted with opts, as EncryptedPrivateKeyInfo DER.
func encryptedDER(t *testing.T, opts *pkcs8.Opts) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := pkcs8.MarshalPrivateKey(key, []byte("pw"), opts)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// withKDFParams returns der with its key derivation parameters replaced by params
// (the encrypted data no longer opens; checkKDF must refuse it first).
func withKDFParams(t *testing.T, der []byte, params any) []byte {
	t.Helper()
	var info encryptedPrivateKeyInfo
	if _, err := asn1.Unmarshal(der, &info); err != nil {
		t.Fatal(err)
	}
	var p2 pbes2Params
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &p2); err != nil {
		t.Fatal(err)
	}
	raw, err := asn1.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	p2.KeyDerivationFunc.Parameters = asn1.RawValue{FullBytes: raw}
	if raw, err = asn1.Marshal(p2); err != nil {
		t.Fatal(err)
	}
	info.Algorithm.Parameters = asn1.RawValue{FullBytes: raw}
	out, err := asn1.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCheckKDF(t *testing.T) {
	pbkdf2 := encryptedDER(t, &pkcs8.Opts{Cipher: pkcs8.AES256CBC,
		KDFOpts: pkcs8.PBKDF2Opts{SaltSize: 16, IterationCount: 10000, HMACHash: crypto.SHA256}})
	scrypt := encryptedDER(t, &pkcs8.Opts{Cipher: pkcs8.AES256CBC,
		KDFOpts: pkcs8.ScryptOpts{SaltSize: 16, CostParameter: 1 << 14, BlockSize: 8, ParallelizationParameter: 1}})
	salt := make([]byte, 16)
	cases := map[string]struct {
		der  []byte
		want error
	}{
		"pbkdf2 default":     {pbkdf2, nil},
		"scrypt default":     {scrypt, nil},
		"pbkdf2 at the cap":  {withKDFParams(t, pbkdf2, pbkdf2Params{Salt: salt, IterationCount: MaxPBKDF2Iterations, PRF: pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}}}), nil},
		"pbkdf2 above":       {withKDFParams(t, pbkdf2, pbkdf2Params{Salt: salt, IterationCount: MaxPBKDF2Iterations + 1}), ErrKDFTooExpensive},
		"pbkdf2 huge":        {withKDFParams(t, pbkdf2, pbkdf2Params{Salt: salt, IterationCount: 1 << 40}), ErrKDFTooExpensive},
		"pbkdf2 zero":        {withKDFParams(t, pbkdf2, pbkdf2Params{Salt: salt, IterationCount: 0}), ErrKDFTooExpensive},
		"scrypt N above":     {withKDFParams(t, scrypt, scryptParams{Salt: salt, CostParameter: MaxScryptN * 2, BlockSize: 1, ParallelizationParameter: 1}), ErrKDFTooExpensive},
		"scrypt memory":      {withKDFParams(t, scrypt, scryptParams{Salt: salt, CostParameter: MaxScryptN, BlockSize: 8, ParallelizationParameter: 1}), ErrKDFTooExpensive},
		"scrypt r·p":         {withKDFParams(t, scrypt, scryptParams{Salt: salt, CostParameter: 1 << 10, BlockSize: 16, ParallelizationParameter: 8}), ErrKDFTooExpensive},
		"scrypt p above":     {withKDFParams(t, scrypt, scryptParams{Salt: salt, CostParameter: 1 << 10, BlockSize: 1, ParallelizationParameter: 17}), ErrKDFTooExpensive},
		"scrypt N not pow2":  {withKDFParams(t, scrypt, scryptParams{Salt: salt, CostParameter: 1000, BlockSize: 8, ParallelizationParameter: 1}), ErrInvalidClientCert},
		"scrypt at the caps": {withKDFParams(t, scrypt, scryptParams{Salt: salt, CostParameter: MaxScryptN, BlockSize: 2, ParallelizationParameter: 1}), nil},
		"garbage":            {[]byte{0x30, 0x03, 0x02, 0x01, 0x01}, ErrInvalidClientCert},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkKDF(tc.der); !errors.Is(err, tc.want) {
				t.Fatalf("checkKDF = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestExpensiveKeyIsRefusedBeforeDerivation saves (Validate) a key whose PBKDF2
// iteration count is above the cap: it is refused without any derivation.
func TestExpensiveKeyIsRefusedBeforeDerivation(t *testing.T) {
	ca, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Client("backup")
	if err != nil {
		t.Fatal(err)
	}
	der := encryptedDER(t, nil)
	der = withKDFParams(t, der, pbkdf2Params{Salt: make([]byte, 16), IterationCount: 50_000_000})
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: der}))
	before := kdfRuns.Load()
	err = Validate(&models.ConnectionTLS{ClientCertPEM: client.CertPEM, ClientKeyPEM: keyPEM, ClientKeyPassword: "pw"})
	if !errors.Is(err, ErrKDFTooExpensive) {
		t.Fatalf("Validate = %v; want ErrKDFTooExpensive", err)
	}
	if kdfRuns.Load() != before {
		t.Fatal("the key was derived before its parameters were checked")
	}
}

// TestEncryptedKeyIsDerivedOnce checks the certificate cache: building several
// TLS configurations from the same encrypted key derives it once; Forget and
// ForgetAll make the next one derive it again.
func TestEncryptedKeyIsDerivedOnce(t *testing.T) {
	ca, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Client("backup")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := client.EncryptedKeyPEM("pw")
	if err != nil {
		t.Fatal(err)
	}
	material := &models.ConnectionTLS{CAPEM: ca.CertPEM, ClientCertPEM: client.CertPEM, ClientKeyPEM: enc, ClientKeyPassword: "pw"}
	Forget(material)
	before := kdfRuns.Load()
	for range 5 {
		if _, err = Config(material); err != nil {
			t.Fatal(err)
		}
	}
	if n := kdfRuns.Load() - before; n != 1 {
		t.Fatalf("5 configurations derived the key %d times; want 1", n)
	}
	// A wrong password is never answered from the cache.
	wrong := *material
	wrong.ClientKeyPassword = "nope"
	if _, err = Config(&wrong); !errors.Is(err, ErrKeyPassword) {
		t.Fatalf("wrong password = %v", err)
	}
	Forget(material)
	if _, err = Config(material); err != nil {
		t.Fatal(err)
	}
	ForgetAll()
	if _, err = Config(material); err != nil {
		t.Fatal(err)
	}
	if n := kdfRuns.Load() - before; n != 4 {
		t.Fatalf("derivations = %d; want 4 (once, wrong password, after Forget, after ForgetAll)", n)
	}
}

func TestCertCacheIsBounded(t *testing.T) {
	c := newCertCache()
	for i := range maxCachedCerts + 10 {
		c.put(c.id("cert", string(rune('a'+i%26))+string(rune(i)), "pw"), tls.Certificate{})
	}
	if len(c.certs) != maxCachedCerts || len(c.order) != maxCachedCerts {
		t.Fatalf("cache holds %d (%d ordered); want %d", len(c.certs), len(c.order), maxCachedCerts)
	}
}

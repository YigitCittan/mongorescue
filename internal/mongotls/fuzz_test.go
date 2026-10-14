package mongotls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/youmark/pkcs8"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

// Limits of the key derivation the fuzz targets actually run. checkKDF allows up to
// MaxPBKDF2Iterations and MaxScryptMemory, which is fine for one save but would slow
// every fuzzing worker to a crawl (and scrypt at 256 MiB per worker can exhaust the
// runner's memory), so inputs above these limits are checked by checkKDF alone.
const (
	fuzzMaxPBKDF2Iterations = 20_000
	fuzzMaxScryptCost       = 1 << 16 // N·r·p
)

// fuzzKeyTypes are the PEM block types FuzzDecryptKey wraps its DER input in.
var fuzzKeyTypes = []string{encryptedPKCS8Type, "PRIVATE KEY", "EC PRIVATE KEY", "RSA PRIVATE KEY", "CERTIFICATE"}

// fuzzSentinels are the errors DecryptKey and Config may return.
var fuzzSentinels = []error{
	ErrInvalidCA, ErrInvalidClientCert, ErrIncompleteClientCert, ErrKeyPassword,
	ErrLegacyEncryptedKey, ErrTooLarge, ErrKDFTooExpensive, ErrUnsupportedKeyEncryption,
}

// fuzzSentinelText holds the messages of fuzzSentinels, so a password that happens to
// be a word of a fixed message is not reported as a leak.
var fuzzSentinelText = func() string {
	var b strings.Builder
	for _, e := range fuzzSentinels {
		b.WriteString(e.Error())
		b.WriteByte('\n')
	}
	return b.String()
}()

// checkFuzzError fails t if err is not one of the sentinels or quotes the password.
func checkFuzzError(t *testing.T, err error, password string) {
	t.Helper()
	known := false
	for _, s := range fuzzSentinels {
		if errors.Is(err, s) {
			known = true
			break
		}
	}
	if !known {
		t.Fatalf("error %q is not a mongotls sentinel", err)
	}
	if len(password) >= 4 && strings.Contains(err.Error(), password) &&
		!strings.Contains(fuzzSentinelText, password) && strings.Trim(password, "0123456789 =()") != "" {
		t.Fatalf("error %q quotes the key password", err)
	}
}

// fuzzKDFCheap reports whether the key derivation of the EncryptedPrivateKeyInfo der
// (already accepted by checkKDF) is cheap enough to run under the fuzzer.
func fuzzKDFCheap(t *testing.T, der []byte) bool {
	t.Helper()
	var info encryptedPrivateKeyInfo
	var params pbes2Params
	if _, err := asn1.Unmarshal(der, &info); err != nil {
		t.Fatalf("checkKDF accepted DER that does not parse: %v", err)
	}
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &params); err != nil {
		t.Fatalf("checkKDF accepted PBES2 parameters that do not parse: %v", err)
	}
	kdf := params.KeyDerivationFunc
	switch {
	case kdf.Algorithm.Equal(oidPBKDF2):
		var p pbkdf2Params
		if _, err := asn1.Unmarshal(kdf.Parameters.FullBytes, &p); err != nil {
			t.Fatalf("checkKDF accepted PBKDF2 parameters that do not parse: %v", err)
		}
		if p.IterationCount < 1 || p.IterationCount > MaxPBKDF2Iterations {
			t.Fatalf("checkKDF accepted %d PBKDF2 iterations", p.IterationCount)
		}
		return p.IterationCount <= fuzzMaxPBKDF2Iterations
	case kdf.Algorithm.Equal(oidScrypt):
		var p scryptParams
		if _, err := asn1.Unmarshal(kdf.Parameters.FullBytes, &p); err != nil {
			t.Fatalf("checkKDF accepted scrypt parameters that do not parse: %v", err)
		}
		n, r, par := int64(p.CostParameter), int64(p.BlockSize), int64(p.ParallelizationParameter)
		if n > MaxScryptN || r > MaxScryptR || par > MaxScryptP || 128*n*r > MaxScryptMemory {
			t.Fatalf("checkKDF accepted scrypt N=%d r=%d p=%d", n, r, par)
		}
		return n*r*par <= fuzzMaxScryptCost
	}
	t.Fatalf("checkKDF accepted key derivation %v", kdf.Algorithm)
	return false
}

// fuzzKeyPEMCheap reports whether DecryptKey(keyPEM, password) derives nothing
// beyond the fuzzing limits: it looks at the block DecryptKey would open.
func fuzzKeyPEMCheap(t *testing.T, keyPEM, password string) bool {
	t.Helper()
	if password == "" || len(keyPEM) > MaxPEMSize {
		return true
	}
	rest := []byte(keyPEM)
	for {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return true
		}
		if !strings.HasSuffix(block.Type, "PRIVATE KEY") {
			continue
		}
		if block.Type != encryptedPKCS8Type || checkKDF(block.Bytes) != nil {
			return true
		}
		return fuzzKDFCheap(t, block.Bytes)
	}
}

// fuzzMaterial holds the seed keys and certificates of the fuzz targets.
type fuzzMaterial struct {
	plainDER, pbkdf2DER, scryptDER []byte
	caPEM, certPEM, keyPEM, encKey string
}

// fuzzSeedMaterial creates the seed material once per process: libFuzzer builds
// (ClusterFuzzLite) run the whole Fuzz function for every input, so key generation
// and derivation must not happen per call.
var fuzzSeedMaterial = sync.OnceValues(func() (*fuzzMaterial, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	m := &fuzzMaterial{}
	if m.plainDER, err = x509.MarshalPKCS8PrivateKey(key); err != nil {
		return nil, err
	}
	if m.pbkdf2DER, err = pkcs8.MarshalPrivateKey(key, []byte("pw-1234"), &pkcs8.Opts{
		Cipher: pkcs8.AES256CBC, KDFOpts: pkcs8.PBKDF2Opts{SaltSize: 16, IterationCount: 2048, HMACHash: crypto.SHA256},
	}); err != nil {
		return nil, err
	}
	if m.scryptDER, err = pkcs8.MarshalPrivateKey(key, []byte("pw-1234"), &pkcs8.Opts{
		Cipher: pkcs8.AES128GCM, KDFOpts: pkcs8.ScryptOpts{SaltSize: 16, CostParameter: 1 << 10, BlockSize: 8, ParallelizationParameter: 1},
	}); err != nil {
		return nil, err
	}
	ca, err := mongotlstest.NewCA()
	if err != nil {
		return nil, err
	}
	client, err := ca.Client("backup")
	if err != nil {
		return nil, err
	}
	if m.encKey, err = client.EncryptedKeyPEM("pw-1234"); err != nil {
		return nil, err
	}
	m.caPEM, m.certPEM, m.keyPEM = ca.CertPEM, client.CertPEM, client.KeyPEM
	return m, nil
})

// seedMaterial returns the seed material or fails f. Under go test f.Fatal ends the
// target and the panic is never reached; in libFuzzer builds (ClusterFuzzLite,
// go-118-fuzz-build) the *testing.F stand-in's Fatal returns, so the panic reports
// the real error instead of a nil dereference of m.
func seedMaterial(f *testing.F) *fuzzMaterial {
	f.Helper()
	m, err := fuzzSeedMaterial()
	if err != nil {
		f.Fatal(err) //nolint:revive // Fatal returns in libFuzzer builds; the panic is for them.
		panic(err)
	}
	return m
}

// FuzzDecryptKey checks DecryptKey on arbitrary DER wrapped in a PEM block of a
// private key type: it never panics, checks the key derivation parameters before
// deriving anything, fails only with its sentinels (never quoting the password), and
// returns an unencrypted PKCS#8 or original key block that parses.
func FuzzDecryptKey(f *testing.F) {
	m := seedMaterial(f)
	plain, pbkdf2DER, scryptDER := m.plainDER, m.pbkdf2DER, m.scryptDER
	f.Add(pbkdf2DER, uint8(0), "pw-1234")
	f.Add(pbkdf2DER, uint8(0), "wrong")
	f.Add(pbkdf2DER, uint8(0), "")
	f.Add(scryptDER, uint8(0), "pw-1234")
	f.Add(plain, uint8(1), "")
	f.Add(plain, uint8(1), "unexpected")
	f.Add(plain, uint8(2), "")
	f.Add([]byte{0x30, 0x03, 0x02, 0x01, 0x01}, uint8(0), "x")
	f.Add([]byte{}, uint8(4), "")
	f.Fuzz(func(t *testing.T, der []byte, typ uint8, password string) {
		if len(der) > MaxPEMSize || len(password) > MaxPEMSize {
			return
		}
		block := &pem.Block{Type: fuzzKeyTypes[int(typ)%len(fuzzKeyTypes)], Bytes: der}
		if block.Type == encryptedPKCS8Type && password != "" && checkKDF(der) == nil && !fuzzKDFCheap(t, der) {
			return
		}
		out, err := DecryptKey(string(pem.EncodeToMemory(block)), password)
		if err != nil {
			checkFuzzError(t, err, password)
			return
		}
		got, rest := pem.Decode(out)
		if got == nil || len(rest) != 0 || got.Type == encryptedPKCS8Type || !strings.HasSuffix(got.Type, "PRIVATE KEY") {
			t.Fatalf("DecryptKey returned %q, want one unencrypted private key block", out)
		}
		if block.Type == encryptedPKCS8Type {
			if got.Type != "PRIVATE KEY" {
				t.Fatalf("decrypted key has type %q, want PRIVATE KEY", got.Type)
			}
			if _, perr := x509.ParsePKCS8PrivateKey(got.Bytes); perr != nil {
				t.Fatalf("decrypted key does not parse: %v", perr)
			}
		} else if password != "" {
			t.Fatal("an unencrypted key was accepted with a password")
		}
	})
}

// FuzzCheckKDF checks that checkKDF never panics on arbitrary DER and accepts only
// PBES2 with PBKDF2 or scrypt within the limits.
func FuzzCheckKDF(f *testing.F) {
	m := seedMaterial(f)
	f.Add(m.pbkdf2DER)
	f.Add(m.scryptDER)
	f.Add([]byte{0x30, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, der []byte) {
		err := checkKDF(der)
		if err == nil {
			fuzzKDFCheap(t, der) // fails t if the parameters are out of bounds
			return
		}
		checkFuzzError(t, err, "")
	})
}

// FuzzConfig checks Validate and Config on arbitrary CA, certificate, key and
// password values: they never panic, fail only with the sentinels (never quoting the
// password), and a configuration they accept carries the CA pool and exactly one
// client certificate when one was given.
func FuzzConfig(f *testing.F) {
	m := seedMaterial(f)
	f.Add(m.caPEM, m.certPEM, m.keyPEM, "", false)
	f.Add(m.caPEM, m.certPEM, m.encKey, "pw-1234", true)
	f.Add(m.caPEM, m.certPEM, m.encKey, "", false)
	f.Add("", m.certPEM, "", "", false)
	f.Add("not pem", "", "", "", false)
	f.Add(m.caPEM, m.caPEM, m.keyPEM, "", false)
	f.Add("", m.certPEM, "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00\n\nAAAA\n-----END RSA PRIVATE KEY-----\n", "pw", false)
	f.Fuzz(func(t *testing.T, caPEM, certPEM, keyPEM, password string, insecure bool) {
		if !fuzzKeyPEMCheap(t, keyPEM, password) {
			return
		}
		tlsIn := &models.ConnectionTLS{
			CAPEM: caPEM, ClientCertPEM: certPEM, ClientKeyPEM: keyPEM, ClientKeyPassword: password,
			AllowInvalidHostnames: !insecure, Insecure: insecure,
		}
		if err := Validate(tlsIn); err != nil {
			checkFuzzError(t, err, password)
			return
		}
		cfg, err := Config(tlsIn)
		if err != nil {
			t.Fatalf("Config failed after Validate accepted the same material: %v", err)
		}
		if tlsIn.IsZero() {
			return
		}
		if cfg == nil {
			t.Fatal("Config returned no configuration for TLS material")
		}
		if caPEM != "" && cfg.RootCAs == nil {
			t.Fatal("CA bundle accepted but not used")
		}
		if certPEM != "" && len(cfg.Certificates) != 1 {
			t.Fatalf("client certificate accepted, %d certificates configured", len(cfg.Certificates))
		}
		if !insecure && cfg.VerifyConnection == nil {
			t.Fatal("hostname check skipped without chain verification")
		}
	})
}

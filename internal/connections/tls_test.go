package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// tlsProber records the TLS material each call received through its context.
type tlsProber struct {
	mu   sync.Mutex
	seen *models.ConnectionTLS
}

func (p *tlsProber) record(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = mongotls.FromContext(ctx)
}

func (p *tlsProber) last() *models.ConnectionTLS {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen
}

func (p *tlsProber) Ping(ctx context.Context, _ string) (connections.ServerInfo, error) {
	p.record(ctx)
	return connections.ServerInfo{Version: "8.0.1"}, nil
}

func (p *tlsProber) ListDatabases(ctx context.Context, _ string) ([]connections.Database, error) {
	p.record(ctx)
	return nil, nil
}

func (p *tlsProber) ListCollections(ctx context.Context, _, _ string) ([]connections.Collection, error) {
	p.record(ctx)
	return nil, nil
}

func ptr[T any](v T) *T { return &v }

// x509Material returns a CA and a client certificate with an encrypted key.
func x509Material(t *testing.T) (ca, cert, key, password string) {
	t.Helper()
	authority, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	client, err := authority.Client("backup")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := client.EncryptedKeyPEM("k3y-pa55")
	if err != nil {
		t.Fatal(err)
	}
	return authority.CertPEM, client.CertPEM, enc, "k3y-pa55"
}

const x509URI = "mongodb://db1.internal:27017/?tls=true&authMechanism=MONGODB-X509"

func TestTLSMaterialIsStoredMaskedAndUsed(t *testing.T) {
	p := &tlsProber{}
	svc := connections.NewService(storetest.New(t), p)
	ctx := context.Background()
	ca, cert, key, pw := x509Material(t)

	c, err := svc.Create(ctx, connections.Input{Name: "x509", URI: x509URI, TLSInput: connections.TLSInput{
		CAPEM: &ca, ClientCertPEM: &cert, ClientKeyPEM: &key, ClientKeyPassword: &pw}})
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientKeyPEM != redact.Mask || c.ClientKeyPassword != redact.Mask || c.CAPEM != ca || c.ClientCertPEM != cert {
		t.Fatalf("create result is not masked: %+v", c.ConnectionTLS)
	}
	for _, get := range []func() (*models.Connection, error){
		func() (*models.Connection, error) { return svc.Get(ctx, c.ID) },
		func() (*models.Connection, error) {
			list, listErr := svc.List(ctx)
			if listErr != nil || len(list) != 1 {
				return nil, listErr
			}
			return list[0], nil
		},
	} {
		got, getErr := get()
		if getErr != nil {
			t.Fatal(getErr)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "PRIVATE KEY") || strings.Contains(string(raw), pw) {
			t.Fatalf("API form leaks the key: %s", raw)
		}
	}

	// The engines and the prober see the full material.
	full, err := svc.Resolve(ctx, c.ID)
	if err != nil || full.ClientKeyPEM != key || full.ClientKeyPassword != pw {
		t.Fatalf("Resolve = %+v, %v", full, err)
	}
	if _, err = svc.Test(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if seen := p.last(); seen == nil || seen.ClientKeyPEM != key || seen.CAPEM != ca {
		t.Fatalf("prober got %+v", seen)
	}
	if _, err = svc.Databases(ctx, c.ID, false); err != nil || p.last() == nil {
		t.Fatalf("Databases without TLS material: %v", err)
	}
	if _, err = svc.Collections(ctx, c.ID, "app"); err != nil || p.last() == nil {
		t.Fatalf("Collections without TLS material: %v", err)
	}

	// Masked values keep the stored secrets; omitted fields keep everything.
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "x509", URI: x509URI, TLSInput: connections.TLSInput{
		ClientKeyPEM: ptr(redact.Mask), ClientKeyPassword: ptr(redact.Mask)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "renamed", URI: x509URI}); err != nil {
		t.Fatal(err)
	}
	if full, err = svc.Resolve(ctx, c.ID); err != nil || full.ClientKeyPEM != key || full.ClientKeyPassword != pw || full.CAPEM != ca {
		t.Fatalf("after updates = %+v, %v", full.Redacted(), err)
	}

	// The test of an edit form uses the stored material through the mask.
	if _, err = svc.TestURIWithTLS(ctx, x509URI, c.ID, models.ReadPreference{}, connections.TLSInput{ClientKeyPEM: ptr(redact.Mask)}); err != nil {
		t.Fatal(err)
	}
	if seen := p.last(); seen == nil || seen.ClientKeyPEM != key {
		t.Fatalf("form test got %+v", seen)
	}

	// Empty strings remove the client certificate.
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "x509", URI: x509URI, TLSInput: connections.TLSInput{
		ClientCertPEM: ptr(""), ClientKeyPEM: ptr(""), ClientKeyPassword: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if full, err = svc.Resolve(ctx, c.ID); err != nil || full.ClientKeyPEM != "" || full.ClientCertPEM != "" || full.CAPEM != ca {
		t.Fatalf("after removal = %+v, %v", full.Redacted(), err)
	}
}

func TestTLSInputValidation(t *testing.T) {
	svc := connections.NewService(storetest.New(t), &tlsProber{})
	ctx := context.Background()
	ca, cert, key, pw := x509Material(t)
	cases := map[string]struct {
		uri  string
		in   connections.TLSInput
		want error
	}{
		"bad CA":               {x509URI, connections.TLSInput{CAPEM: ptr("junk")}, mongotls.ErrInvalidCA},
		"cert without key":     {x509URI, connections.TLSInput{ClientCertPEM: &cert}, mongotls.ErrIncompleteClientCert},
		"wrong password":       {x509URI, connections.TLSInput{ClientCertPEM: &cert, ClientKeyPEM: &key, ClientKeyPassword: ptr("nope")}, mongotls.ErrKeyPassword},
		"mask without stored":  {x509URI, connections.TLSInput{ClientCertPEM: &cert, ClientKeyPEM: ptr(redact.Mask)}, connections.ErrMaskedTLSSecret},
		"insecure unconfirmed": {x509URI, connections.TLSInput{Insecure: ptr(true)}, connections.ErrInsecureNotConfirmed},
		"tls=false":            {"mongodb://h/?tls=false", connections.TLSInput{CAPEM: &ca}, connections.ErrInvalid},
		"tlsCAFile in uri":     {"mongodb://h/?tlsCAFile=%2Fetc%2Fca.pem", connections.TLSInput{CAPEM: &ca}, connections.ErrInvalid},
		"tlsInsecure in uri":   {"mongodb://h/?tlsInsecure=true", connections.TLSInput{AllowInvalidHostnames: ptr(true)}, connections.ErrInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Create(ctx, connections.Input{Name: name, URI: tc.uri, TLSInput: tc.in})
			if !errors.Is(err, tc.want) || !errors.Is(err, connections.ErrInvalid) {
				t.Fatalf("Create = %v, want %v (and ErrInvalid)", err, tc.want)
			}
			if strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), pw) {
				t.Fatalf("error leaks material: %v", err)
			}
		})
	}
}

func TestTLSInsecureNeedsConfirmationOnce(t *testing.T) {
	svc := connections.NewService(storetest.New(t), &tlsProber{})
	ctx := context.Background()
	c, err := svc.Create(ctx, connections.Input{Name: "lab", URI: "mongodb://h/?tls=true",
		TLSInput: connections.TLSInput{Insecure: ptr(true), ConfirmInsecure: true}})
	if err != nil || !c.Insecure {
		t.Fatalf("confirmed insecure = %+v, %v", c, err)
	}
	// Keeping it on needs no new confirmation.
	if c, err = svc.Update(ctx, c.ID, connections.Input{Name: "lab", URI: "mongodb://h/?tls=true",
		TLSInput: connections.TLSInput{Insecure: ptr(true)}}); err != nil || !c.Insecure {
		t.Fatalf("kept insecure = %+v, %v", c, err)
	}
	// Turning it off and on again needs one.
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "lab", URI: "mongodb://h/?tls=true",
		TLSInput: connections.TLSInput{Insecure: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "lab", URI: "mongodb://h/?tls=true",
		TLSInput: connections.TLSInput{Insecure: ptr(true)}}); !errors.Is(err, connections.ErrInsecureNotConfirmed) {
		t.Fatalf("unconfirmed insecure = %v", err)
	}
}

func TestTLSChangeClearsLastTest(t *testing.T) {
	svc := connections.NewService(storetest.New(t), &tlsProber{})
	ctx := context.Background()
	ca, _, _, _ := x509Material(t)
	c, err := svc.Create(ctx, connections.Input{Name: "tls", URI: "mongodb://h/?tls=true"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Test(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if c, err = svc.Update(ctx, c.ID, connections.Input{Name: "tls", URI: "mongodb://h/?tls=true", TLSInput: connections.TLSInput{CAPEM: &ca}}); err != nil {
		t.Fatal(err)
	}
	if c.LastTestAt != nil || c.LastTestOK {
		t.Fatalf("last test kept after a TLS change: %+v", c)
	}
}

// TestTLSMaterialForcesTLSInTheURI saves a connection with only a CA and a URI
// that does not ask for TLS: the stored URI gets tls=true, so a client built
// from it alone never connects in plain text.
func TestTLSMaterialForcesTLSInTheURI(t *testing.T) {
	svc := connections.NewService(storetest.New(t), &tlsProber{})
	ctx := context.Background()
	ca, _, _, _ := x509Material(t)
	c, err := svc.Create(ctx, connections.Input{Name: "ca-only", URI: "mongodb://u:pw@db.internal:27017/?replicaSet=rs0",
		TLSInput: connections.TLSInput{CAPEM: &ca}})
	if err != nil {
		t.Fatal(err)
	}
	full, err := svc.Resolve(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if full.URI != "mongodb://u:pw@db.internal:27017/?replicaSet=rs0&tls=true" || !strings.Contains(c.URI, "tls=true") {
		t.Fatalf("stored URI = %q, redacted %q; want tls=true", full.URI, c.URI)
	}
	// The redacted form still keeps the stored password on update.
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "renamed", URI: c.URI}); err != nil {
		t.Fatal(err)
	}
	// Without TLS material the URI is left alone.
	plain, err := svc.Create(ctx, connections.Input{Name: "plain", URI: "mongodb://h:27017/"})
	if err != nil || strings.Contains(plain.URI, "tls") {
		t.Fatalf("plain connection = %+v, %v", plain, err)
	}
}

// TestStoredTLSDoesNotFollowANewHost checks that the stored material of a
// connection is never applied to other hosts: a form test of another host does
// not use the stored CA, key or loosened checks, and moving an insecure
// connection to another host needs the confirmation again.
func TestStoredTLSDoesNotFollowANewHost(t *testing.T) {
	p := &tlsProber{}
	svc := connections.NewService(storetest.New(t), p)
	ctx := context.Background()
	ca, cert, key, pw := x509Material(t)
	const here = "mongodb://db1.internal:27017/?tls=true&authMechanism=MONGODB-X509"
	const elsewhere = "mongodb://attacker.example:27017/?tls=true&authMechanism=MONGODB-X509"
	c, err := svc.Create(ctx, connections.Input{Name: "x509", URI: here, TLSInput: connections.TLSInput{
		CAPEM: &ca, ClientCertPEM: &cert, ClientKeyPEM: &key, ClientKeyPassword: &pw,
		Insecure: ptr(true), ConfirmInsecure: true}})
	if err != nil {
		t.Fatal(err)
	}

	// Same hosts: the stored material is used through the mask.
	if _, err = svc.TestURIWithTLS(ctx, here, c.ID, models.ReadPreference{}, connections.TLSInput{}); err != nil {
		t.Fatal(err)
	}
	if seen := p.last(); seen == nil || seen.ClientKeyPEM != key || !seen.Insecure {
		t.Fatalf("same-host test got %+v", seen)
	}
	// Other hosts: nothing stored is applied.
	if _, err = svc.TestURIWithTLS(ctx, elsewhere, c.ID, models.ReadPreference{}, connections.TLSInput{}); err != nil {
		t.Fatal(err)
	}
	if seen := p.last(); seen != nil {
		t.Fatalf("other-host test got the stored material %+v", seen.Redacted())
	}
	if _, err = svc.TestURIWithTLS(ctx, elsewhere, c.ID, models.ReadPreference{}, connections.TLSInput{
		ClientCertPEM: &cert, ClientKeyPEM: ptr(redact.Mask)}); !errors.Is(err, connections.ErrMaskedTLSSecret) {
		t.Fatalf("other-host test with the masked key = %v; want ErrMaskedTLSSecret", err)
	}
	if _, err = svc.TestURIWithTLS(ctx, elsewhere, c.ID, models.ReadPreference{}, connections.TLSInput{
		Insecure: ptr(true)}); !errors.Is(err, connections.ErrInsecureNotConfirmed) {
		t.Fatalf("other-host insecure test = %v; want ErrInsecureNotConfirmed", err)
	}

	// Moving the insecure connection to other hosts needs the confirmation again.
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "x509", URI: elsewhere}); !errors.Is(err, connections.ErrInsecureNotConfirmed) {
		t.Fatalf("move without confirmation = %v; want ErrInsecureNotConfirmed", err)
	}
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "x509", URI: elsewhere,
		TLSInput: connections.TLSInput{ConfirmInsecure: true}}); err != nil {
		t.Fatalf("confirmed move: %v", err)
	}
	// Renaming on the same hosts keeps it without a new confirmation.
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "renamed", URI: elsewhere}); err != nil {
		t.Fatalf("rename: %v", err)
	}
}

// TestTLSFlagChangesAreAudited checks the audit annotations of changes of the
// loosened TLS checks, and that unchanged flags add none.
func TestTLSFlagChangesAreAudited(t *testing.T) {
	svc := connections.NewService(storetest.New(t), &tlsProber{})
	const uri = "mongodb://h/?tls=true"
	ctx := auditlog.WithAnnotations(context.Background())
	c, err := svc.Create(ctx, connections.Input{Name: "lab", URI: uri,
		TLSInput: connections.TLSInput{Insecure: ptr(true), ConfirmInsecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	if a := auditlog.Annotations(ctx); a["tls_insecure_from"] != "false" || a["tls_insecure_to"] != "true" || a["tls_allow_invalid_hostnames_to"] != "" {
		t.Fatalf("create annotations = %v", a)
	}
	ctx = auditlog.WithAnnotations(context.Background())
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "lab", URI: uri,
		TLSInput: connections.TLSInput{Insecure: ptr(false), AllowInvalidHostnames: ptr(true)}}); err != nil {
		t.Fatal(err)
	}
	a := auditlog.Annotations(ctx)
	if a["tls_insecure_from"] != "true" || a["tls_insecure_to"] != "false" ||
		a["tls_allow_invalid_hostnames_from"] != "false" || a["tls_allow_invalid_hostnames_to"] != "true" {
		t.Fatalf("update annotations = %v", a)
	}
	ctx = auditlog.WithAnnotations(context.Background())
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "renamed", URI: uri}); err != nil {
		t.Fatal(err)
	}
	if a = auditlog.Annotations(ctx); len(a) != 0 {
		t.Fatalf("rename annotations = %v", a)
	}
}

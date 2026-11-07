package mongotls_test

import (
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

func TestExpiriesReportTheEarliestCertificateOfEachKind(t *testing.T) {
	ca1, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	client, err := ca1.ClientExpiring("c", soon)
	if err != nil {
		t.Fatal(err)
	}
	// The second CA is created later, so it expires later: the first one counts.
	got := mongotls.Expiries(&models.ConnectionTLS{CAPEM: ca2.CertPEM + ca1.CertPEM, ClientCertPEM: client.CertPEM, ClientKeyPEM: client.KeyPEM})
	if len(got) != 2 || got[0].Kind != mongotls.KindClient || got[1].Kind != mongotls.KindCA {
		t.Fatalf("expiries = %+v", got)
	}
	if !got[0].NotAfter.Equal(soon) || len(got[0].Fingerprint) != 64 {
		t.Errorf("client = %+v, want expiry %v and a SHA-256 fingerprint", got[0], soon)
	}
	one := mongotls.Expiries(&models.ConnectionTLS{CAPEM: ca1.CertPEM})
	two := mongotls.Expiries(&models.ConnectionTLS{CAPEM: ca2.CertPEM})
	if len(one) != 1 || len(two) != 1 {
		t.Fatalf("single CAs: %+v %+v", one, two)
	}
	want := one[0].NotAfter
	if two[0].NotAfter.Before(want) {
		want = two[0].NotAfter
	}
	if !got[1].NotAfter.Equal(want) {
		t.Errorf("CA expiry = %v, want the earliest of the bundle, %v", got[1].NotAfter, want)
	}
}

func TestExpiriesIgnoreMissingAndUnparseableMaterial(t *testing.T) {
	if got := mongotls.Expiries(nil); got != nil {
		t.Errorf("nil: %+v", got)
	}
	if got := mongotls.Expiries(&models.ConnectionTLS{}); len(got) != 0 {
		t.Errorf("zero: %+v", got)
	}
	bad := &models.ConnectionTLS{CAPEM: "not pem", ClientCertPEM: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"}
	if got := mongotls.Expiries(bad); len(got) != 0 {
		t.Errorf("garbage: %+v", got)
	}
}

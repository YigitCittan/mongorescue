package mongoconn

import (
	"context"
	"crypto/tls"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/mongotls/mongotlstest"
)

func TestClientOptionsUseTLSMaterialFromContext(t *testing.T) {
	ca, err := mongotlstest.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Client("backup")
	if err != nil {
		t.Fatal(err)
	}
	uri := "mongodb://db.example:27017/?authMechanism=MONGODB-X509"

	if opts := clientOptions(context.Background(), uri); opts.TLSConfig != nil {
		t.Fatalf("no material, yet a TLS config: %+v", opts.TLSConfig)
	}

	material := &models.ConnectionTLS{CAPEM: ca.CertPEM, ClientCertPEM: client.CertPEM, ClientKeyPEM: client.KeyPEM}
	opts := clientOptions(mongotls.NewContext(context.Background(), material), uri)
	if opts.TLSConfig == nil || opts.TLSConfig.RootCAs == nil || len(opts.TLSConfig.Certificates) != 1 {
		t.Fatalf("TLS config = %+v", opts.TLSConfig)
	}
	if opts.TLSConfig.InsecureSkipVerify {
		t.Fatal("verification is off by default")
	}
	if opts.Auth == nil || opts.Auth.AuthMechanism != "MONGODB-X509" {
		t.Fatalf("auth = %+v", opts.Auth)
	}
}

func TestClientOptionsFailClosedOnBadMaterial(t *testing.T) {
	ctx := mongotls.NewContext(context.Background(), &models.ConnectionTLS{CAPEM: "not a certificate", Insecure: true})
	opts := clientOptions(ctx, "mongodb://db.example/")
	if opts.TLSConfig == nil || opts.TLSConfig.VerifyConnection == nil {
		t.Fatalf("bad material yields %+v; want a config that fails every handshake", opts.TLSConfig)
	}
	if opts.TLSConfig.VerifyConnection(tls.ConnectionState{}) == nil {
		t.Fatal("handshake check passed")
	}
}

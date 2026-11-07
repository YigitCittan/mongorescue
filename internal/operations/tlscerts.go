package operations

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// TLSCertExpiryChecked announces certificates of connections that newly reached
// an expiry threshold (see connections.TLSExpiryTracker): a
// connection.tls_cert_expiring event at 30 and at 7 days before the expiry and a
// connection.tls_cert_expired event once it passed. Events name the connection
// and the kind of certificate, never PEM.
func (s *Service) TLSCertExpiryChecked(ctx context.Context, announce []connections.TLSCertStatus) {
	ctx = context.WithoutCancel(ctx)
	now := s.now().UTC()
	for _, st := range announce {
		typ := events.ConnectionTLSCertExpiring
		var detail string
		kind := tlsKindText(st.Kind)
		at := st.NotAfter.UTC().Format(time.RFC3339)
		switch st.Level {
		case models.TLSExpiryExpired:
			typ = events.ConnectionTLSCertExpired
			detail = fmt.Sprintf("the %s of connection %q expired at %s; backups and restores through it fail until it is replaced",
				kind, st.ConnectionName, at)
		case models.TLSExpiryCritical:
			detail = fmt.Sprintf("the %s of connection %q expires at %s (within 7 days); replace it in the connection's TLS settings",
				kind, st.ConnectionName, at)
		case models.TLSExpiryWarn:
			detail = fmt.Sprintf("the %s of connection %q expires at %s (within 30 days); replace it in the connection's TLS settings",
				kind, st.ConnectionName, at)
		default:
			continue
		}
		s.logger.WarnContext(ctx, "a connection TLS certificate expires or has expired", slog.String("connection_id", st.ConnectionID),
			slog.String("kind", st.Kind), slog.Time("not_after", st.NotAfter.UTC()))
		s.publish(ctx, events.Event{Type: typ, Time: now, ConnectionID: st.ConnectionID, Action: st.Kind, Detail: redact.Text(detail)})
	}
}

// tlsKindText describes a certificate kind for people.
func tlsKindText(kind string) string {
	if kind == "client" {
		return "x509 client certificate"
	}
	return "CA certificate"
}

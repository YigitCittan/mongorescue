package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAuditDefaultsAndValidation(t *testing.T) {
	svc := newSvc(t, &memRepo{})
	if a := svc.Current().Audit; a.RetentionDays != DefaultAuditRetentionDays || a.WebhookURL != "" || a.WebhookSecret != "" {
		t.Fatalf("defaults = %+v", a)
	}
	ctx := context.Background()
	for _, p := range []AuditPatch{
		{RetentionDays: ptr(29)},
		{RetentionDays: ptr(36501)},
		{WebhookURL: ptr("ftp://siem.example.com/in")},
		{WebhookURL: ptr("https:///no-host")},
		{WebhookURL: ptr("https://siem.example.com/in\nX-Evil: 1")},
		{WebhookURL: ptr("https://siem.example.com/" + strings.Repeat("a", 2100))},
		{WebhookSecret: ptr(strings.Repeat("s", 1025))},
	} {
		if _, err := svc.Update(ctx, Patch{Audit: &p}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Update(%+v) = %v; want ErrInvalid", p, err)
		}
	}
	if _, err := svc.Update(ctx, Patch{Audit: &AuditPatch{WebhookSecret: ptr(SecretMask)}}); !errors.Is(err, ErrMaskedSecret) {
		t.Fatalf("masked secret without a stored one = %v", err)
	}
	if _, err := svc.Update(ctx, Patch{Audit: &AuditPatch{WebhookURL: ptr(SecretMask)}}); !errors.Is(err, ErrMaskedSecret) {
		t.Fatalf("masked URL without a stored one = %v", err)
	}
}

// TestAuditWebhookSecretsAreMaskedAndKept proves the webhook URL (beyond its origin)
// and the signing secret never leave the service unmasked, are stored as secrets,
// and survive being sent back masked.
func TestAuditWebhookSecretsAreMaskedAndKept(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	ctx := context.Background()
	const url = "https://siem.example.com/hooks/T0/s3cr3t-path?token=abc"
	const secret = "hmac-signing-secret"
	masked, err := svc.Update(ctx, Patch{Audit: &AuditPatch{RetentionDays: ptr(90), WebhookURL: ptr(url), WebhookSecret: ptr(secret)}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(masked)
	if strings.Contains(string(raw), "s3cr3t-path") || strings.Contains(string(raw), secret) || strings.Contains(string(raw), "token=abc") {
		t.Fatalf("masked settings leak a secret: %s", raw)
	}
	if masked.Audit.WebhookURL != "https://siem.example.com/"+SecretMask || masked.Audit.WebhookSecret != SecretMask || masked.Audit.RetentionDays != 90 {
		t.Fatalf("masked audit = %+v", masked.Audit)
	}
	if !IsSecret(KeyAuditWebhookURL) || !IsSecret(KeyAuditWebhookSecret) {
		t.Fatal("the webhook URL and secret must be sealed at rest")
	}
	// Sending the masked values back keeps the stored ones.
	if _, err = svc.Update(ctx, Patch{Audit: &AuditPatch{WebhookURL: &masked.Audit.WebhookURL, WebhookSecret: &masked.Audit.WebhookSecret}}); err != nil {
		t.Fatal(err)
	}
	if a := svc.Current().Audit; a.WebhookURL != url || a.WebhookSecret != secret {
		t.Fatalf("after echoing the masks = %+v", a)
	}
	// A bare origin is shown as is; "" turns forwarding off.
	if m, _ := svc.Update(ctx, Patch{Audit: &AuditPatch{WebhookURL: ptr("https://siem.example.com")}}); m.Audit.WebhookURL != "https://siem.example.com" {
		t.Fatalf("bare origin shown as %q", m.Audit.WebhookURL)
	}
	if _, err = svc.Update(ctx, Patch{Audit: &AuditPatch{WebhookURL: ptr(""), WebhookSecret: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if a := newSvc(t, repo).Current().Audit; a.WebhookURL != "" || a.WebhookSecret != "" || a.RetentionDays != 90 {
		t.Fatalf("reloaded audit = %+v", a)
	}
}

func TestMaskEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"https://h.example":              "https://h.example",
		"https://h.example/":             "https://h.example/",
		"https://h.example:8443/x":       "https://h.example:8443/" + SecretMask,
		"https://u:p@h.example":          "https://h.example/" + SecretMask,
		"https://h.example/?sig=1":       "https://h.example/" + SecretMask,
		"not a url":                      SecretMask,
		"http://[::1]:9000/hook#section": "http://[::1]:9000/" + SecretMask,
	} {
		if got := maskEndpoint(in); got != want {
			t.Errorf("maskEndpoint(%q) = %q; want %q", in, got, want)
		}
	}
}

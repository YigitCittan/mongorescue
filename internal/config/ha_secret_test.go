package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestHATargetNeverCarriesCredentials checks that a storage location with a
// credential anywhere is refused without echoing it, and that the labels used in
// errors and logs never show one.
func TestHATargetNeverCarriesCredentials(t *testing.T) {
	const secret = "s3cr3tValue"
	for _, v := range []string{
		"s3://bucket/ha?endpoint=https://AKIA:" + secret + "@minio:9000",
		"s3://bucket/ha?endpoint=https://minio:9000/?X-Amz-Signature=" + secret,
		"s3://bucket/ha?endpoint=https://minio:9000&secret_access_key=" + secret,
		"s3://AKIA:" + secret + "@bucket/ha",
	} {
		_, err := ParseHATarget(v)
		if !errors.Is(err, ErrInvalidHA) {
			t.Errorf("ParseHATarget(%q) = %v; want ErrInvalidHA", v, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error of %q shows the credential: %v", v, err)
		}
		if label := HATargetLabel(v); strings.Contains(label, secret) {
			t.Errorf("HATargetLabel(%q) = %q shows the credential", v, label)
		}
		err = HAConfig{Mode: HAModeActivePassive, Target: v, Cluster: "prod", LeaseTTL: 30 * time.Second,
			RenewInterval: 10 * time.Second, SnapshotInterval: 5 * time.Minute}.Validate()
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("Validate(%q) = %v; want an error without the credential", v, err)
		}
	}
	if got := HATargetLabel("s3://bucket/ha?endpoint=https://minio:9000&region=eu-west-1"); got != "s3://bucket/ha (endpoint https://minio:9000)" {
		t.Errorf("label = %q", got)
	}
}

package models

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskEndpoint(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"https://hc-ping.com":               "https://hc-ping.com",
		"https://hc-ping.com/":              "https://hc-ping.com/",
		"https://hc-ping.com/uuid":          "https://hc-ping.com/" + SecretMask,
		"https://hc-ping.com/?token=x":      "https://hc-ping.com/" + SecretMask,
		"https://u:p@hc-ping.com/":          "https://hc-ping.com/" + SecretMask,
		"no scheme":                         SecretMask,
		"https://push.example.com:8443/a/b": "https://push.example.com:8443/" + SecretMask,
	}
	for in, want := range cases {
		if got := MaskEndpoint(in); got != want {
			t.Errorf("MaskEndpoint(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestResolveHeartbeatURL(t *testing.T) {
	const stored = "https://hc-ping.com/secret-uuid"
	cases := []struct {
		name, in, stored, want string
		err                    error
	}{
		{"new", " https://hc-ping.com/new ", stored, "https://hc-ping.com/new", nil},
		{"mask keeps", SecretMask, stored, stored, nil},
		{"masked form keeps", MaskEndpoint(stored), stored, stored, nil},
		{"empty clears", "", stored, "", nil},
		{"masked for another host", "https://other.example.com/" + SecretMask, stored, "", ErrMaskedHeartbeatURL},
		{"mask without stored", SecretMask, "", "", ErrMaskedHeartbeatURL},
		{"masked path elsewhere", "https://hc-ping.com/x/" + SecretMask, stored, "", ErrMaskedHeartbeatURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveHeartbeatURL(tc.in, tc.stored)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("ResolveHeartbeatURL(%q) = %q, %v; want %q, %v", tc.in, got, err, tc.want, tc.err)
			}
		})
	}
}

func TestValidateHeartbeatURL(t *testing.T) {
	for _, ok := range []string{"", "https://hc-ping.com/uuid", "http://10.0.0.5:8000/ping/x?y=1"} {
		if err := ValidateHeartbeatURL(ok); err != nil {
			t.Errorf("ValidateHeartbeatURL(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"hc-ping.com/uuid", "ftp://x/y", "https://", "https://u:p@x/y", "https://x/\ny", "https://x/" + strings.Repeat("a", MaxHeartbeatURLLength)} {
		if err := ValidateHeartbeatURL(bad); !errors.Is(err, ErrInvalidHeartbeatURL) {
			t.Errorf("ValidateHeartbeatURL(%q) = %v; want ErrInvalidHeartbeatURL", bad, err)
		}
	}
}

func TestJobRedacted(t *testing.T) {
	j := &Job{ID: "j", HeartbeatURL: "https://hc-ping.com/secret-uuid"}
	r := j.Redacted()
	if r.HeartbeatURL != "https://hc-ping.com/"+SecretMask || j.HeartbeatURL != "https://hc-ping.com/secret-uuid" {
		t.Fatalf("Redacted = %q (original %q)", r.HeartbeatURL, j.HeartbeatURL)
	}
	if got := RedactJobs([]*Job{j, nil}); got[0].HeartbeatURL != r.HeartbeatURL || got[1] != nil {
		t.Fatalf("RedactJobs = %v", got)
	}
	var none *Job
	if none.Redacted() != nil {
		t.Fatal("nil job redacted to non-nil")
	}
}

package redact

import (
	"strings"
	"testing"
)

// TestTextNeverShowsThePassword feeds Text the forms connection strings take in
// driver errors, tool stderr and logs, and checks that no secret survives.
func TestTextNeverShowsThePassword(t *testing.T) {
	for _, tc := range []struct {
		in      string
		secrets []string
	}{
		{"dial mongodb://u:SEC1@h:27017 failed", []string{"SEC1"}},
		{"error:mongodb://u:SEC2@h", []string{"SEC2"}},
		{"uri=mongodb://u:SEC3@h/?authSource=admin", []string{"SEC3"}},
		{`"mongodb://u:SEC4@h"`, []string{"SEC4"}},
		{"(mongodb://u:SEC5@h)", []string{"SEC5"}},
		{"mongodb+srv://user:SEC6@cluster0.example.net/?retryWrites=true", []string{"SEC6"}},
		{"MONGODB://U:SEC7@H", []string{"SEC7"}},
		{"mongodb://u:p@SEC8@h/", []string{"SEC8"}},
		{"mongodb://u:pa://SEC9@h:27017", []string{"SEC9", "pa://"}},
		{"mongodb://u:SEC10/x?y#z@h/", []string{"SEC10"}},
		{"mongodb://u:SEC11@h1:27017,h2:27017,h3:27017/?replicaSet=rs0", []string{"SEC11"}},
		{"mongodb://h/?authMechanismProperties=AWS_SESSION_TOKEN:SEC12", []string{"SEC12"}},
		{"mongodb://h/?password=SEC13&x=1", []string{"SEC13"}},
		{"mongodb://h/?tlsCertificateKeyFilePassword=SEC14", []string{"SEC14"}},
		{"mongodb://h/?Password=SEC15", []string{"SEC15"}},
		{"mongodb://h/?%70assword=SEC16", []string{"SEC16"}},
		{"a mongodb://u:SEC17@h and mongodb://v:SEC18@g", []string{"SEC17", "SEC18"}},
		{"mongodb://u:SEC19@h\nmongodb://v:SEC20@g", []string{"SEC19", "SEC20"}},
		{"https://hooks.example.com/x?token=SEC21", []string{"SEC21"}},
		{"https://api.example.com/v1?api_key=SEC22&access_token=SEC23&sig=SEC24", []string{"SEC22", "SEC23", "SEC24"}},
		{"postgres://u:SEC25@h", []string{"SEC25"}},
		{"https://user:SEC26@proxy.example.com:3128", []string{"SEC26"}},
	} {
		got := Text(tc.in)
		for _, s := range tc.secrets {
			if strings.Contains(got, s) {
				t.Errorf("Text(%q) = %q shows %q", tc.in, got, s)
			}
		}
		if URI(tc.in) != tc.in && !strings.Contains(URI(tc.in), Mask) {
			t.Errorf("URI(%q) changed the string without masking", tc.in)
		}
	}
}

// TestTextKeepsHarmlessText checks that redaction does not mangle text without
// credentials, so logs stay useful.
func TestTextKeepsHarmlessText(t *testing.T) {
	for _, s := range []string{
		"backup completed in 3s",
		"mongodb://db.internal:27017/?replicaSet=rs0",
		"https://hooks.example.com/services",
		"see http://localhost:8080/ for the dashboard",
	} {
		if got := Text(s); got != s {
			t.Errorf("Text(%q) = %q; want it unchanged", s, got)
		}
	}
}

package mongouri

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		uri   string
		valid bool
	}{
		{name: "standard", uri: "mongodb://localhost:27017", valid: true},
		{name: "standard with db and options", uri: "mongodb://h1:27017,h2:27017/db?replicaSet=rs0", valid: true},
		{name: "credentials", uri: "mongodb://u:p@h/db", valid: true},
		{name: "percent-encoded credentials", uri: "mongodb://u%40x:p%2F%3F%23%20s@h/db", valid: true},
		{name: "empty username", uri: "mongodb://:secret@h/db", valid: true},
		{name: "srv", uri: "mongodb+srv://u:p@cluster0.example.net/db?retryWrites=true", valid: true},
		{name: "username only", uri: "mongodb://u@h", valid: true},
		{name: "at sign in query value", uri: "mongodb://h/?appName=me@corp", valid: true},
		{name: "at sign in auth mechanism properties", uri: "mongodb://u:p@h/?authMechanismProperties=SERVICE_NAME:mongodb@REALM", valid: true},
		{name: "ipv6 host with port", uri: "mongodb://u:p@[::1]:27017/db", valid: true},
		{name: "unix socket host", uri: "mongodb://%2Ftmp%2Fmongodb-27017.sock/db", valid: true},
		{name: "trailing slash no db", uri: "mongodb://h:27017/", valid: true},
		{name: "empty query", uri: "mongodb://h/?", valid: true},

		{name: "empty", uri: "", valid: false},
		{name: "wrong scheme", uri: "postgres://u:p@h/db", valid: false},
		{name: "http scheme", uri: "http://h", valid: false},
		{name: "no scheme", uri: "localhost:27017", valid: false},
		{name: "uppercase scheme", uri: "MONGODB://h", valid: false},
		{name: "raw slash in password", uri: "mongodb://u:pa/ss@h/db", valid: false},
		{name: "raw question mark in password", uri: "mongodb://u:pa?ss@h/db", valid: false},
		{name: "raw hash in password", uri: "mongodb://u:pa#ss@h/db", valid: false},
		{name: "raw at in password", uri: "mongodb://u:p@ss@h1,h2/db", valid: false},
		{name: "space in password", uri: "mongodb://u:pa ss@h/db", valid: false},
		{name: "tab in username", uri: "mongodb://u\tx:p@h/db", valid: false},
		{name: "missing host", uri: "mongodb://", valid: false},
		{name: "missing host after userinfo", uri: "mongodb://u:p@/db", valid: false},
		{name: "space in host", uri: "mongodb://h ost/db", valid: false},
		{name: "raw slash in password cut to non-numeric port", uri: "mongodb://u:pa/ss@h/db", valid: false},
		{name: "raw slash in numeric password leaves at in path", uri: "mongodb://u:123/ss@h/db", valid: false},
		{name: "raw question mark in numeric password leaves bare option", uri: "mongodb://u:123?ss@h/db", valid: false},
		{name: "non-numeric port", uri: "mongodb://h:abc/db", valid: false},
		{name: "port too long", uri: "mongodb://h:123456/db", valid: false},
		{name: "empty port", uri: "mongodb://h:/db", valid: false},
		{name: "empty host in list", uri: "mongodb://h1,,h2/db", valid: false},
		{name: "fragment", uri: "mongodb://h/db#x", valid: false},
		{name: "option without value", uri: "mongodb://h/?replicaSet", valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.uri)
			if tt.valid && err != nil {
				t.Fatalf("Validate(%q) = %v; want nil", tt.uri, err)
			}
			if !tt.valid && !errors.Is(err, ErrInvalidMongoURI) {
				t.Fatalf("Validate(%q) = %v; want ErrInvalidMongoURI", tt.uri, err)
			}
		})
	}
}

func TestValidateErrorDoesNotEchoURI(t *testing.T) {
	err := Validate("mongodb://u:pa/secret@h/db")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error must not contain the URI, got %v", err)
	}
	if !strings.Contains(err.Error(), "percent-encoded") {
		t.Fatalf("error should tell the user to percent-encode credentials, got %q", err)
	}
}

package config

import (
	"testing"
)

func TestSanitizeURI(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Empty URI",
			input:    "",
			expected: "",
		},
		{
			name:     "Standard URI without credentials",
			input:    "mongodb://localhost:27017/mydb",
			expected: "mongodb://localhost:27017/mydb",
		},
		{
			name:     "Standard URI with username and password",
			input:    "mongodb://admin:superSecret123@localhost:27017/mydb?authSource=admin",
			expected: "mongodb://admin:******@localhost:27017/mydb?authSource=admin",
		},
		{
			name:     "SRV Atlas URI with password and special characters",
			input:    "mongodb+srv://clusterUser:p%40ssw0rd!@cluster0.mongodb.net/prod?retryWrites=true&w=majority",
			expected: "mongodb+srv://clusterUser:******@cluster0.mongodb.net/prod?retryWrites=true&w=majority",
		},
		{
			name:     "URI with username only (no password)",
			input:    "mongodb://appuser@localhost:27017/mydb",
			expected: "mongodb://appuser@localhost:27017/mydb",
		},
		{
			name:     "Replica Set URI with multiple hosts and credentials",
			input:    "mongodb://mongo_user:secretPass@node1.internal:27017,node2.internal:27017/db?replicaSet=rs0",
			expected: "mongodb://mongo_user:******@node1.internal:27017,node2.internal:27017/db?replicaSet=rs0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeURI(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeURI(%q) = %q; want %q", tt.input, got, tt.expected)
			}
		})
	}
}

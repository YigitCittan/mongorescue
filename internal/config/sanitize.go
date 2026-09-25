package config

import "github.com/yigitcittan/mongorescue/internal/redact"

// SanitizeURI masks passwords in MongoDB connection strings to prevent credential leaks
// in logs, HTTP responses, or error messages.
// It supports standard (mongodb://), DNS seedlist (mongodb+srv://), and multi-host replica sets.
//
// It is a thin wrapper around redact.URI, kept for backward compatibility with existing callers.
func SanitizeURI(rawURI string) string {
	return redact.URI(rawURI)
}

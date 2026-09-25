package models

import (
	"time"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Connection is a managed MongoDB server (or replica set / cluster) that jobs back up
// from and restores write into.
type Connection struct {
	// ID is the unique identifier (e.g. "conn_1a2b3c4d").
	ID string `json:"id"`

	// Name is a human-readable label.
	Name string `json:"name"`

	// URI is the full connection string including credentials. It is encrypted at rest
	// and only ever returned to API clients in redacted form (see Redacted).
	URI string `json:"uri"`

	// Description is an optional free-text note.
	Description string `json:"description,omitempty"`

	// CreatedAt is when the connection was added.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is when the connection was last modified.
	UpdatedAt time.Time `json:"updated_at"`

	// LastTestAt is when the connection was last tested.
	LastTestAt *time.Time `json:"last_test_at,omitempty"`

	// LastTestOK reports whether the last test succeeded.
	LastTestOK bool `json:"last_test_ok"`

	// LastTestError is the redacted failure reason of the last test.
	LastTestError string `json:"last_test_error,omitempty"`

	// ServerVersion is the MongoDB version reported by the last successful test.
	ServerVersion string `json:"server_version,omitempty"`
}

// Redacted returns a copy that is safe to serialize to API clients or logs: the URI
// password and sensitive query options are masked.
func (c *Connection) Redacted() *Connection {
	if c == nil {
		return nil
	}
	clone := *c
	clone.URI = redact.URI(c.URI)
	if c.LastTestAt != nil {
		t := *c.LastTestAt
		clone.LastTestAt = &t
	}
	return &clone
}

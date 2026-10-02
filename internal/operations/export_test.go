package operations

import "time"

// SetBulkCap lowers the item cap of bulk operations, so tests need not store
// MaxBulkItems records.
func SetBulkCap(s *Service, n int) { s.bulkCap = n }

// SetNow replaces the service's clock.
func SetNow(s *Service, now func() time.Time) { s.now = now }

package operations

// SetBulkCap lowers the item cap of bulk operations, so tests need not store
// MaxBulkItems records.
func SetBulkCap(s *Service, n int) { s.bulkCap = n }

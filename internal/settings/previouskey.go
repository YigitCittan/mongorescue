package settings

// WarningPreviousKey identifies the notice that secret.key.previous is no longer
// needed: every metadata snapshot sealed with it was pruned, but no recovery kit was
// downloaded since the rotation, so MongoRescue did not delete it on its own.
const WarningPreviousKey = "previous_secret_key"

// previousKeyMessage is the text of the WarningPreviousKey warning.
const previousKeyMessage = "The metadata snapshots sealed with the secret key a rotation replaced were deleted after the delete grace period. " +
	"Download a recovery kit (MongoRescue then deletes secret.key.previous), or delete secret.key.previous from the data directory yourself."

// SetPreviousKeyWarning shows (or hides) WarningPreviousKey. The state is not
// stored: the metadata backup service recomputes it while it runs.
func (s *Service) SetPreviousKeyWarning(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.previousKey = active
}

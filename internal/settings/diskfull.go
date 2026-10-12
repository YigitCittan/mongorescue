package settings

// WarningDiskFull identifies the warning that a write to the metadata database
// failed because the data directory's disk is full: new backups and restores are
// refused until space is freed (see internal/diskguard).
const WarningDiskFull = "data_dir_full"

// diskFullMessage is the text of the WarningDiskFull warning.
const diskFullMessage = "The data directory is full: the metadata database cannot be written, so new backups and restores are refused. " +
	"Free space on the disk of the data directory; MongoRescue resumes on its own once there is enough."

// SetDiskFullWarning shows (or hides) WarningDiskFull. The state is not stored
// (the disk is full): the data directory's space guard sets it while the episode
// lasts.
func (s *Service) SetDiskFullWarning(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diskFull = active
}

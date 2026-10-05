package operations

import (
	"github.com/yigitcittan/mongorescue/internal/models"
)

// validateJobThrottling checks and normalises a job's read preference, upload
// cap, mongodump parallelism and backup window: an empty tag list becomes none and
// a window that sets nothing is removed. Failures are ErrInvalid errors.
func validateJobThrottling(job *models.Job) error {
	if len(job.ReadPreferenceTags) == 0 {
		job.ReadPreferenceTags = nil
	}
	if err := job.ReadPref().Validate(); err != nil {
		return invalid(err)
	}
	if err := models.ValidateUploadMbps(job.MaxUploadMbps); err != nil {
		return invalid(err)
	}
	if job.NumParallelCollections < 0 || job.NumParallelCollections > models.MaxNumParallelCollections {
		return invalid(models.ErrInvalidParallelCollections)
	}
	if job.BackupWindow.IsZero() {
		job.BackupWindow = nil
		return nil
	}
	if err := job.BackupWindow.Normalize(); err != nil {
		return invalid(err)
	}
	return nil
}

// applyThrottlingUpdate copies the read preference, throttling and window fields
// an update sets onto job.
func applyThrottlingUpdate(job *models.Job, u JobUpdate) {
	if u.ReadPreference != nil {
		job.ReadPreference = *u.ReadPreference
		job.ReadPreferenceTags = models.CloneTagSets(u.ReadPreferenceTags)
	}
	job.MaxUploadMbps = derefOr(u.MaxUploadMbps, job.MaxUploadMbps)
	job.NumParallelCollections = derefOr(u.NumParallelCollections, job.NumParallelCollections)
	if u.BackupWindow != nil {
		job.BackupWindow = u.BackupWindow.Clone()
	}
}

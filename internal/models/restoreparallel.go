package models

import "fmt"

// Limits and tool defaults of the mongorestore parallelism options.
const (
	// MaxRestoreParallelCollections bounds num_parallel_collections of a restore
	// (mongorestore's --numParallelCollections).
	MaxRestoreParallelCollections = MaxNumParallelCollections
	// MaxInsertionWorkersPerCollection bounds insertion_workers_per_collection of a
	// restore (mongorestore's --numInsertionWorkersPerCollection).
	MaxInsertionWorkersPerCollection = 32
	// MongorestoreParallelCollections is mongorestore's own default of
	// --numParallelCollections, which applies when a restore sets none.
	MongorestoreParallelCollections = 4
	// MongorestoreInsertionWorkers is mongorestore's own default of
	// --numInsertionWorkersPerCollection.
	MongorestoreInsertionWorkers = 1
)

// Restore parallelism errors.
var (
	// ErrInvalidRestoreParallelCollections is returned for a restore's
	// num_parallel_collections outside 0..MaxRestoreParallelCollections.
	ErrInvalidRestoreParallelCollections = fmt.Errorf("num_parallel_collections must be between 1 and %d (0 for the default)", MaxRestoreParallelCollections)
	// ErrInvalidInsertionWorkers is returned for a restore's
	// insertion_workers_per_collection outside 0..MaxInsertionWorkersPerCollection.
	ErrInvalidInsertionWorkers = fmt.Errorf("insertion_workers_per_collection must be between 1 and %d (0 for the default)", MaxInsertionWorkersPerCollection)
)

// ValidateRestoreParallelism checks the two mongorestore parallelism values of a
// restore request, its record or a setting: 0 means "the default", otherwise they
// are between 1 and MaxRestoreParallelCollections and MaxInsertionWorkersPerCollection.
func ValidateRestoreParallelism(collections, workers int) error {
	if collections < 0 || collections > MaxRestoreParallelCollections {
		return ErrInvalidRestoreParallelCollections
	}
	if workers < 0 || workers > MaxInsertionWorkersPerCollection {
		return ErrInvalidInsertionWorkers
	}
	return nil
}

// ValidateParallelism checks the request's parallelism options (see
// ValidateRestoreParallelism).
func (r RestoreRequest) ValidateParallelism() error {
	return ValidateRestoreParallelism(r.NumParallelCollections, r.InsertionWorkersPerCollection)
}

// EffectiveRestoreParallelism resolves the mongorestore parallelism of a restore:
// each value is the request's when set, else the configured default, else
// mongorestore's own (MongorestoreParallelCollections, MongorestoreInsertionWorkers).
func EffectiveRestoreParallelism(reqCollections, reqWorkers, defCollections, defWorkers int) (collections, workers int) {
	collections, workers = reqCollections, reqWorkers
	if collections <= 0 {
		collections = defCollections
	}
	if collections <= 0 {
		collections = MongorestoreParallelCollections
	}
	if workers <= 0 {
		workers = defWorkers
	}
	if workers <= 0 {
		workers = MongorestoreInsertionWorkers
	}
	return collections, workers
}

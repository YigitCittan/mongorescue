package oplog

import "go.mongodb.org/mongo-driver/v2/bson"

// LimitAt returns the position t:i as a Filter.Limit, for callers that must not
// import the driver's bson package (only internal/oplog may).
func LimitAt(t, i uint32) bson.Timestamp {
	return bson.Timestamp{T: t, I: i}
}

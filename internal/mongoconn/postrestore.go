package mongoconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/postrestore"
)

// Server error codes a post-restore command treats as done: dropping a collection or
// an index that is not there (re-applied erasures must be idempotent).
const (
	namespaceNotFoundCode = 26
	indexNotFoundCode     = 27
)

// maxCommandErrorLength caps the server message quoted in a failed command's error.
const maxCommandErrorLength = 200

// ErrPostRestoreCommand is returned when a post-restore command fails on the server.
var ErrPostRestoreCommand = errors.New("mongoconn: post-restore command failed")

// CheckPostRestoreCommand reports whether command, an allowed post-restore command
// (postrestore.Parse), is valid extended JSON the driver can send.
func (p *Prober) CheckPostRestoreCommand(command json.RawMessage) error {
	if _, err := postrestore.Parse(command); err != nil {
		return err
	}
	if _, err := commandDocument(command); err != nil {
		return fmt.Errorf("%w: %w", postrestore.ErrInvalid, err)
	}
	return nil
}

// commandDocument decodes an extended JSON command, keeping the order of its keys.
func commandDocument(command json.RawMessage) (bson.D, error) {
	var doc bson.D
	if err := bson.UnmarshalExtJSON(command, false, &doc); err != nil {
		return nil, errors.New("not valid extended JSON")
	}
	return doc, nil
}

// commandReply holds the counts of a command's reply; the documents it may hold
// (findAndModify's value) are never decoded.
type commandReply struct {
	N               int64      `bson:"n"`
	NModified       int64      `bson:"nModified"`
	Upserted        []bson.Raw `bson:"upserted"`
	LastErrorObject *struct {
		N        int64         `bson:"n"`
		Upserted bson.RawValue `bson:"upserted"`
	} `bson:"lastErrorObject"`
	WriteErrors []struct {
		Code int `bson:"code"`
	} `bson:"writeErrors"`
	WriteConcernError *struct {
		Code     int    `bson:"code"`
		CodeName string `bson:"codeName"`
	} `bson:"writeConcernError"`
}

// RunPostRestoreCommand runs a post-restore command in database on the server at uri
// and returns its counts. The command is checked again first (postrestore.Parse)
// and admin, config and local are refused. Write errors fail the command with their
// codes only: their messages can quote document values. Dropping a collection or
// index that does not exist succeeds.
func (p *Prober) RunPostRestoreCommand(ctx context.Context, uri, database string, command json.RawMessage) (models.PostRestoreCounts, error) {
	var counts models.PostRestoreCounts
	parsed, err := postrestore.Parse(command)
	if err != nil {
		return counts, err
	}
	if database == "" || slices.Contains([]string{models.AdminDatabase, "config", "local"}, database) {
		return counts, fmt.Errorf("%w: database %q", postrestore.ErrNotClone, database)
	}
	doc, err := commandDocument(command)
	if err != nil {
		return counts, fmt.Errorf("%w: %w", postrestore.ErrInvalid, err)
	}
	err = withClient(ctx, uri, func(c *mongo.Client) error {
		var reply commandReply
		if runErr := c.Database(database).RunCommand(ctx, doc).Decode(&reply); runErr != nil {
			if notThere(parsed.Name, runErr) {
				return nil
			}
			return commandError(runErr)
		}
		if n := len(reply.WriteErrors); n > 0 {
			return fmt.Errorf("%w: %d write error(s), first code %d", ErrPostRestoreCommand, n, reply.WriteErrors[0].Code)
		}
		if wce := reply.WriteConcernError; wce != nil {
			return fmt.Errorf("%w: write concern error %d (%s)", ErrPostRestoreCommand, wce.Code, wce.CodeName)
		}
		counts = replyCounts(parsed.Name, &reply)
		return nil
	})
	return counts, err
}

// replyCounts returns the counts of a successful reply.
func replyCounts(name string, r *commandReply) models.PostRestoreCounts {
	switch name {
	case "delete":
		return models.PostRestoreCounts{N: r.N}
	case "update":
		return models.PostRestoreCounts{N: r.N, Modified: r.NModified, Upserted: int64(len(r.Upserted))}
	case "findAndModify":
		if r.LastErrorObject == nil {
			return models.PostRestoreCounts{}
		}
		out := models.PostRestoreCounts{N: r.LastErrorObject.N}
		if r.LastErrorObject.Upserted.Type != 0 {
			out.Upserted = 1
		}
		return out
	}
	return models.PostRestoreCounts{}
}

// notThere reports whether err is a drop of a collection, or an index, that does not
// exist.
func notThere(name string, err error) bool {
	var ce mongo.CommandError
	if !errors.As(err, &ce) {
		return false
	}
	switch name {
	case "drop":
		return ce.Code == namespaceNotFoundCode
	case "dropIndexes":
		return ce.Code == namespaceNotFoundCode || ce.Code == indexNotFoundCode
	}
	return false
}

// commandError renders a failed command: the server's code, name and a short message.
func commandError(err error) error {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		msg := ce.Message
		if len(msg) > maxCommandErrorLength {
			msg = msg[:maxCommandErrorLength] + "…"
		}
		return fmt.Errorf("%w: %s (code %d): %s", ErrPostRestoreCommand, ce.Name, ce.Code, msg)
	}
	return fmt.Errorf("%w: %w", ErrPostRestoreCommand, err)
}

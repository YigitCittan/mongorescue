package mongoconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

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

// CommandSession runs the post-restore commands of one restore over one client. It
// is not safe for concurrent use; Close it when done.
type CommandSession struct {
	client *mongo.Client
}

// OpenCommandSession connects to the server at uri for the post-restore commands of
// one restore. Errors never quote the connection string.
func (p *Prober) OpenCommandSession(ctx context.Context, uri string) (*CommandSession, error) {
	client, err := mongo.Connect(clientOptions(ctx, uri))
	if err != nil {
		return nil, errors.New("invalid connection string")
	}
	return &CommandSession{client: client}, nil
}

// Close disconnects the session's client.
func (s *CommandSession) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.client.Disconnect(ctx)
}

// RunPostRestoreCommand runs a post-restore command in database and returns its
// counts. The command is checked again first (postrestore.Parse) and admin, config
// and local are refused. Failures carry codes and code names only, never the
// server's message: it can quote document values (a duplicate key, a failed
// validation). Dropping a collection or index that does not exist succeeds.
func (s *CommandSession) RunPostRestoreCommand(ctx context.Context, database string, command json.RawMessage) (models.PostRestoreCounts, error) {
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
	var reply commandReply
	if runErr := s.client.Database(database).RunCommand(ctx, doc).Decode(&reply); runErr != nil {
		if notThere(parsed.Name, runErr) {
			return counts, nil
		}
		return counts, commandError(ctx, runErr)
	}
	if n := len(reply.WriteErrors); n > 0 {
		return counts, fmt.Errorf("%w: %d write error(s), first code %d", ErrPostRestoreCommand, n, reply.WriteErrors[0].Code)
	}
	if wce := reply.WriteConcernError; wce != nil {
		return counts, fmt.Errorf("%w: write concern error %d (%s)", ErrPostRestoreCommand, wce.Code, wce.CodeName)
	}
	return replyCounts(parsed.Name, &reply), nil
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

// commandError renders a failed command without any server message, which can quote
// document values (E11000 duplicate key {email: ...}): a server error by its code and
// code name, a timeout or cancellation of ctx as such, and anything else (network,
// server selection) as a driver error. The original error is not wrapped.
func commandError(ctx context.Context, err error) error {
	var ce mongo.CommandError
	var se mongo.ServerError
	switch {
	case errors.As(err, &ce):
		name := ce.Name
		if name == "" {
			name = "error"
		}
		return fmt.Errorf("%w: %s (code %d)", ErrPostRestoreCommand, name, ce.Code)
	case errors.As(err, &se):
		return fmt.Errorf("%w: server error", ErrPostRestoreCommand)
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: timed out", ErrPostRestoreCommand)
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: cancelled", ErrPostRestoreCommand)
	case mongo.IsNetworkError(err):
		return fmt.Errorf("%w: network error", ErrPostRestoreCommand)
	default:
		return fmt.Errorf("%w: driver error (the server could not be reached or selected)", ErrPostRestoreCommand)
	}
}

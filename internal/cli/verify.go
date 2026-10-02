package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/apiclient"
	"github.com/yigitcittan/mongorescue/internal/models"
)

const verifyUsage = `Usage: mongorescue verify BACKUP_ID [flags]

Re-reads the backup's archive on its storage target, in the background on the
server, and compares it with the checksum recorded at backup time (encrypted
archives are decrypted to their end). With --wait it polls until the result is in
and exits 1 on a mismatch or an error. It stops waiting after an hour (exit 6)
unless --timeout says otherwise (--timeout 0 waits forever). Needs an operator or
admin API key.
`

// defaultVerifyTimeout bounds "verify --wait" without --timeout: a verification the
// server never runs (refused in the background, or the server restarted) would
// otherwise be waited for forever.
var defaultVerifyTimeout = time.Hour

// runVerify implements "mongorescue verify".
func runVerify(ctx context.Context, s *session, args []string) error {
	fs := s.newFlagSet("verify", verifyUsage, true)
	pos, err := s.parse(fs, args)
	if err != nil {
		return err
	}
	id, err := onlyOneID(pos, "backup")
	if err != nil {
		return err
	}
	client, err := s.connect()
	if err != nil {
		return err
	}
	if s.opts.wait && !s.set["timeout"] {
		s.opts.timeout = defaultVerifyTimeout
	}
	res, err := client.VerifyBackup(ctx, id)
	if err != nil {
		return startError(err, "backups --id "+id+" --json")
	}
	if !s.opts.wait {
		switch {
		case s.opts.json:
			return writeJSON(s.stdout, res.Raw)
		case s.opts.quiet:
			_, err = fmt.Fprintln(s.stdout, id)
			return err
		}
		_, err = fmt.Fprintf(s.stdout, "Started the verification of backup %s.\nSee the result with: mongorescue list backups --id %s --json (verified_at, verification)\n", id, id)
		return err
	}
	before := res.Value.VerifiedAt
	s.infof("Verifying backup %s; waiting for the result.", id)
	final := res
	err = s.wait(ctx, waitTarget{kind: "verification of backup", id: id, hint: "mongorescue list backups --id " + id + " --json"}, func(ctx context.Context) (bool, string, error) {
		got, pollErr := client.GetBackup(ctx, id)
		if pollErr != nil {
			return false, "", pollErr
		}
		final = got
		return !sameTime(before, got.Value.VerifiedAt), "verifying", nil
	})
	if err != nil {
		return err
	}
	return s.printVerification(final)
}

// sameTime reports whether a and b are both nil or the same instant.
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// printVerification prints the outcome of a verification and fails unless it is ok.
func (s *session) printVerification(res *apiclient.Result[models.BackupRecord]) error {
	b := res.Value
	switch {
	case s.opts.json:
		if err := writeJSON(s.stdout, res.Raw); err != nil {
			return err
		}
	case s.opts.quiet:
		fmt.Fprintln(s.stdout, b.ID)
	case b.Verification == models.VerificationOK:
		fmt.Fprintf(s.stdout, "Backup %s verified at %s: the archive matches its checksum.\n", b.ID, fmtTimePtr(b.VerifiedAt))
	default:
		fmt.Fprintf(s.stdout, "Backup %s verification %s at %s%s.\n", b.ID, b.Verification, fmtTimePtr(b.VerifiedAt), suffix(b.VerificationError))
	}
	if b.Verification != models.VerificationOK {
		return s.reportedInText(failedErrorf("verification of backup %s: %s%s", b.ID, b.Verification, suffix(b.VerificationError)))
	}
	return nil
}

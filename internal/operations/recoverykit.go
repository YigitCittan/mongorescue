package operations

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auditlog"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// PlaintextKitGrantTTL is how long after its approval the requester may use an
// approved ApprovalPlaintextRecoveryKit request, once.
const PlaintextKitGrantTTL = time.Hour

// plaintextKitUsed starts the Result of a used ApprovalPlaintextRecoveryKit
// request.
const plaintextKitUsed = "used: a recovery kit with the plaintext secret.key was downloaded at "

// ErrPlaintextKitApproval is returned when the approval passed for a plaintext
// recovery kit download does not allow it. Its message is safe to show to clients.
var ErrPlaintextKitApproval = errors.New("operations: the approval does not allow a recovery kit with the plaintext secret key: " +
	"it must be your own approved plaintext_recovery_kit request, approved within the last hour and not used yet")

// AuthorizePlaintextRecoveryKit decides whether the caller (an administrator) may
// download a recovery kit carrying the plaintext secret.key of a KMS-wrapped
// installation. Without the two-person rule it allows it. With the rule, a call
// without approvalID stores an approval request (ApprovalPlaintextRecoveryKit) and
// returns *ApprovalPendingError; once another administrator approved it, the
// requester calls again with its ID, which is used up by this call (within
// PlaintextKitGrantTTL of the approval). The approver never gets the kit.
func (s *Service) AuthorizePlaintextRecoveryKit(ctx context.Context, approvalID string) error {
	if err := auth.RequireScope(ctx, auth.ScopeAdmin); err != nil {
		return err
	}
	if !s.settings().Security.RequireSecondApprover {
		return nil
	}
	p := auth.PrincipalFrom(ctx)
	if approvalID == "" {
		return s.requestApproval(ctx, &models.Approval{Action: models.ApprovalPlaintextRecoveryKit, Subject: p.UserID(),
			Summary: "download a recovery kit with the plaintext secret.key, which is otherwise only stored wrapped by a KMS"})
	}
	st, err := s.approvals()
	if err != nil {
		return err
	}
	now := s.now().UTC()
	_, err = st.UpdateApproval(ctx, approvalID, func(a *models.Approval) error {
		if a.Action != models.ApprovalPlaintextRecoveryKit || a.Status != models.ApprovalApproved ||
			a.RequestedByUserID == "" || a.RequestedByUserID != p.UserID() ||
			a.DecidedAt == nil || now.Sub(*a.DecidedAt) > PlaintextKitGrantTTL || strings.HasPrefix(a.Result, plaintextKitUsed) {
			return ErrPlaintextKitApproval
		}
		a.Result = plaintextKitUsed + now.Format(time.RFC3339)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return ErrPlaintextKitApproval
	}
	if err != nil {
		return err
	}
	auditlog.Annotate(ctx, "approval_id", approvalID)
	return nil
}

package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gtb.local/backend/internal/platform/value"
)

var (
	ErrVersionConflict   = errors.New("payment version does not match the expected version")
	ErrInvalidTransition = errors.New("payment transition is not permitted")
	ErrClaimConflict     = errors.New("payment is already claimed by a conflicting submission or cancellation")
	ErrMissingEvidence   = errors.New("payment transition requires evidence")
)

type LifecycleState string

const (
	Received       LifecycleState = "RECEIVED"
	Validating     LifecycleState = "VALIDATING"
	Scheduled      LifecycleState = "SCHEDULED"
	RequiresReview LifecycleState = "REQUIRES_REVIEW"
	Accepted       LifecycleState = "ACCEPTED"
	Executing      LifecycleState = "EXECUTING"
	Completed      LifecycleState = "COMPLETED"
	Rejected       LifecycleState = "REJECTED"
	Cancelled      LifecycleState = "CANCELLED"
	Returned       LifecycleState = "RETURNED"
)

// lifecycleTransitions is the complete business lifecycle graph. Guards in
// Transition add the claim, outcome and evidence preconditions.
var lifecycleTransitions = map[LifecycleState][]LifecycleState{
	Received:       {Validating, Rejected, Cancelled},
	Validating:     {Scheduled, RequiresReview, Accepted, Rejected, Cancelled},
	Scheduled:      {Validating, Rejected, Cancelled},
	RequiresReview: {Validating, Accepted, Rejected, Cancelled},
	Accepted:       {Validating, Executing, Rejected, Cancelled},
	Executing:      {Completed, Rejected},
	Completed:      {Returned},
}

// Cancellation can win only before execution claims submission.
func (state LifecycleState) cancellable() bool {
	switch state {
	case Received, Validating, Scheduled, RequiresReview, Accepted:
		return true
	}
	return false
}

func (state LifecycleState) Terminal() bool {
	return state == Rejected || state == Cancelled || state == Returned
}

type ExecutionOutcome string

const (
	NotSubmitted     ExecutionOutcome = "NOT_SUBMITTED"
	Pending          ExecutionOutcome = "PENDING"
	ConfirmedSuccess ExecutionOutcome = "CONFIRMED_SUCCESS"
	ConfirmedFailure ExecutionOutcome = "CONFIRMED_FAILURE"
	Unknown          ExecutionOutcome = "UNKNOWN"
)

// An unknown outcome resolves only through evidence of success or failure.
var outcomeTransitions = map[ExecutionOutcome][]ExecutionOutcome{
	NotSubmitted: {Pending},
	Pending:      {ConfirmedSuccess, ConfirmedFailure, Unknown},
	Unknown:      {ConfirmedSuccess, ConfirmedFailure},
}

type ActorType string

const (
	CustomerActor ActorType = "CUSTOMER"
	OperatorActor ActorType = "OPERATOR"
	SystemActor   ActorType = "SYSTEM"
)

// Actor records who caused a change. Subject is an opaque OIDC subject or a
// stable system component name; it conveys no authority by itself.
type Actor struct {
	Type    ActorType `json:"type"`
	Subject string    `json:"subject"`
}

func (actor Actor) validate() error {
	switch actor.Type {
	case CustomerActor, OperatorActor, SystemActor:
	default:
		return fmt.Errorf("%w: unknown actor type", ErrInvalidTransition)
	}
	if actor.Subject == "" || len(actor.Subject) > 256 {
		return fmt.Errorf("%w: actor subject must contain 1 to 256 bytes", ErrInvalidTransition)
	}
	return nil
}

type ClaimKind string

const (
	SubmissionClaim   ClaimKind = "SUBMISSION"
	CancellationClaim ClaimKind = "CANCELLATION"
)

// Claim is the single exclusive boundary between cancellation and submission.
// Whichever commits first at the current version wins; the other conflicts.
type Claim struct {
	Kind      ClaimKind `json:"kind"`
	ClaimID   value.ID  `json:"claimId"`
	ClaimedAt time.Time `json:"claimedAt"`
}

type EvidenceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Payment struct {
	ID                  value.ID
	CustomerID          value.ID
	Product             Product
	InstructionHash     string
	Lifecycle           LifecycleState
	Outcome             ExecutionOutcome
	Version             int64
	ExecutionWorkflowID string
	Claim               *Claim
	ReceivedAt          time.Time
	UpdatedAt           time.Time
}

type ChangeKind string

const (
	ChangeReceived          ChangeKind = "RECEIVED"
	ChangeTransition        ChangeKind = "TRANSITION"
	ChangeSubmissionClaim   ChangeKind = "SUBMISSION_CLAIMED"
	ChangeCancellationClaim ChangeKind = "CANCELLATION_CLAIMED"
	ChangeExecutionOwner    ChangeKind = "EXECUTION_OWNER_ASSIGNED"
)

// Change is the append-only record of one aggregate version (invariant 7):
// evidence, actor, policy version and resulting aggregate version.
type Change struct {
	PaymentID     value.ID
	Version       int64
	Kind          ChangeKind
	FromState     LifecycleState
	ToState       LifecycleState
	FromOutcome   ExecutionOutcome
	ToOutcome     ExecutionOutcome
	Actor         Actor
	PolicyVersion string
	Reason        string
	Evidence      []EvidenceRef
	CommandID     *value.ID
	OccurredAt    time.Time
}

const ReceiptPolicyVersion = "payment-receipt/v1"

// PaymentWorkflowID is the stable parent workflow identity started by dispatch.
func PaymentWorkflowID(paymentID value.ID) string {
	return "payment/" + string(paymentID)
}

// ExecutionWorkflowID is the stable identity of the execution child that owns
// financial effects for the payment's product.
func ExecutionWorkflowID(paymentID value.ID, product Product) string {
	return "execution/" + string(paymentID) + "/" + strings.ToLower(strings.ReplaceAll(string(product), "_", "-"))
}

// NewPayment creates version 1 in RECEIVED: durably received, not accepted.
func NewPayment(id value.ID, instruction Instruction, actor Actor, commandID value.ID, at time.Time) (Payment, Change, error) {
	if err := id.Validate(); err != nil {
		return Payment{}, Change{}, fmt.Errorf("%w: payment ID", ErrInvalidInstruction)
	}
	if err := commandID.Validate(); err != nil {
		return Payment{}, Change{}, fmt.Errorf("%w: command ID", ErrInvalidInstruction)
	}
	hash, err := instruction.Hash()
	if err != nil {
		return Payment{}, Change{}, err
	}
	if err := actor.validate(); err != nil {
		return Payment{}, Change{}, err
	}
	at = at.UTC()
	payment := Payment{
		ID: id, CustomerID: instruction.CustomerID, Product: instruction.Product,
		InstructionHash: hash, Lifecycle: Received, Outcome: NotSubmitted, Version: 1,
		ReceivedAt: at, UpdatedAt: at,
	}
	return payment, Change{
		PaymentID: id, Version: 1, Kind: ChangeReceived, ToState: Received,
		FromOutcome: NotSubmitted, ToOutcome: NotSubmitted, Actor: actor,
		PolicyVersion: ReceiptPolicyVersion, Reason: "instruction durably received",
		Evidence: []EvidenceRef{{Type: "command", ID: string(commandID)}}, CommandID: &commandID,
		OccurredAt: at,
	}, nil
}

type TransitionRequest struct {
	ExpectedVersion int64
	To              LifecycleState
	// Outcome is optional; empty keeps the current execution outcome.
	Outcome       ExecutionOutcome
	Actor         Actor
	PolicyVersion string
	Reason        string
	Evidence      []EvidenceRef
	CommandID     *value.ID
	At            time.Time
}

// Transition applies one versioned lifecycle and/or execution-outcome change.
func (payment Payment) Transition(request TransitionRequest) (Payment, Change, error) {
	if err := payment.checkChange(request.ExpectedVersion, request.Actor, request.PolicyVersion, request.Reason); err != nil {
		return Payment{}, Change{}, err
	}
	to, outcome := request.To, request.Outcome
	if to == "" {
		to = payment.Lifecycle
	}
	if outcome == "" {
		outcome = payment.Outcome
	}
	if to == payment.Lifecycle && outcome == payment.Outcome {
		return Payment{}, Change{}, fmt.Errorf("%w: transition changes nothing", ErrInvalidTransition)
	}
	if to != payment.Lifecycle && !contains(lifecycleTransitions[payment.Lifecycle], to) {
		return Payment{}, Change{}, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, payment.Lifecycle, to)
	}
	if outcome != payment.Outcome && !contains(outcomeTransitions[payment.Outcome], outcome) {
		return Payment{}, Change{}, fmt.Errorf("%w: outcome %s to %s", ErrInvalidTransition, payment.Outcome, outcome)
	}
	if err := guardTransition(payment, to, outcome, request.Evidence); err != nil {
		return Payment{}, Change{}, err
	}
	next := payment
	next.Lifecycle, next.Outcome = to, outcome
	return payment.advance(next, Change{
		Kind: ChangeTransition, Actor: request.Actor, PolicyVersion: request.PolicyVersion,
		Reason: request.Reason, Evidence: request.Evidence, CommandID: request.CommandID,
	}, request.At)
}

func guardTransition(payment Payment, to LifecycleState, outcome ExecutionOutcome, evidence []EvidenceRef) error {
	claimed := func(kind ClaimKind) bool { return payment.Claim != nil && payment.Claim.Kind == kind }
	switch {
	case claimed(CancellationClaim) && to != Cancelled:
		return fmt.Errorf("%w: a cancellation claim permits only cancellation", ErrInvalidTransition)
	case claimed(SubmissionClaim) && to != payment.Lifecycle && to != Executing && to != Completed && to != Rejected && to != Returned:
		return fmt.Errorf("%w: a submission claim cannot return to pre-execution states", ErrInvalidTransition)
	case to == Rejected && (outcome == Pending || outcome == Unknown || outcome == ConfirmedSuccess):
		// A timeout or unknown outcome is never rejection evidence.
		return fmt.Errorf("%w: rejection after submission requires confirmed failure", ErrInvalidTransition)
	case to == Cancelled && !claimed(CancellationClaim):
		return fmt.Errorf("%w: cancellation requires a cancellation claim", ErrInvalidTransition)
	case to == Executing && !claimed(SubmissionClaim):
		return fmt.Errorf("%w: execution requires the submission claim", ErrInvalidTransition)
	case outcome != NotSubmitted && !claimed(SubmissionClaim):
		return fmt.Errorf("%w: execution outcome requires the submission claim", ErrInvalidTransition)
	case to == Completed && outcome != ConfirmedSuccess:
		return fmt.Errorf("%w: completion requires confirmed execution success", ErrInvalidTransition)
	}
	needsEvidence := to == Completed || to == Returned || to == Rejected ||
		(outcome != payment.Outcome && (outcome == ConfirmedSuccess || outcome == ConfirmedFailure || outcome == Unknown))
	if needsEvidence && len(evidence) == 0 {
		return ErrMissingEvidence
	}
	for _, reference := range evidence {
		if reference.Type == "" || reference.ID == "" || len(reference.Type) > 64 || len(reference.ID) > 256 {
			return fmt.Errorf("%w: evidence references need a type and ID", ErrInvalidTransition)
		}
	}
	return nil
}

// ClaimSubmission records the atomic execution claim. Repeating the same claim
// ID is an idempotent replay and returns a nil change.
func (payment Payment) ClaimSubmission(expectedVersion int64, claimID value.ID, actor Actor, at time.Time) (Payment, *Change, error) {
	if payment.Claim != nil {
		return payment.replayClaim(SubmissionClaim, claimID)
	}
	if payment.Lifecycle != Accepted {
		return Payment{}, nil, fmt.Errorf("%w: submission can be claimed only when ACCEPTED", ErrInvalidTransition)
	}
	return payment.claim(SubmissionClaim, ChangeSubmissionClaim, expectedVersion, claimID, actor, at)
}

// ClaimCancellation wins only before a submission claim. Repeating the same
// cancellation command is an idempotent replay and returns a nil change.
func (payment Payment) ClaimCancellation(expectedVersion int64, commandID value.ID, actor Actor, at time.Time) (Payment, *Change, error) {
	if payment.Claim != nil {
		return payment.replayClaim(CancellationClaim, commandID)
	}
	if !payment.Lifecycle.cancellable() {
		return Payment{}, nil, fmt.Errorf("%w: %s payment cannot be cancelled", ErrInvalidTransition, payment.Lifecycle)
	}
	return payment.claim(CancellationClaim, ChangeCancellationClaim, expectedVersion, commandID, actor, at)
}

func (payment Payment) replayClaim(kind ClaimKind, claimID value.ID) (Payment, *Change, error) {
	if payment.Claim.Kind == kind && payment.Claim.ClaimID == claimID {
		return payment, nil, nil
	}
	return Payment{}, nil, ErrClaimConflict
}

func (payment Payment) claim(kind ClaimKind, changeKind ChangeKind, expectedVersion int64, claimID value.ID, actor Actor, at time.Time) (Payment, *Change, error) {
	if err := claimID.Validate(); err != nil {
		return Payment{}, nil, fmt.Errorf("%w: claim ID must be a canonical UUIDv4", ErrInvalidTransition)
	}
	if err := payment.checkChange(expectedVersion, actor, ReceiptPolicyVersion, string(changeKind)); err != nil {
		return Payment{}, nil, err
	}
	next := payment
	next.Claim = &Claim{Kind: kind, ClaimID: claimID, ClaimedAt: at.UTC()}
	id := claimID
	updated, change, err := payment.advance(next, Change{
		Kind: changeKind, Actor: actor, PolicyVersion: "payment-claims/v1",
		Reason:   strings.ToLower(string(kind)) + " claimed",
		Evidence: []EvidenceRef{{Type: "claim", ID: string(claimID)}}, CommandID: &id,
	}, at)
	return updated, &change, err
}

// AssignExecutionOwner persists the one execution child permitted to perform
// financial effects. Reassigning the same owner is a replay (nil change).
func (payment Payment) AssignExecutionOwner(expectedVersion int64, workflowID string, actor Actor, at time.Time) (Payment, *Change, error) {
	if workflowID != ExecutionWorkflowID(payment.ID, payment.Product) {
		return Payment{}, nil, fmt.Errorf("%w: execution owner must be the stable execution workflow ID", ErrInvalidTransition)
	}
	if payment.ExecutionWorkflowID == workflowID {
		return payment, nil, nil
	}
	if payment.Lifecycle.Terminal() {
		return Payment{}, nil, fmt.Errorf("%w: closed payment cannot gain an execution owner", ErrInvalidTransition)
	}
	if err := payment.checkChange(expectedVersion, actor, ReceiptPolicyVersion, "execution owner assigned"); err != nil {
		return Payment{}, nil, err
	}
	next := payment
	next.ExecutionWorkflowID = workflowID
	updated, change, err := payment.advance(next, Change{
		Kind: ChangeExecutionOwner, Actor: actor, PolicyVersion: "payment-execution-owner/v1",
		Reason: "execution owner assigned", Evidence: []EvidenceRef{{Type: "workflow", ID: workflowID}},
	}, at)
	return updated, &change, err
}

func (payment Payment) checkChange(expectedVersion int64, actor Actor, policyVersion, reason string) error {
	if expectedVersion != payment.Version {
		return ErrVersionConflict
	}
	if err := actor.validate(); err != nil {
		return err
	}
	if policyVersion == "" || len(policyVersion) > 64 || reason == "" || len(reason) > 256 {
		return fmt.Errorf("%w: policy version and reason are required", ErrInvalidTransition)
	}
	return nil
}

func (payment Payment) advance(next Payment, change Change, at time.Time) (Payment, Change, error) {
	at = at.UTC()
	if at.IsZero() {
		return Payment{}, Change{}, fmt.Errorf("%w: change time is required", ErrInvalidTransition)
	}
	next.Version = payment.Version + 1
	next.UpdatedAt = at
	change.PaymentID = payment.ID
	change.Version = next.Version
	change.FromState, change.ToState = payment.Lifecycle, next.Lifecycle
	change.FromOutcome, change.ToOutcome = payment.Outcome, next.Outcome
	change.OccurredAt = at
	return next, change, nil
}

func contains[T comparable](values []T, wanted T) bool {
	for _, candidate := range values {
		if candidate == wanted {
			return true
		}
	}
	return false
}

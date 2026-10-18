# Rosetta 03: the payment state machine

**Payments idea.** A payment has two state dimensions:
- the business **lifecycle** (`RECEIVED` … `COMPLETED`, `REJECTED`,
  `CANCELLED`, `RETURNED`);
- the **execution outcome** reported by the rail (`NOT_SUBMITTED`, `PENDING`,
  `CONFIRMED_SUCCESS`, `CONFIRMED_FAILURE`, `UNKNOWN`).

Every change is versioned and records actor, policy, reason and evidence.
Guards encode hard-won rules:
- you cannot reject a payment whose outcome is unknown, because the money may
  have moved;
- completion needs confirmed success plus evidence;
- a single **claim** decides the race between cancellation and submission.

Files: `backend/internal/payment/domain/payment.go` and its test
`domain_test.go`. This package is pure: no database, no HTTP, no clock (the
caller passes `At`).

## Go

The graph is data:

From `backend/internal/payment/domain/payment.go:36`:

```go
var lifecycleTransitions = map[LifecycleState][]LifecycleState{
	Received:       {Validating, Rejected, Cancelled},
	Validating:     {Scheduled, RequiresReview, Accepted, Rejected, Cancelled},
	Scheduled:      {Validating, Rejected, Cancelled},
	RequiresReview: {Validating, Accepted, Rejected, Cancelled},
	Accepted:       {Validating, Executing, Rejected, Cancelled},
	Executing:      {Completed, Rejected},
	Completed:      {Returned},
}
```

A transition returns a *new* payment and a `Change` record; it never mutates:

From `backend/internal/payment/domain/payment.go:222`:

```go
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
	// …
	if to != payment.Lifecycle && !contains(lifecycleTransitions[payment.Lifecycle], to) {
		return Payment{}, Change{}, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, payment.Lifecycle, to)
	}
	// …
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
```

The business guards:

From `backend/internal/payment/domain/payment.go:253`:

```go
func guardTransition(payment Payment, to LifecycleState, outcome ExecutionOutcome, evidence []EvidenceRef) error {
	claimed := func(kind ClaimKind) bool { return payment.Claim != nil && payment.Claim.Kind == kind }
	switch {
	case claimed(CancellationClaim) && to != Cancelled:
		return fmt.Errorf("%w: a cancellation claim permits only cancellation", ErrInvalidTransition)
	// …
	case to == Rejected && (outcome == Pending || outcome == Unknown || outcome == ConfirmedSuccess):
		// A timeout or unknown outcome is never rejection evidence.
		return fmt.Errorf("%w: rejection after submission requires confirmed failure", ErrInvalidTransition)
	// …
	case to == Completed && outcome != ConfirmedSuccess:
		return fmt.Errorf("%w: completion requires confirmed execution success", ErrInvalidTransition)
	}
	needsEvidence := to == Completed || to == Returned || to == Rejected ||
		(outcome != payment.Outcome && (outcome == ConfirmedSuccess || outcome == ConfirmedFailure || outcome == Unknown))
	if needsEvidence && len(evidence) == 0 {
		return ErrMissingEvidence
	}
```

Claims resolve the cancel/submit race, and replaying the winning claim is a
no-op:

From `backend/internal/payment/domain/payment.go:287`:

```go
func (payment Payment) ClaimSubmission(expectedVersion int64, claimID value.ID, actor Actor, at time.Time) (Payment, *Change, error) {
	if payment.Claim != nil {
		return payment.replayClaim(SubmissionClaim, claimID)
	}
	if payment.Lifecycle != Accepted {
		return Payment{}, nil, fmt.Errorf("%w: submission can be claimed only when ACCEPTED", ErrInvalidTransition)
	}
	return payment.claim(SubmissionClaim, ChangeSubmissionClaim, expectedVersion, claimID, actor, at)
}
```

## Java 21

```java
enum LifecycleState { RECEIVED, VALIDATING, SCHEDULED, REQUIRES_REVIEW, ACCEPTED, EXECUTING,
                      COMPLETED, REJECTED, CANCELLED, RETURNED }

record Payment(Id id, Id customerId, Product product, String instructionHash, LifecycleState lifecycle,
               ExecutionOutcome outcome, long version, String executionWorkflowId,
               Optional<Claim> claim, Instant receivedAt, Instant updatedAt) {

    private static final Map<LifecycleState, Set<LifecycleState>> LIFECYCLE = Map.of(
        RECEIVED, EnumSet.of(VALIDATING, REJECTED, CANCELLED),
        VALIDATING, EnumSet.of(SCHEDULED, REQUIRES_REVIEW, ACCEPTED, REJECTED, CANCELLED),
        /* … */
        COMPLETED, EnumSet.of(RETURNED));

    /** Returns the next state and its history record; throws on an illegal change. */
    Transitioned transition(TransitionRequest req) {
        checkChange(req.expectedVersion(), req.actor(), req.policyVersion(), req.reason());
        var to = req.to() != null ? req.to() : lifecycle;
        var out = req.outcome() != null ? req.outcome() : outcome;
        if (to != lifecycle && !LIFECYCLE.getOrDefault(lifecycle, Set.of()).contains(to))
            throw new InvalidTransitionException(lifecycle + " to " + to);
        guard(to, out, req.evidence());
        var next = new Payment(id, customerId, product, instructionHash, to, out, version + 1,
                               executionWorkflowId, claim, receivedAt, req.at());
        return new Transitioned(next, Change.of(this, next, req));
    }

    private void guard(LifecycleState to, ExecutionOutcome out, List<EvidenceRef> evidence) {
        boolean cancelClaim = claim.map(c -> c.kind() == CANCELLATION).orElse(false);
        if (cancelClaim && to != CANCELLED) throw new InvalidTransitionException("…");
        if (to == REJECTED && EnumSet.of(PENDING, UNKNOWN, CONFIRMED_SUCCESS).contains(out))
            throw new InvalidTransitionException("rejection after submission requires confirmed failure");
        /* … */
    }
}
record Transitioned(Payment payment, Change change) {}
```

## Why the Go looks like this

- **Multiple return values replace a result wrapper.** Go returns
  `(Payment, Change, error)` directly. Java needs a `Transitioned` record, plus
  an exception for the error path.
- **Value receiver = immutability by default.** `func (payment Payment)`
  receives a copy. `next := payment` makes another copy, which is then edited,
  so the caller's payment cannot change. A Java record needs the full
  constructor call instead.
- **`fmt.Errorf("%w: …")` keeps both a category and a message.** The category
  (`ErrInvalidTransition`) lets callers decide what to do (`errors.Is`). The
  message (`ACCEPTED to RECEIVED`) helps whoever reads the log. Java achieves
  this with an exception subclass plus a message.
- **Returning `(Payment, *Change, error)` from the claim methods** uses a nil
  `*Change` to mean "replay, nothing to persist". A nil pointer as a
  meaningful third outcome is common in Go.
- **Maps with slice values as data.** `lifecycleTransitions` is a package-level
  `var`. Go has no immutable maps, so it is protected by being unexported: only
  this package can touch it.
- **The generic `contains[T comparable]`** stands in for
  `Set.contains`, because the transitions are small slices, not sets.

## Try it

```sh
cd backend && go test -v -run 'Unknown|Claims|HappyPath' ./internal/payment/domain/
```

Read `TestUnknownOutcomeCannotBeRejectedOrCompletedBlindly` in
`domain_test.go`: it is the "never refund on a timeout" rule written as a test.

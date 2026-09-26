package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gtb.local/backend/internal/platform/value"
)

var (
	customerID = value.ID("20000000-0000-4000-8000-000000000001")
	mayaID     = value.ID("10000000-0000-4000-8000-000000000001")
	danielID   = value.ID("10000000-0000-4000-8000-000000000002")
	paymentID  = value.ID("70000000-0000-4000-8000-000000000001")
	commandID  = value.ID("71000000-0000-4000-8000-000000000001")
	system     = Actor{Type: SystemActor, Subject: "payment-worker"}
	now        = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
)

func bookTransfer() Instruction {
	creditor := danielID
	return Instruction{
		CustomerID: customerID, Product: BookTransfer, DebtorAccountID: mayaID,
		Amount:   value.Money{AmountMinor: 10000, Currency: value.SGD},
		Creditor: Creditor{Name: "Daniel", AccountID: &creditor}, Reference: "rent",
	}
}

func external(product Product, currency value.Currency, bank, country string) Instruction {
	instruction := bookTransfer()
	instruction.Product = product
	instruction.Amount.Currency = currency
	instruction.Creditor = Creditor{Name: "Daniel", AccountNumber: "0123-456", BankIdentifier: bank, Country: country}
	return instruction
}

func TestInstructionValidation(t *testing.T) {
	valid := []Instruction{
		bookTransfer(),
		external(DomesticInstant, value.SGD, "OCBCSGSG", "SG"),
		external(RTGS, value.SGD, "7339", "SG"),
		external(CrossBorder, value.USD, "CHASUS33XXX", "US"),
	}
	for _, instruction := range valid {
		if err := instruction.Validate(); err != nil {
			t.Fatalf("%s rejected: %v", instruction.Product, err)
		}
	}
	mutate := func(change func(*Instruction)) Instruction {
		instruction := bookTransfer()
		change(&instruction)
		return instruction
	}
	same := mayaID
	invalid := map[string]Instruction{
		"zero amount":        mutate(func(i *Instruction) { i.Amount.AmountMinor = 0 }),
		"negative amount":    mutate(func(i *Instruction) { i.Amount.AmountMinor = -1 }),
		"unsupported ccy":    mutate(func(i *Instruction) { i.Amount.Currency = "JPY" }),
		"unknown product":    mutate(func(i *Instruction) { i.Product = "WIRE" }),
		"bad customer":       mutate(func(i *Instruction) { i.CustomerID = "cust-demo-001" }),
		"self transfer":      mutate(func(i *Instruction) { i.Creditor.AccountID = &same }),
		"missing creditor":   mutate(func(i *Instruction) { i.Creditor.AccountID = nil }),
		"book with bank":     mutate(func(i *Instruction) { i.Creditor.BankIdentifier = "OCBCSGSG" }),
		"blank name":         mutate(func(i *Instruction) { i.Creditor.Name = "" }),
		"padded name":        mutate(func(i *Instruction) { i.Creditor.Name = " Daniel" }),
		"control reference":  mutate(func(i *Instruction) { i.Reference = "a\nb" }),
		"long reference":     mutate(func(i *Instruction) { i.Reference = strings.Repeat("x", 141) }),
		"domestic USD":       external(DomesticInstant, value.USD, "OCBCSGSG", "SG"),
		"domestic abroad":    external(RTGS, value.SGD, "OCBCSGSG", "MY"),
		"cross-border to SG": external(CrossBorder, value.USD, "CHASUS33", "SG"),
		"cross-border BIC":   external(CrossBorder, value.USD, "chase", "US"),
		"external with GTB account": func() Instruction {
			instruction := external(DomesticInstant, value.SGD, "OCBCSGSG", "SG")
			instruction.Creditor.AccountID = &same
			return instruction
		}(),
	}
	for name, instruction := range invalid {
		if err := instruction.Validate(); !errors.Is(err, ErrInvalidInstruction) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestInstructionHashIsCanonical(t *testing.T) {
	first, err := bookTransfer().Hash()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := bookTransfer().Hash()
	changed := bookTransfer()
	changed.Amount.AmountMinor++
	third, _ := changed.Hash()
	if first != second || first == third || len(first) != 64 {
		t.Fatalf("hash not canonical: %s %s %s", first, second, third)
	}
}

func TestIdempotencyScope(t *testing.T) {
	if err := (IdempotencyScope{CustomerAPI, customerID, "order-1:retry.A_b"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []IdempotencyScope{
		{"BATCH", customerID, "k"}, {CustomerAPI, "cust", "k"}, {CustomerAPI, customerID, ""},
		{CustomerAPI, customerID, "has space"}, {CustomerAPI, customerID, strings.Repeat("k", 129)},
	} {
		if err := scope.Validate(); err == nil {
			t.Errorf("accepted %+v", scope)
		}
	}
}

func received(t *testing.T) Payment {
	t.Helper()
	payment, change, err := NewPayment(paymentID, bookTransfer(), Actor{CustomerActor, "subject-1"}, commandID, now)
	if err != nil {
		t.Fatal(err)
	}
	if payment.Version != 1 || payment.Lifecycle != Received || payment.Outcome != NotSubmitted ||
		change.Version != 1 || change.Kind != ChangeReceived || change.PolicyVersion == "" || len(change.Evidence) != 1 {
		t.Fatalf("unexpected receipt: %+v %+v", payment, change)
	}
	return payment
}

func step(t *testing.T, payment Payment, to LifecycleState, outcome ExecutionOutcome, evidence ...EvidenceRef) Payment {
	t.Helper()
	next, change, err := payment.Transition(TransitionRequest{
		ExpectedVersion: payment.Version, To: to, Outcome: outcome, Actor: system,
		PolicyVersion: "test/v1", Reason: "test", Evidence: evidence, At: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("%s/%s -> %s/%s: %v", payment.Lifecycle, payment.Outcome, to, outcome, err)
	}
	if next.Version != payment.Version+1 || change.Version != next.Version || change.FromState != payment.Lifecycle {
		t.Fatalf("version not advanced: %+v %+v", next, change)
	}
	return next
}

func accepted(t *testing.T) Payment {
	return step(t, step(t, received(t), Validating, ""), Accepted, "")
}

func TestHappyPathRequiresClaimAndEvidence(t *testing.T) {
	payment := accepted(t)
	if _, _, err := payment.Transition(TransitionRequest{ExpectedVersion: payment.Version, To: Executing, Actor: system, PolicyVersion: "v", Reason: "r", At: now}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("executing without claim: %v", err)
	}
	claimID := value.ID("72000000-0000-4000-8000-000000000001")
	claimed, change, err := payment.ClaimSubmission(payment.Version, claimID, system, now)
	if err != nil || change == nil || claimed.Claim.Kind != SubmissionClaim || claimed.Version != payment.Version+1 {
		t.Fatalf("claim: %+v %+v %v", claimed, change, err)
	}
	executing := step(t, claimed, Executing, Pending)
	evidence := EvidenceRef{Type: "core-banking-posting", ID: "posting-1"}
	if _, _, err := executing.Transition(TransitionRequest{ExpectedVersion: executing.Version, To: Completed, Outcome: ConfirmedSuccess, Actor: system, PolicyVersion: "v", Reason: "r", At: now}); !errors.Is(err, ErrMissingEvidence) {
		t.Fatalf("completion without evidence: %v", err)
	}
	completed := step(t, executing, Completed, ConfirmedSuccess, evidence)
	returned := step(t, completed, Returned, "", EvidenceRef{Type: "return", ID: "r-1"})
	if !returned.Lifecycle.Terminal() {
		t.Fatal("returned must be terminal")
	}
}

func TestUnknownOutcomeCannotBeRejectedOrCompletedBlindly(t *testing.T) {
	payment := accepted(t)
	payment, _, _ = payment.ClaimSubmission(payment.Version, "72000000-0000-4000-8000-000000000001", system, now)
	payment = step(t, payment, Executing, Pending)
	payment = step(t, payment, "", Unknown, EvidenceRef{"timeout", "attempt-1"})
	for _, to := range []LifecycleState{Rejected, Completed} {
		if _, _, err := payment.Transition(TransitionRequest{ExpectedVersion: payment.Version, To: to, Actor: system, PolicyVersion: "v", Reason: "r", Evidence: []EvidenceRef{{"x", "y"}}, At: now}); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s from UNKNOWN: %v", to, err)
		}
	}
	if _, _, err := payment.Transition(TransitionRequest{ExpectedVersion: payment.Version, Outcome: NotSubmitted, Actor: system, PolicyVersion: "v", Reason: "r", At: now}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("outcome regressed: %v", err)
	}
	rejected := step(t, payment, Rejected, ConfirmedFailure, EvidenceRef{"rail-reject", "rj-1"})
	if rejected.Outcome != ConfirmedFailure {
		t.Fatal("rejection outcome not recorded")
	}
}

func TestVersionConflictAndInvalidEdges(t *testing.T) {
	payment := received(t)
	if _, _, err := payment.Transition(TransitionRequest{ExpectedVersion: 7, To: Validating, Actor: system, PolicyVersion: "v", Reason: "r", At: now}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	for _, to := range []LifecycleState{Accepted, Executing, Completed, Returned, Cancelled} {
		if _, _, err := payment.Transition(TransitionRequest{ExpectedVersion: 1, To: to, Actor: system, PolicyVersion: "v", Reason: "r", Evidence: []EvidenceRef{{"x", "y"}}, At: now}); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("RECEIVED -> %s: %v", to, err)
		}
	}
	if _, _, err := payment.Transition(TransitionRequest{ExpectedVersion: 1, To: Validating, Actor: system, Reason: "r", At: now}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("missing policy version: %v", err)
	}
	if _, _, err := payment.Transition(TransitionRequest{ExpectedVersion: 1, To: Validating, Actor: Actor{}, PolicyVersion: "v", Reason: "r", At: now}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("missing actor: %v", err)
	}
}

func TestCancellationAndSubmissionClaimsAreExclusive(t *testing.T) {
	payment := accepted(t)
	cancelID := value.ID("73000000-0000-4000-8000-000000000001")
	submitID := value.ID("72000000-0000-4000-8000-000000000001")

	cancelled, change, err := payment.ClaimCancellation(payment.Version, cancelID, Actor{CustomerActor, "subject-1"}, now)
	if err != nil || change == nil {
		t.Fatal(err)
	}
	if _, _, err := cancelled.ClaimSubmission(cancelled.Version, submitID, system, now); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("submission after cancellation: %v", err)
	}
	replayed, change, err := cancelled.ClaimCancellation(payment.Version, cancelID, Actor{CustomerActor, "subject-1"}, now)
	if err != nil || change != nil || replayed.Version != cancelled.Version {
		t.Fatalf("cancellation replay: %+v %v", change, err)
	}
	if _, _, err := cancelled.Transition(TransitionRequest{ExpectedVersion: cancelled.Version, To: Validating, Actor: system, PolicyVersion: "v", Reason: "r", At: now}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel-claimed payment revalidated: %v", err)
	}
	step(t, cancelled, Cancelled, "")

	submitted, _, err := payment.ClaimSubmission(payment.Version, submitID, system, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := submitted.ClaimCancellation(submitted.Version, cancelID, Actor{CustomerActor, "subject-1"}, now); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("cancellation after submission: %v", err)
	}
	if _, _, err := submitted.Transition(TransitionRequest{ExpectedVersion: submitted.Version, To: Validating, Actor: system, PolicyVersion: "v", Reason: "r", At: now}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("claimed payment returned to validation: %v", err)
	}
	if _, _, err := submitted.ClaimSubmission(submitted.Version, "72000000-0000-4000-8000-000000000002", system, now); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("second submission claim: %v", err)
	}
	// Both racers read the same version: the loser also fails its version check.
	if _, _, err := payment.ClaimSubmission(payment.Version-1, submitID, system, now); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale submission claim: %v", err)
	}
	executing := step(t, submitted, Executing, "")
	if _, _, err := executing.ClaimCancellation(executing.Version, "73000000-0000-4000-8000-000000000002", system, now); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("cancellation while executing: %v", err)
	}
	if _, _, err := received(t).ClaimSubmission(1, submitID, system, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("submission claim before acceptance: %v", err)
	}
}

func TestExecutionOwnerAssignment(t *testing.T) {
	payment := received(t)
	owner := ExecutionWorkflowID(paymentID, BookTransfer)
	if owner != "execution/"+string(paymentID)+"/book-transfer" || PaymentWorkflowID(paymentID) != "payment/"+string(paymentID) {
		t.Fatalf("unexpected workflow IDs: %s", owner)
	}
	if _, _, err := payment.AssignExecutionOwner(1, "execution/other", system, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("arbitrary owner accepted: %v", err)
	}
	assigned, change, err := payment.AssignExecutionOwner(1, owner, system, now)
	if err != nil || change == nil || assigned.ExecutionWorkflowID != owner || assigned.Version != 2 {
		t.Fatalf("assign: %+v %v", assigned, err)
	}
	replayed, change, err := assigned.AssignExecutionOwner(1, owner, system, now)
	if err != nil || change != nil || replayed.Version != 2 {
		t.Fatalf("replay: %+v %v", change, err)
	}
}

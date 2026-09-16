# Chapter 15 — Operating a Payment Platform

## Business question

What does it take to run payment software safely when customers and other banks depend on it continuously?

Correct code is necessary but not sufficient. A payment platform is also an operational system with financial, customer, and sometimes systemic consequences when it degrades.

## Scenario: GTB's dependency slows down

At 19:30, an external payment rail begins responding slowly.

GTB's request queue grows. Customers retry. The fraud service receives more traffic. Worker threads become saturated. Timeouts increase. Operations see a spike in “failed” payments, but some external outcomes are actually unknown.

The incident is not just about latency. Poor operational behavior can create duplicate payments, incorrect customer messages, reconciliation breaks, and liquidity pressure.

## Availability has a business meaning

A payment service can be technically “up” while unable to complete useful payments.

Meaningful availability measures might include:

- percentage of eligible payments accepted;
- time to reach a defined business outcome;
- number of payments stuck beyond expected duration;
- time to reconcile ambiguous outcomes;
- ability to continue safely when a dependency degrades.

Infrastructure metrics matter, but business-level indicators tell the bank whether the payment capability is functioning.

## Back-pressure and overload

When downstream capacity is lower than incoming demand, the platform must slow, queue, reject, or reroute work deliberately.

Unbounded retries are dangerous. They can turn one failing dependency into a platform-wide outage.

Useful controls include bounded queues, circuit breakers, rate limits, retry budgets, and priority classes. The correct combination depends on the payment's urgency and business criticality.

## Observability should follow the payment

Operators need to trace one payment across services and external boundaries using stable identifiers.

A useful operational view answers:

- where is the payment now?
- what was the last confirmed business fact?
- what is it waiting for?
- what retries occurred?
- which accounting entries exist?
- what external references are known?
- does reconciliation show a break?

This is more valuable than a collection of disconnected service logs.

## Auditability

Payments require a durable history of important actions and decisions.

Audit data may need to record:

- who initiated or approved an action;
- which rule or decision produced a result;
- what message was sent or received;
- which state changed;
- which manual repair was performed;
- which financial entries resulted.

Audit records should be protected from casual modification and retained according to applicable policy and regulation.

## Resilience and recovery

Business continuity asks a harder question than “can Kubernetes restart the pod?”

GTB needs to know how to recover payment processing after loss of an availability zone, region, database, message broker, or external connection.

Recovery design should consider:

- durable payment state;
- ledger integrity;
- duplicate prevention after replay;
- re-establishing external sessions;
- reconciliation after recovery;
- operator procedures;
- recovery point and recovery time objectives.

The safest disaster-recovery test is one that includes financial reconciliation, not only service startup.

## Security

Payment systems are attractive targets because they can authorize movement of value and contain sensitive financial data.

Architecture should apply least privilege, strong service identity, encryption, secret management, segregation of duties, and controlled operator actions.

A production operator who can repair payment state should not automatically have unrestricted ability to create financial postings.

## Architecture implication

Operational design is part of the product architecture.

A mature payment platform includes:

- business and technical SLOs;
- end-to-end correlation;
- controlled retries;
- back-pressure;
- repair workflows;
- audit trails;
- disaster-recovery procedures;
- security controls;
- reconciliation after incidents.

## Key takeaway

Payment reliability is not only about keeping services online. It is about keeping financial processing **safe, explainable, and recoverable** while failures occur.

> **Operate the payment as a business process, not merely as a collection of microservices.**

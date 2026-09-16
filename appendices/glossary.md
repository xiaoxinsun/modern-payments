# Working Glossary

This glossary is intentionally short. It defines how terms are used in this draft, not every variation used by every payment scheme.

**Acceptance** — The point at which a bank or scheme takes responsibility for processing an instruction under its rules. Acceptance does not necessarily mean settlement or beneficiary credit.

**Available balance** — The amount currently available for use after considering booked balance plus relevant holds, reservations, overdraft rules, and other product logic.

**Beneficiary / payee / creditor** — The party intended to receive value. Terminology varies by scheme and standard.

**Booked balance** — Balance derived from entries that have been posted to the account.

**Clearing** — Processing and exchange of payment information and, in some systems, calculation of obligations between participants prior to settlement.

**Compensation** — A new action that counteracts or corrects a previous action that cannot simply be rolled back.

**Correspondent banking** — An arrangement in which one bank provides account or payment services to another bank, commonly used in cross-border payments.

**Domain event** — A durable representation or notification of a meaningful business fact, depending on the system design.

**Finality** — The point at which settlement or payment is considered irrevocable under the applicable legal and scheme framework. Exact meaning must be defined for the relevant system.

**Idempotency** — Property that repeated execution of the same logical request does not create additional unintended business effects.

**Instruction** — A request to perform a payment. The instruction is not the same as payment completion.

**Ledger** — Authoritative accounting record of financial entries and positions for a defined scope.

**On-us payment** — A payment where the sending and receiving accounts are maintained by the same institution, allowing the value movement to be recorded internally.

**Payment rail** — A set of rules, participants, messaging, clearing, and settlement arrangements used to process a class of payments.

**Projection / read model** — A view derived from authoritative facts and optimized for a particular audience or query pattern.

**Reconciliation** — Comparison of independent records to verify that payment and accounting evidence agree and to detect breaks.

**Reservation / hold** — A mechanism that reduces spendable funds without necessarily changing the final booked balance yet.

**Return** — A payment sent back after prior processing or acceptance according to product or scheme rules.

**Reversal** — A financial entry or process that counteracts a previous financial effect while preserving history.

**RTGS** — Real-time gross settlement: settlement of individual obligations in real time rather than netting them for later batch settlement.

**Settlement** — Discharge of financial obligations between participants using the relevant settlement mechanism and asset.

**Settlement finality** — Legally and operationally significant point at which settlement becomes irrevocable under the applicable framework.

**System of record** — A system authoritative for a defined category of facts. A payment platform can have several systems of record for different facts.

**Telemetry** — Logs, metrics, traces, and similar operational signals describing software behavior.

**Workflow state** — State used to coordinate and recover a business process. It is not automatically accounting or domain truth.

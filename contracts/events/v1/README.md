# Event contracts, version 1

`envelope.schema.json` defines the shared event metadata and generic object payload.
Go's `internal/contracts.Event` mirrors this shape. Aggregate versions and event
sequences use decimal strings; timestamps are UTC. Feature phases add typed
payloads, transactional persistence, sequencing and delivery. See ADR 0002.

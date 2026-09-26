# Simulator message contracts, version 1

`message.schema.json` defines a versioned envelope with an opaque external message
ID, authenticated source, correlation, timestamp and object payload. Feature
phases define individual instructions, callbacks and independent report payloads
before implementing their adapters. This envelope does not prove settlement.

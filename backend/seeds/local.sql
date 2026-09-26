-- Infrastructure-only synthetic marker; no customers, accounts or money.
BEGIN;
INSERT INTO _gtb.seed_manifest (dataset, domain)
VALUES ('local-i02-v1', current_database())
ON CONFLICT (dataset) DO NOTHING;

DO $$
BEGIN
  IF current_database() = 'corebanking' THEN
    INSERT INTO app.accounts (
      account_id, external_reference, customer_id, account_kind, currency, booked_balance_minor
    ) VALUES
      ('10000000-0000-4000-8000-000000000001', 'customer/maya/sgd',
       '20000000-0000-4000-8000-000000000001', 'CUSTOMER_DEPOSIT', 'SGD', 100000),
      ('10000000-0000-4000-8000-000000000002', 'customer/daniel/sgd',
       '20000000-0000-4000-8000-000000000002', 'CUSTOMER_DEPOSIT', 'SGD', 20000),
      ('10000000-0000-4000-8000-000000000003', 'system/outgoing-clearing/sgd',
       NULL, 'OUTGOING_CLEARING', 'SGD', 0),
      ('10000000-0000-4000-8000-000000000004', 'system/settlement-nostro/sgd',
       NULL, 'SETTLEMENT_NOSTRO', 'SGD', 120000),
      ('10000000-0000-4000-8000-000000000005', 'system/incoming-suspense/sgd',
       NULL, 'INCOMING_SUSPENSE', 'SGD', 0)
    ON CONFLICT (account_id) DO NOTHING;

    INSERT INTO app.operations (
      operation_id, operation_type, request_hash, resource_type, resource_id, correlation_id
    ) VALUES (
      '30000000-0000-4000-8000-000000000001', 'POST',
      '7b2f5c50131b767a139eaa8f5d829a9be0a46e54ed46b1c4832667bbdf2e70d7',
      'POSTING', '40000000-0000-4000-8000-000000000001',
      '50000000-0000-4000-8000-000000000001'
    ) ON CONFLICT (operation_id) DO NOTHING;

    INSERT INTO app.posting_journals (posting_id, operation_id, description)
    VALUES (
      '40000000-0000-4000-8000-000000000001',
      '30000000-0000-4000-8000-000000000001',
      'Balanced local opening balances'
    ) ON CONFLICT (posting_id) DO NOTHING;

    INSERT INTO app.posting_entries (
      posting_id, line_number, account_id, side, amount_minor, currency
    ) VALUES
      ('40000000-0000-4000-8000-000000000001', 1,
       '10000000-0000-4000-8000-000000000004', 'DEBIT', 120000, 'SGD'),
      ('40000000-0000-4000-8000-000000000001', 2,
       '10000000-0000-4000-8000-000000000001', 'CREDIT', 100000, 'SGD'),
      ('40000000-0000-4000-8000-000000000001', 3,
       '10000000-0000-4000-8000-000000000002', 'CREDIT', 20000, 'SGD')
    ON CONFLICT (posting_id, line_number) DO NOTHING;

    INSERT INTO app.posting_events (
      event_id, event_type, schema_version, aggregate_type, aggregate_id,
      aggregate_version, event_sequence, occurred_at, correlation_id,
      causation_id, source, payload
    ) VALUES (
      '60000000-0000-4000-8000-000000000001',
      'CoreBankingPostingCommitted', 1, 'CoreBankingPosting',
      '40000000-0000-4000-8000-000000000001', 1, 1,
      '2026-01-01T00:00:00Z', '50000000-0000-4000-8000-000000000001',
      '30000000-0000-4000-8000-000000000001', 'core-banking-service',
      '{"operationId":"30000000-0000-4000-8000-000000000001","postingId":"40000000-0000-4000-8000-000000000001","reversalOfPostingId":null,"entries":[{"accountId":"10000000-0000-4000-8000-000000000004","side":"DEBIT","amount":{"amountMinor":"120000","currency":"SGD"}},{"accountId":"10000000-0000-4000-8000-000000000001","side":"CREDIT","amount":{"amountMinor":"100000","currency":"SGD"}},{"accountId":"10000000-0000-4000-8000-000000000002","side":"CREDIT","amount":{"amountMinor":"20000","currency":"SGD"}}]}'::jsonb
    ) ON CONFLICT (event_id) DO NOTHING;

    INSERT INTO app.posting_outbox (event_id, occurred_at)
    VALUES ('60000000-0000-4000-8000-000000000001', '2026-01-01T00:00:00Z')
    ON CONFLICT (event_id) DO NOTHING;

    INSERT INTO _gtb.seed_manifest (dataset, domain)
    VALUES ('local-b01-v1', current_database())
    ON CONFLICT (dataset) DO NOTHING;
  END IF;
END $$;
COMMIT;

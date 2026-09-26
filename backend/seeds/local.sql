-- Infrastructure-only synthetic marker; no customers, accounts or money.
BEGIN;
INSERT INTO _gtb.seed_manifest (dataset, domain)
VALUES ('local-i02-v1', current_database())
ON CONFLICT (dataset) DO NOTHING;
COMMIT;

-- Infrastructure scaffolding only; business tables arrive in their owning phases.
CREATE SCHEMA app;
REVOKE ALL ON SCHEMA app FROM PUBLIC;
CREATE TABLE _gtb.seed_manifest (
    dataset text PRIMARY KEY,
    domain text NOT NULL CHECK (domain = current_database()),
    seeded_at timestamptz NOT NULL DEFAULT now()
);

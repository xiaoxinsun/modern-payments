#!/usr/bin/env python3
"""Explicitly recreate local containers and verify persistent state."""
import importlib.util
import json
from pathlib import Path
import subprocess
import uuid

spec = importlib.util.spec_from_file_location("database", Path(__file__).resolve().parents[1] / "database.py")
db = importlib.util.module_from_spec(spec)
spec.loader.exec_module(db)


def command(arguments, data=None):
    result = subprocess.run(db.COMPOSE + arguments, input=data, text=True,
                            stdout=subprocess.PIPE, check=True)
    return result.stdout.strip()


def snapshot():
    records = {}
    for domain in db.DOMAINS:
        records[domain] = db.domain_sql(domain, """
SELECT json_agg(s ORDER BY dataset) FROM _gtb.seed_manifest s;
SELECT json_agg(m ORDER BY name) FROM _gtb.schema_migrations m;
""").stdout.strip()
        if "local-i02-v1" not in records[domain]:
            raise RuntimeError(f"Missing seed in {domain}; run make db-seed")
    records["oidc"] = db.psql("oidc", "oidc", "OIDC_DB_PASSWORD",
                              "SELECT id FROM realm WHERE name = 'master';").stdout.strip()
    namespace = command(["run", "--rm", "--no-deps", "--entrypoint", "temporal", "temporal-namespace",
                         "operator", "namespace", "describe", "--namespace", "gtb", "--output", "json"])
    records["temporal"] = json.loads(namespace)["namespaceInfo"]["id"]
    if not records["oidc"] or not records["temporal"]:
        raise RuntimeError("Missing identity/workflow metadata")
    return records


def main():
    before = snapshot()
    topic = "gtb.i02.persistence." + uuid.uuid4().hex
    kafka = ["exec", "-T", "kafka"]
    command(kafka + ["/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "localhost:19092",
                     "--create", "--topic", topic, "--partitions", "1", "--replication-factor", "1"])
    try:
        command(kafka + ["/opt/kafka/bin/kafka-console-producer.sh", "--bootstrap-server", "localhost:19092",
                         "--topic", topic], "i02-persisted-record\n")
        command(["down"])
        command(["up", "-d", "--wait", "--wait-timeout", "300"])
        if before != snapshot():
            raise RuntimeError("Application seeds/migrations or Temporal/OIDC state changed after restart")
        message = command(kafka + ["/opt/kafka/bin/kafka-console-consumer.sh", "--bootstrap-server", "localhost:19092",
                                   "--topic", topic, "--from-beginning", "--max-messages", "1", "--timeout-ms", "15000"])
        if message != "i02-persisted-record":
            raise RuntimeError("Kafka record did not survive restart")
        print("PASS all four domain seed/migration records, Temporal namespace, OIDC realm and Kafka message survived down/up")
    finally:
        command(kafka + ["/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "localhost:19092",
                         "--delete", "--topic", topic])


if __name__ == "__main__":
    main()

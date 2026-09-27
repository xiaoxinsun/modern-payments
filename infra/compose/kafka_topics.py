#!/usr/bin/env python3
"""Create the GTB event topics explicitly (auto-creation is disabled); safe to rerun."""
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
COMPOSE = ["docker", "compose", "--env-file", str(ROOT / "infra/compose/.env"),
           "-f", str(ROOT / "infra/compose/compose.yaml")]
KAFKA = COMPOSE + ["exec", "-T", "kafka", "/opt/kafka/bin/kafka-topics.sh", "--bootstrap-server", "localhost:19092"]

# Keys are aggregate IDs, so partitions only spread load; ordering holds per
# aggregate. Kafka is not the system of record: the PostgreSQL journals are, and
# projections rebuild from them, so a finite retention is safe.
TOPICS = {
    "gtb.payment.events.v1": 3,
    "gtb.corebanking.postings.v1": 3,
}
RETENTION_MS = str(7 * 24 * 60 * 60 * 1000)


def run(arguments):
    result = subprocess.run(KAFKA + arguments, text=True, capture_output=True)
    if result.returncode:
        sys.exit(f"kafka-topics failed: {result.stderr.strip()}")
    return result.stdout


for topic, partitions in TOPICS.items():
    run(["--create", "--if-not-exists", "--topic", topic, "--partitions", str(partitions),
         "--replication-factor", "1", "--config", "cleanup.policy=delete",
         "--config", f"retention.ms={RETENTION_MS}"])
    description = run(["--describe", "--topic", topic])
    count = sum(1 for line in description.splitlines() if "\tPartition: " in line)
    if count < partitions:
        sys.exit(f"{topic} has {count} partitions; expected at least {partitions}")
    print(f"Topic ready: {topic} ({count} partitions, retention {RETENTION_MS} ms)")

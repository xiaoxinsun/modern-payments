#!/usr/bin/env python3
"""Exercise B03 event delivery and queries end to end without printing credentials.

Runs core-banking-service, payment-api, payment-worker (payment relay),
event-projector and query-api on loopback test ports against the local stack,
then interrupts Kafka and the projector, forces redelivery, quarantines a
poison record and rebuilds the projections while payments keep arriving.
Stop any walkthrough event-projector first: it shares the consumer group.
"""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import uuid

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "infra" / "compose"))
sys.path.insert(0, str(Path(__file__).resolve().parent))
import database as db  # noqa: E402
import oidc_sync  # noqa: E402
from payment_smoke import (  # noqa: E402
    BIN, MAYA_ACCOUNT, MAYA_CUSTOMER, DANIEL_ACCOUNT, DANIEL_CUSTOMER, Service, base_environment,
    credentials, expect, http, payment_request, token, user_token,
)

PAYMENT_API = "http://127.0.0.1:18082"
QUERY_API = "http://127.0.0.1:18083"
CORE_BANKING = "http://127.0.0.1:18084"
PROJECTOR = "http://127.0.0.1:18085"
WORKER = "http://127.0.0.1:18086"
COMPOSE = db.COMPOSE
PAYMENT_TOPIC = "gtb.payment.events.v1"
POSTINGS_TOPIC = "gtb.corebanking.postings.v1"
STALE_AFTER = 6


def admin_sql(database, sql):
    return db.psql("gtb_admin", database, "POSTGRES_ADMIN_PASSWORD", sql).stdout.strip()


def wait_for(name, probe, timeout=30.0, interval=0.5):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = probe()
        if last:
            return last
        time.sleep(interval)
    raise AssertionError(f"{name}: condition not met within {timeout}s (last {last})")


def kafka(*arguments, stdin=None):
    return subprocess.run(COMPOSE + ["exec", "-T", "kafka", *arguments], input=stdin, text=True,
                          capture_output=True, check=True).stdout


class Stack:
    def __init__(self, values):
        self.values = values
        core_env = base_environment("127.0.0.1:18084", "core-banking-service", "corebanking", values["COREBANKING_DB_PASSWORD"])
        payment_env = base_environment("127.0.0.1:18082", "payment-api", "payment", values["PAYMENT_DB_PASSWORD"])
        payment_env.update({"GTB_SERVICE_CLIENT_SECRET": values["PAYMENT_SERVICE_OIDC_SECRET"], "GTB_COREBANKING_URL": CORE_BANKING})
        worker_env = base_environment("127.0.0.1:18086", "payment-worker", "payment", values["PAYMENT_DB_PASSWORD"])
        self.projector_env = base_environment("127.0.0.1:18085", "event-projector", "projections", values["PROJECTIONS_DB_PASSWORD"])
        self.projector_env.update({"GTB_SERVICE_CLIENT_SECRET": values["EVENT_PROJECTOR_OIDC_SECRET"], "GTB_PAYMENT_API_URL": PAYMENT_API})
        query_env = base_environment("127.0.0.1:18083", "payment-api", "projections", values["QUERY_READER_DB_PASSWORD"])
        query_env["GTB_DATABASE_URL"] = query_env["GTB_DATABASE_URL"].replace("//projections:", "//query_reader:", 1)
        query_env.update({"GTB_PAYMENT_API_URL": PAYMENT_API, "GTB_PROJECTION_STALE_AFTER": f"{STALE_AFTER}s"})
        self.core = Service("core-banking", BIN / "core-banking-service", core_env, CORE_BANKING + "/health/ready")
        self.payment = Service("payment-api", BIN / "payment-api", payment_env, PAYMENT_API + "/health/ready")
        self.worker = Service("payment-worker", BIN / "payment-worker", worker_env, WORKER + "/health/ready")
        self.projector = Service("event-projector", BIN / "event-projector", self.projector_env, PROJECTOR + "/health/ready")
        self.query = Service("query-api", BIN / "query-api", query_env, QUERY_API + "/health/ready")
        self.services = [self.core, self.payment, self.worker, self.projector, self.query]
        self.extra_logs = []

    def restart_projector(self):
        self.projector.log.close()
        self.extra_logs.append(self.projector.log)
        self.projector.log = tempfile.NamedTemporaryFile(prefix="gtb-b03-event-projector-", suffix=".jsonl", delete=False)
        self.projector.start()


def main():
    values = credentials()
    required = ["PAYMENT_DB_PASSWORD", "COREBANKING_DB_PASSWORD", "PROJECTIONS_DB_PASSWORD", "QUERY_READER_DB_PASSWORD",
                "PAYMENT_SERVICE_OIDC_SECRET", "EVENT_PROJECTOR_OIDC_SECRET", "GTB_CUSTOMER_PASSWORD",
                "GTB_OPERATIONS_PASSWORD", "GTB_COMPLIANCE_PASSWORD", "OIDC_ADMIN_PASSWORD"]
    if any(not values.get(name) for name in required):
        raise RuntimeError("missing local credentials; run make infra-init db-provision")
    for binary in ("core-banking-service", "payment-api", "payment-worker", "event-projector", "query-api"):
        if not (BIN / binary).exists():
            raise RuntimeError(f"{binary} binary missing; run make build")
    oidc_sync.main()
    subprocess.run([sys.executable, str(ROOT / "infra" / "compose" / "kafka_topics.py")], check=True)

    stack = Stack(values)
    customer = lambda: user_token("customer.demo", values["GTB_CUSTOMER_PASSWORD"])
    ops = lambda: user_token("operations.demo", values["GTB_OPERATIONS_PASSWORD"])
    compliance = lambda: user_token("compliance.demo", values["GTB_COMPLIANCE_PASSWORD"])
    service_token = token({"grant_type": "client_credentials", "client_id": "payment-service",
                           "client_secret": values["PAYMENT_SERVICE_OIDC_SECRET"]})

    def submit(amount):
        status, headers, body = http("POST", PAYMENT_API + "/api/v1/payments", customer(), payment_request(amount=amount),
                                     {"Idempotency-Key": "b03-smoke-" + uuid.uuid4().hex})
        if status != 202:
            raise AssertionError(f"submit returned {status}: {body}")
        return body["paymentId"]

    def view(payment_id, bearer=None, query=""):
        return http("GET", f"{QUERY_API}/api/v1/payments/{payment_id}{query}", bearer or customer())

    def projected(payment_id):
        status, _, body = view(payment_id)
        return body if status == 200 and body["status"] == "PROJECTED" else None

    def freshness():
        status, _, body = http("GET", QUERY_API + "/api/v1/ops/projections", ops())
        consumer = [c for c in (body or {}).get("consumers", []) if c["consumer"] == "payment-projector"]
        return consumer[0]["freshness"] if status == 200 and consumer else None

    def outbox_state(payment_id):
        return admin_sql("payment", f"""SELECT count(*) FILTER (WHERE o.published_at IS NULL), max(o.delivery_attempts)
            FROM app.outbox o JOIN app.domain_events e USING (event_id) WHERE e.aggregate_id = '{payment_id}';""")

    try:
        for service in stack.services:
            service.start()
        expect("five services ready", len(stack.services), 5)

        # 1. Relay -> Kafka -> projector -> query, with freshness and scope.
        first = submit("101")
        body = wait_for("first payment projected", lambda: projected(first))
        expect("projected view version", (body["status"], body["version"], body["lifecycleState"]), ("PROJECTED", "1", "RECEIVED"))
        expect("projection freshness", wait_for("fresh", lambda: freshness() == "CURRENT" and "CURRENT"), "CURRENT")
        status, _, timeline = http("GET", f"{QUERY_API}/api/v1/payments/{first}/timeline", customer())
        expect("timeline", (status, [e["eventType"] for e in timeline["entries"]]), (200, ["PaymentReceived"]))
        status, _, page = http("GET", f"{QUERY_API}/api/v1/payments?customerId={MAYA_CUSTOMER}&limit=5", customer())
        expect("customer search includes the payment", (status, first in [p["paymentId"] for p in page["items"]]), (200, True))
        expect("customer search outside scope", http("GET", f"{QUERY_API}/api/v1/payments?customerId={DANIEL_CUSTOMER}", customer())[0], 403)
        expect("customer search without customerId", http("GET", f"{QUERY_API}/api/v1/payments", customer())[0], 400)
        expect("operator search", http("GET", f"{QUERY_API}/api/v1/payments?limit=3", ops())[0], 200)
        expect("compliance reviewer denied", view(first, compliance())[0], 403)
        expect("customer denied ops status", http("GET", QUERY_API + "/api/v1/ops/projections", customer())[0], 403)
        status, _, body = view(first, query="?minVersion=2")
        expect("minVersion above projection", (status, body["status"]), (200, "BEHIND"))
        expect("unknown payment", view(str(uuid.uuid4()))[0], 404)

        # 2. Projector outage: receipts stay readable (PENDING), views go STALE.
        stack.projector.stop()
        second = submit("102")
        status, _, body = view(second)
        expect("unprojected payment answered from the command store", (status, body["status"], body["lifecycleState"]), (200, "PENDING", "RECEIVED"))
        status, _, body = http("GET", f"{QUERY_API}/api/v1/payments/{second}/timeline", customer())
        expect("unprojected timeline", (status, body["status"], body["entries"]), (200, "PENDING", []))
        expect("projector outage shows stale views", wait_for("stale", lambda: freshness() == "STALE" and "STALE", timeout=STALE_AFTER + 10), "STALE")
        expect("stale view still served", view(first)[2]["projection"]["freshness"], "STALE")
        stack.restart_projector()
        wait_for("second payment projected after projector restart", lambda: projected(second))
        expect("freshness recovers", wait_for("fresh again", lambda: freshness() == "CURRENT" and "CURRENT"), "CURRENT")
        print("PASS projector restart resumed from its database checkpoint")

        # 3. Broker outage: receipts commit, the outbox holds events, views go STALE.
        subprocess.run(COMPOSE + ["stop", "kafka"], check=True, capture_output=True)
        try:
            third = submit("103")
            print("PASS receipt committed while Kafka is down")
            wait_for("relay retries while Kafka is down", lambda: (lambda s: s.split("|")[0] == "1" and int(s.split("|")[1] or 0) >= 1)(outbox_state(third)), timeout=40)
            print("PASS event held in the outbox with failed delivery attempts recorded")
            expect("broker outage pending view", view(third)[2]["status"], "PENDING")
            expect("broker outage shows stale views", wait_for("stale", lambda: freshness() == "STALE" and "STALE", timeout=STALE_AFTER + 15), "STALE")
        finally:
            subprocess.run(COMPOSE + ["up", "-d", "--wait", "--wait-timeout", "180", "kafka"], check=True, capture_output=True)
        wait_for("third payment projected after Kafka returns", lambda: projected(third), timeout=90)
        expect("outbox drained after Kafka returns", outbox_state(third).split("|")[0], "0")
        expect("freshness recovers after broker outage", wait_for("fresh", lambda: freshness() == "CURRENT" and "CURRENT", timeout=60), "CURRENT")

        # 4. Redelivery: a relay republish and a lost checkpoint change nothing.
        active = lambda: admin_sql("projections", "SELECT generation FROM app.projection_generations WHERE status = 'ACTIVE';")
        counts = lambda: admin_sql("projections", f"""SELECT (SELECT count(*) FROM app.payment_summary WHERE generation = {active()}),
            (SELECT count(*) FROM app.payment_timeline WHERE generation = {active()}),
            (SELECT count(*) FROM app.inbox WHERE generation = {active()});""")
        offsets = lambda: int(admin_sql("projections", "SELECT COALESCE(sum(next_offset), 0) FROM app.consumer_offsets WHERE consumer = 'payment-projector';"))
        before, offsets_before = counts(), offsets()
        event_id = admin_sql("payment", f"SELECT event_id FROM app.domain_events WHERE aggregate_id = '{first}' AND event_sequence = 1;")
        admin_sql("payment", f"UPDATE app.outbox SET published_at = NULL WHERE event_id = '{event_id}';")
        wait_for("republished event consumed", lambda: offsets() > offsets_before)
        expect("relay republish is deduplicated", counts(), before)
        expect("republished event keeps its identity",
               admin_sql("payment", f"SELECT delivery_attempts >= 2 AND published_at IS NOT NULL FROM app.outbox WHERE event_id = '{event_id}';"), "t")
        stack.projector.stop()
        admin_sql("projections", "DELETE FROM app.consumer_offsets WHERE consumer = 'payment-projector';")
        stack.restart_projector()
        wait_for("topic replayed from the start", lambda: offsets() >= offsets_before + 1, timeout=60)
        time.sleep(2)
        expect("full redelivery after checkpoint loss is deduplicated", counts(), before)

        # 5. Poison record: quarantined and visible; the partition keeps moving.
        poison_key = "poison-" + uuid.uuid4().hex
        kafka("/opt/kafka/bin/kafka-console-producer.sh", "--bootstrap-server", "localhost:19092", "--topic", PAYMENT_TOPIC,
              "--property", "parse.key=true", "--property", "key.separator=|", stdin=f"{poison_key}|not an event\n")
        wait_for("poison record in repair queue", lambda: admin_sql("projections",
                 "SELECT count(*) FROM app.repair_queue WHERE status = 'OPEN' AND record_value = 'not an event';") == "1")
        status, _, body = http("GET", QUERY_API + "/api/v1/ops/projections", ops())
        expect("open repairs visible to operators", (status, int(body["openRepairs"]["count"]) >= 1), (200, True))
        after_poison = submit("104")
        wait_for("payment after poison projected", lambda: projected(after_poison))
        print("PASS poison record did not block its partition")
        admin_sql("projections", "UPDATE app.repair_queue SET status = 'RESOLVED' WHERE record_value = 'not an event';")

        # 6. Shadow rebuild while payments keep arriving; queries are equivalent,
        #    and nothing on the command or ledger side is replayed.
        sample = [first, second, third, after_poison]
        def snapshot(payment_id):
            _, _, v = view(payment_id)
            _, _, t = http("GET", f"{QUERY_API}/api/v1/payments/{payment_id}/timeline", customer())
            return ({k: v[k] for k in ("paymentId", "status", "lifecycleState", "executionOutcome", "version", "receivedAt", "amount")},
                    [(e["eventId"], e["eventSequence"], e["eventType"]) for e in t["entries"]])
        views_before = {payment_id: snapshot(payment_id) for payment_id in sample}
        postings_before = admin_sql("corebanking", "SELECT count(*) FROM app.posting_journals;")
        events_before = int(admin_sql("payment", "SELECT count(*) FROM app.domain_events;"))
        generation_before = active()
        concurrent, errors = [], []
        def submit_during_rebuild():
            try:
                for index in range(5):
                    concurrent.append(submit(str(200 + index)))
                    time.sleep(0.3)
            except Exception as failure:  # noqa: BLE001
                errors.append(failure)
        writer = threading.Thread(target=submit_during_rebuild)
        writer.start()
        rebuild = subprocess.run([str(BIN / "event-projector"), "rebuild"], cwd=ROOT, env=stack.projector_env,
                                 capture_output=True, text=True, timeout=180)
        writer.join()
        if errors:
            raise errors[0]
        report = json.loads(rebuild.stdout)
        expect("rebuild exit status", rebuild.returncode, 0)
        expect("rebuild activated a new generation", (report["activated"], report["problems"], str(report["retiredGeneration"])), (True, [], generation_before))
        print(f"PASS rebuild verified {report['projectedPayments']} payments / {report['projectedEvents']} events "
              f"against watermark {report['watermark']['position']} ({report['sampledTimelines']} timelines sampled)")
        expect("reads switched to the rebuilt generation", active(), str(report["generation"]))
        for payment_id in concurrent:
            wait_for(f"concurrent payment {payment_id} projected", lambda p=payment_id: projected(p))
        print("PASS payments submitted during the rebuild are projected in the new generation")
        expect("rebuilt views and timelines equal the originals", {p: snapshot(p) for p in sample} == views_before, True)
        journal = admin_sql("payment", "SELECT count(*), count(DISTINCT aggregate_id) FROM app.domain_events;")
        wait_for("rebuilt generation matches the whole journal", lambda: admin_sql("projections", f"""SELECT
            (SELECT count(*) FROM app.inbox WHERE generation = {active()}) || '|' ||
            (SELECT count(*) FROM app.payment_summary WHERE generation = {active()});""") == journal, timeout=30)
        print(f"PASS projection equals journal facts ({journal.replace('|', ' events, ')} payments)")
        expect("rebuild posted nothing to the ledger", admin_sql("corebanking", "SELECT count(*) FROM app.posting_journals;"), postings_before)
        expect("rebuild wrote no payment events", int(admin_sql("payment", "SELECT count(*) FROM app.domain_events;")) - events_before, len(concurrent))

        # 7. Core Banking postings are relayed to their own topic.
        booking = lambda amount, debit, credit: {"operationId": str(uuid.uuid4()), "description": "B03 relay smoke", "entries": [
            {"accountId": debit, "side": "DEBIT", "amount": {"amountMinor": amount, "currency": "SGD"}},
            {"accountId": credit, "side": "CREDIT", "amount": {"amountMinor": amount, "currency": "SGD"}}]}
        status, _, posting = http("POST", CORE_BANKING + "/integrations/v1/core-banking/postings", service_token, booking("1", MAYA_ACCOUNT, DANIEL_ACCOUNT))
        expect("posting committed", status, 200)
        status, _, reversal = http("POST", CORE_BANKING + "/integrations/v1/core-banking/reversals", service_token, {
            "operationId": str(uuid.uuid4()), "originalPostingId": posting["postingId"], "description": "B03 relay smoke reversal"})
        expect("posting reversed (balances unchanged)", status, 200)
        wait_for("posting outbox published", lambda: admin_sql("corebanking", f"""SELECT count(*) FROM app.posting_outbox o
            JOIN app.posting_events e USING (event_id) WHERE e.aggregate_id IN ('{posting['postingId']}', '{reversal['postingId']}')
            AND o.published_at IS NOT NULL;""") == "2")
        consumed = kafka("/opt/kafka/bin/kafka-console-consumer.sh", "--bootstrap-server", "localhost:19092", "--topic", POSTINGS_TOPIC,
                         "--from-beginning", "--timeout-ms", "10000", "--property", "print.key=true", "--property", "key.separator=|")
        records = [line.split("|", 1) for line in consumed.splitlines() if "|" in line]
        mine = [json.loads(value) for key, value in records if key in (posting["postingId"], reversal["postingId"])]
        expect("posting events on Kafka keyed by posting ID", sorted(e["eventType"] for e in mine), ["CoreBankingPostingCommitted"] * 2)

        for service in (stack.query, stack.projector, stack.worker, stack.payment, stack.core):
            service.stop()
        print("PASS all services shut down gracefully")
    finally:
        for service in stack.services:
            service.kill()
            service.log.close()
    logs = [(service.name, Path(service.log.name)) for service in stack.services]
    logs += [("event-projector", Path(log.name)) for log in stack.extra_logs]
    for name, path in logs:
        lines = path.read_text(encoding="utf-8").splitlines()
        for line in lines:
            json.loads(line)
        for key, secret in values.items():
            if ("PASSWORD" in key or "SECRET" in key) and secret and any(secret in line for line in lines):
                raise AssertionError(f"credential value leaked in {name} log for {key}")
        path.unlink()
    print("PASS structured logs parsed and contained no local credential values")
    print(f"B03 eventing smoke checks passed (payments {first}, {third})")


if __name__ == "__main__":
    try:
        main()
    except Exception as failure:
        print(f"FAIL: {failure}", file=sys.stderr)
        sys.exit(1)

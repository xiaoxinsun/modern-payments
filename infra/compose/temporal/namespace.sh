#!/bin/sh
set -eu

# Retry transient initialization races, but never hide a permanently failed setup.
attempt=0
until temporal operator cluster health; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 12 ]; then exit 1; fi
    sleep 5
done
attempt=0
until temporal operator namespace describe --namespace gtb >/dev/null 2>&1 || \
    temporal operator namespace create --namespace gtb --retention 72h; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 12 ]; then exit 1; fi
    sleep 5
done

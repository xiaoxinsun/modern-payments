#!/bin/sh
set -eu

# Databases already belong to Temporal. These commands also accept existing schemas.
for database in temporal temporal_visibility; do
    schema=temporal
    if [ "$database" = temporal_visibility ]; then schema=visibility; fi
    temporal-sql-tool --plugin postgres12 --ep "$POSTGRES_SEEDS" \
        -u "$POSTGRES_USER" -p "$DB_PORT" --db "$database" setup-schema -v 0.0
    temporal-sql-tool --plugin postgres12 --ep "$POSTGRES_SEEDS" \
        -u "$POSTGRES_USER" -p "$DB_PORT" --db "$database" update-schema \
        -d "/etc/temporal/schema/postgresql/v12/$schema/versioned"
done

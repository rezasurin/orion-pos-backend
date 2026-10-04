#!/usr/bin/env bash
# Restore drill: prove that a backup of the Orion database can be restored and is complete.
#
# It dumps the source database, restores the dump into a fresh scratch database, then compares
# the migration version and the row count of every table, and prints a verdict. It never writes
# to the source. Run it monthly, and once before the pilot; record the result in
# docs/runbooks/restore.md (see docs/guides/hosting-and-restore.md).
#
#   SOURCE_URL         schema-owner URL of the database to check (ORION_MIGRATE_DATABASE_URL).
#                      Use the owner, not orion_api: row-level security would hide other tenants'
#                      rows from the service role and the counts would be wrong.
#   SCRATCH_ADMIN_URL  URL of a server where a scratch database may be created and dropped, as a
#                      role that can CREATE DATABASE (never the production server's superuser).
#   DUMP_FILE          optional: restore this existing custom-format dump (pg_dump -Fc, or a
#                      nightly logical backup) instead of taking a new one.
#   KEEP_SCRATCH=1     keep the scratch database afterwards, to poke at it.
#
# The restore uses --no-owner --no-privileges, so this drill proves the data, not the role
# setup; recreate roles as in deploy/README.md when restoring for real.
set -euo pipefail

: "${SOURCE_URL:?set SOURCE_URL to the schema-owner URL of the database to check}"
: "${SCRATCH_ADMIN_URL:?set SCRATCH_ADMIN_URL to a server URL where a scratch database may be created}"

scratch="orion_drill_$(date -u +%Y%m%d%H%M%S)"
work="$(mktemp -d)"
dump="${DUMP_FILE:-$work/orion.dump}"
# The same server, a different database name: swap the path of SCRATCH_ADMIN_URL.
scratch_url="$(printf '%s' "$SCRATCH_ADMIN_URL" | sed -E "s#(://[^/]+)/[^?]*#\1/${scratch}#")"

cleanup() {
  if [ "${KEEP_SCRATCH:-}" != "1" ]; then
    psql "$SCRATCH_ADMIN_URL" -qAtc "DROP DATABASE IF EXISTS \"$scratch\"" >/dev/null 2>&1 || true
  else
    echo "kept scratch database: $scratch_url"
  fi
  rm -rf "$work"
}
trap cleanup EXIT

if [ -z "${DUMP_FILE:-}" ]; then
  echo "1/4 dumping the source"
  pg_dump --format=custom --no-owner --no-privileges --file "$dump" "$SOURCE_URL"
else
  echo "1/4 using the dump $DUMP_FILE"
fi
ls -lh "$dump" | awk '{print "    dump size: " $5}'

echo "2/4 restoring into $scratch"
psql "$SCRATCH_ADMIN_URL" -qAtc "CREATE DATABASE \"$scratch\"" >/dev/null
# Group roles named in grants are skipped by --no-privileges; extensions come with the dump.
pg_restore --no-owner --no-privileges --exit-on-error --dbname "$scratch_url" "$dump"

counts() { # table<TAB>count for every table in the public schema, sorted
  psql "$1" -qAt -F $'\t' <<'SQL'
SELECT format('SELECT %L, count(*) FROM %I.%I', table_name, table_schema, table_name)
FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY table_name
\gexec
SQL
}
version() {
  psql "$1" -qAtc "SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied"
}

echo "3/4 comparing migration versions and row counts"
src_version="$(version "$SOURCE_URL")"
dst_version="$(version "$scratch_url")"
counts "$SOURCE_URL" >"$work/source.counts"
counts "$scratch_url" >"$work/scratch.counts"

status=0
if [ "$src_version" != "$dst_version" ]; then
  echo "    FAIL migration version: source $src_version, restored $dst_version"
  status=1
else
  echo "    migration version $dst_version matches"
fi
if ! diff -u "$work/source.counts" "$work/scratch.counts" >"$work/diff"; then
  echo "    FAIL row counts differ (- source, + restored):"
  sed 's/^/      /' "$work/diff"
  status=1
else
  tables="$(wc -l <"$work/source.counts" | tr -d ' ')"
  rows="$(awk -F'\t' '{s+=$2} END {print s+0}' "$work/source.counts")"
  echo "    $tables tables, $rows rows, all counts match"
fi

echo "4/4 verdict"
if [ "$status" -eq 0 ]; then
  echo "    PASS: the backup restores completely"
else
  echo "    FAIL: do not trust this backup"
fi
exit "$status"

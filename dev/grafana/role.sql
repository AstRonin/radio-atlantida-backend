-- Read-only database role of Grafana (docs/tech-design.md §10.3,
-- docs/phase-14-plan.md decision 4). Run as a superuser every time the
-- monitoring services start (service grafana-role); safe to run again.
--
-- The password comes from the session setting radio.grafana_password, set
-- by the caller (PGOPTIONS='-c radio.grafana_password=...' for psql), so
-- this file carries no secret. Plain SQL without psql meta-commands: the
-- integration test (grafana_test.go) runs the same file through pgx.
--
-- Copied to radio-atlantida-deploy (config/grafana/role.sql); change both.

SET client_min_messages = warning;

DO $$
DECLARE
    pw text := current_setting('radio.grafana_password', true);
BEGIN
    IF pw IS NULL OR pw = '' THEN
        RAISE EXCEPTION 'radio.grafana_password is not set';
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'grafana_ro') THEN
        CREATE ROLE grafana_ro LOGIN;
    END IF;
    EXECUTE format('ALTER ROLE grafana_ro WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE '
        'NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT 3 PASSWORD %L', pw);
END
$$;

-- Reads of the statistics views, including other sessions' query text in
-- pg_stat_activity. No grant on application tables.
GRANT pg_monitor TO grafana_ro;

ALTER ROLE grafana_ro SET statement_timeout = '5s';
ALTER ROLE grafana_ro SET idle_in_transaction_session_timeout = '10s';
ALTER ROLE grafana_ro SET default_transaction_read_only = on;

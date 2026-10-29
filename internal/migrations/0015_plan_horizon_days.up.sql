-- 0015: rotation.plan_horizon_hours becomes rotation.plan_horizon_days
-- (docs/rotation-long-horizon-plan.md). A saved hours value is copied once as
-- ceil(hours / 24) days, unless the days value is saved already. The old key
-- is no longer defined, so the settings never read it again; its row stays.
-- Data only: no schema change.

INSERT INTO settings (key, value, updated_at)
SELECT 'rotation.plan_horizon_days',
       to_jsonb(CEIL(h.hours / 24.0)::integer),
       now()
FROM (
    SELECT (value #>> '{}')::numeric AS hours
    FROM settings
    WHERE key = 'rotation.plan_horizon_hours' AND jsonb_typeof(value) = 'number'
) AS h
ON CONFLICT (key) DO NOTHING;

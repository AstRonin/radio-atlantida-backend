-- 0017: the saved rotation.plan_horizon_hours is not converted to days
-- (docs/rotation-long-horizon-plan.md, decision after migration 0015). A
-- station on the old default gets the default of rotation.plan_horizon_days,
-- 31 days. This removes only the days row that 0015 copied from the hours row:
-- the same value as ceil(hours / 24) while the hours row exists. Data only; no
-- schema change. Migration 0015 itself is not edited (it is applied).

DELETE FROM settings AS d
WHERE d.key = 'rotation.plan_horizon_days'
  AND EXISTS (
    SELECT 1
    FROM settings AS h
    WHERE h.key = 'rotation.plan_horizon_hours'
      AND jsonb_typeof(h.value) = 'number'
      AND d.value = to_jsonb(CEIL((h.value #>> '{}')::numeric / 24.0)::integer)
  );

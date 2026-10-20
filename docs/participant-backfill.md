# Participant journal backfill

`email.thread_participant` is filled by the snapshot consumer. Threads handled
before it was deployed have no rows, so their earlier agents cannot read them.
This one-off runbook adds what `call_center` can still prove. Run it only when
the check below finds such Threads, and before granting read access.

The result is a lower bound: an agent who took an email and returned it to the
queue within the same attempt is not recovered, since `call_center` keeps only
the last agent of an attempt. Replaying `cc_email_attempt_outbox` is not a
source: published rows are deleted by retention.

## 1. Check

```sql
SELECT count(*) AS projected_threads
FROM email.thread
WHERE kind = 'regular' AND (projection_version IS NOT NULL OR last_handled_version IS NOT NULL);

SELECT (SELECT count(*) FROM call_center.cc_member_attempt WHERE channel = 'email') AS attempts,
       (SELECT count(*) FROM call_center.cc_member_attempt_history WHERE channel = 'email') AS finished;
```

All zero: no backfill; record that in the rollout notes.

## 2. Candidates

One statement reads all three sources, so an attempt moved to history while it
runs is seen exactly once. `member_call_id` is matched as text, never cast, and
every attempt must be confirmed by its join.

```sql
CREATE TEMP TABLE participant_candidates AS
WITH attempts AS (
    -- Current attempts after Take; bridged_at alone proves nothing here.
    SELECT a.id, a.domain_id, a.member_call_id, a.agent_id, 'current' AS source
    FROM call_center.cc_member_attempt a
    WHERE a.channel = 'email' AND a.agent_id IS NOT NULL
      AND a.state IN ('active', 'postponed', 'processing')
    UNION ALL
    -- Finished, not yet moved by SaveToHistory.
    SELECT a.id, a.domain_id, a.member_call_id, a.agent_id, 'leaving'
    FROM call_center.cc_member_attempt a
    WHERE a.channel = 'email' AND a.state = 'leaving' AND a.agent_id IS NOT NULL
      AND a.bridged_at IS NOT NULL AND a.result IN ('processed', 'processing_timeout')
    UNION ALL
    SELECT h.id, h.domain_id, h.member_call_id, h.agent_id, 'history'
    FROM call_center.cc_member_attempt_history h
    WHERE h.channel = 'email' AND h.agent_id IS NOT NULL
      AND h.bridged_at IS NOT NULL AND h.result IN ('processed', 'processing_timeout')
)
SELECT at.*, t.id AS thread_id, ag.user_id,
    EXISTS (
        SELECT 1 FROM call_center.cc_email_join j
        WHERE j.state = 'applied' AND j.domain_id = at.domain_id
          AND j.thread_id = t.id AND j.attempt_id = at.id
    ) AS confirmed
FROM attempts at
LEFT JOIN email.thread t
    ON t.domain_id = at.domain_id AND t.id::text = at.member_call_id AND t.kind = 'regular'
LEFT JOIN call_center.cc_agent ag ON ag.id = at.agent_id AND ag.domain_id = at.domain_id;
```

## 3. Reconciliation report

Rows that will not become participants, with the reason. Keep the output with
the rollout notes.

```sql
SELECT source, domain_id, id AS attempt_id, member_call_id, agent_id,
    CASE WHEN thread_id IS NULL THEN 'no_thread'
         WHEN NOT confirmed THEN 'not_confirmed'
         ELSE 'no_user' END AS reason
FROM participant_candidates
WHERE thread_id IS NULL OR NOT confirmed OR user_id IS NULL
ORDER BY domain_id, attempt_id;
```

## 4. Backfill

Idempotent; a row written from a snapshot is never touched. Version and time
stay unknown (`NULL`) until a snapshot refines them; `backfilled_at` marks the
origin.

```sql
BEGIN;
INSERT INTO email.thread_participant (domain_id, thread_id, user_id, agent_id, backfilled_at)
SELECT DISTINCT ON (domain_id, thread_id, user_id) domain_id, thread_id, user_id, agent_id, now()
FROM participant_candidates
WHERE thread_id IS NOT NULL AND confirmed AND user_id IS NOT NULL
ORDER BY domain_id, thread_id, user_id, id DESC
ON CONFLICT (domain_id, thread_id, user_id) DO NOTHING;
COMMIT;
```

## 5. Control

`last_handled_*` is a subset of what the journal must hold. Any row here is
investigated before read access is granted.

```sql
SELECT t.domain_id, t.id AS thread_id, t.last_handled_user_id
FROM email.thread t
WHERE t.last_handled_user_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM email.thread_participant p
      WHERE p.domain_id = t.domain_id AND p.thread_id = t.id AND p.user_id = t.last_handled_user_id);
```

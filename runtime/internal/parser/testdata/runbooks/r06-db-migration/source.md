# Runbook 6: Database Migration with Dry-Run and Validation

**Domain:** Data Engineering  
**Complexity:** Linear + Looping (retry) + Compensating actions  
**Interaction:** Human approval + automated validation

## Summary

Migrates production database schema (add columns, indexes, foreign keys). Includes dry-run on
staging, validation queries, production migration, post-migration validation, and automatic
rollback on failure.

## Steps

1. **Pre-Flight Checks** (automated, type: assert)
   - Asserts:
     - Migration script exists: `ls migrations/20260418_add_user_preferences.sql`
     - Staging database reachable: `pg_isready -h staging-db`
     - Production database reachable: `pg_isready -h prod-db`
     - Backup completed within past 24h: query backup metadata
   - Fails if: any assertion fails

2. **Dry-Run on Staging** (automated, type: cli)
   - Executes: `psql -h staging-db -f migrations/20260418_add_user_preferences.sql`
   - Captures: stdout, stderr, execution time
   - Fails if: exit code != 0 (syntax error, constraint violation)

3. **Staging Validation Queries** (automated, type: cli)
   - Executes validation SQL:
     ```sql
     SELECT column_name FROM information_schema.columns 
     WHERE table_name = 'users' AND column_name = 'preferences';
     
     SELECT indexname FROM pg_indexes 
     WHERE tablename = 'users' AND indexname = 'idx_users_preferences';
     ```
   - Asserts: new column and index exist
   - Fails if: validation query returns no rows

4. **Estimate Production Migration Time** (automated, type: cli)
   - Executes: `EXPLAIN ANALYZE <migration_sql>` on production replica
   - Parses: estimated execution time from EXPLAIN output
   - Stores: estimated_duration (for approval prompt)

5. **DBA Approval: Proceed to Production** (human, type: approval)
   - Shows: migration SQL, dry-run results, estimated duration
   - Approver: dba-team-id
   - Question: "Approve production migration?"
   - Warning: "This will acquire table lock for ~<estimated_duration>"
   - Timeout: 8 hours (maintenance window expires)

6. **Schedule Maintenance Window** (human, type: collector)
   - Prompts DBA for:
     - Maintenance start time (datetime)
     - Expected duration (minutes)
     - Notification message (text)
   - Sends: maintenance notification to #engineering Slack channel
   - Stores: maintenance window metadata

7. **Wait for Maintenance Window** (automated, type: cli)
   - Sleeps until maintenance start time
   - Executes: `sleep $(( $(date -d '<maintenance_start>' +%s) - $(date +%s) ))`

8. **Take Pre-Migration Snapshot** (automated, type: cli)
   - Executes: `pg_dump -h prod-db -Fc -f /backup/pre-migration-$(date +%s).dump`
   - Verifies: dump file size > 0, no errors in pg_dump output
   - Stores: snapshot path for rollback
   - Fails if: insufficient disk space, dump fails

9. **Register Rollback Compensation** (automated, internal)
   - Registers: "Restore from snapshot and revert schema" if any subsequent step fails
   - Compensation steps:
     - Drop new column: `ALTER TABLE users DROP COLUMN preferences;`
     - Drop new index: `DROP INDEX idx_users_preferences;`
     - Restore from snapshot if DROP fails: `pg_restore`

10. **Execute Production Migration** (automated, type: cli, with retry)
    - Executes: `psql -h prod-db -f migrations/20260418_add_user_preferences.sql`
    - Timeout: 30 minutes (kill if exceeds)
    - Retries: 2 times with 5 minute backoff (handles transient deadlocks)
    - Captures: stdout, stderr, execution time
    - Fails if: exit code != 0, timeout, or max retries exceeded → triggers rollback

11. **Production Validation Queries** (automated, type: cli, retry loop)
    - Executes same validation SQL as step 3, but on production
    - Retries: 5 times with 10s backoff (handles replication lag)
    - Asserts: new column and index exist, no data corruption
    - Fails if: validation fails → triggers rollback

12. **Smoke Tests** (automated, type: cli, parallel)
    - Parallel tests:
      - Test 1: Insert new row with preferences column (type: cli)
      - Test 2: Query with new index (type: cli, check EXPLAIN uses index)
      - Test 3: Update existing row preferences column (type: cli)
      - Test 4: Verify foreign key constraints (type: cli)
    - All tests must pass
    - Fails if: any test fails → triggers rollback

13. **Application Health Check** (automated, type: cli, iterate)
    - Loop for 5 minutes (30 iterations, 10s interval):
      - Query: `curl https://api.company.com/health`
      - Check: response status 200, response time < 1s
    - Convergence: 10 consecutive healthy checks
    - Early exit: if any check fails → triggers rollback

14. **DBA Confirmation: Migration Successful** (human, type: approval)
    - Shows: migration execution time, validation results, smoke test results, health checks
    - Approver: dba-team-id
    - Question: "Confirm migration is stable?"
    - Timeout: 30 minutes → auto-rollback (DBA not monitoring)

15. **Clear Rollback Compensation** (automated, internal)
    - Unregisters rollback (migration confirmed successful)

16. **Post-Migration Cleanup** (automated, type: cli)
    - Executes: `VACUUM ANALYZE users;` (rebuild statistics)
    - Sends: maintenance complete notification to Slack
    - Records: migration in schema_versions table

17. **Delete Pre-Migration Snapshot** (automated, type: cli)
    - Deletes: `/backup/pre-migration-*.dump`
    - Only runs if step 14 approved (migration confirmed stable)

## Complexity Tags
- Linear with looping (retry on transient failures)
- Compensation/rollback (automatic revert on failure)
- Iterate (health check with convergence)

## Key Schema Challenges

1. **Time-based wait** — Step 7 sleeps until maintenance window. Schema must support
   datetime-based wait (not just duration), handling timezone, DST.

2. **Compensation with multi-step rollback** — Step 9 registers 3-step rollback: (1) DROP
   COLUMN, (2) DROP INDEX, (3) pg_restore if DROP fails. Schema must support multi-step
   compensation with fallback logic.

3. **Automatic rollback on approval timeout** — Step 14 timeout triggers rollback (not
   escalation). Schema must support timeout action: execute compensation vs. escalate vs. fail.

4. **Retry with backoff for transient failures** — Step 10 retries migration on deadlock.
   Schema must distinguish transient errors (retry) from permanent errors (fail immediately).

5. **Conditional cleanup** — Step 17 only deletes snapshot if step 14 approved. Schema must
   support conditional step execution based on earlier step outcome.

6. **Compensation scope** — Rollback compensation registered at step 9 applies to steps 10-14.
   Schema must define compensation scope (which steps are protected).

7. **Convergence-based iterate** — Step 13 loops until 10 consecutive healthy checks. Schema
   must support convergence condition (not just boolean exit condition).

8. **Evidence capture for audit** — All SQL queries, execution times, validation results must
   be captured for audit trail. Schema must support detailed evidence capture for CLI steps
   (stdout, stderr, exit code, duration, timestamp).

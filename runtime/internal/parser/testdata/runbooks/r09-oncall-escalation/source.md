# Runbook 9: On-Call Escalation Ladder

**Domain:** SRE/Operations  
**Complexity:** Looping (escalation retry) + Multi-party (escalation chain)  
**Interaction:** Automated escalation + human acknowledgment

## Summary

Escalates critical alerts through on-call chain until acknowledged. Tries primary on-call
(page + wait), then secondary, then manager, then director. Includes timeout at each level
and simultaneous notification to all if no acknowledgment after 30 minutes.

## Steps

1. **Receive Critical Alert** (automated, type: extension)
   - Triggered by: monitoring system (Datadog, PagerDuty) webhook
   - Collects: alert severity, affected service, alert description, timestamp
   - Only proceeds if: severity == "critical"
   - Stores: alert_id for correlation

2. **Lookup On-Call Schedule** (automated, type: extension)
   - Calls PagerDuty API: get current on-call schedule for service
   - Returns:
     - Primary on-call engineer
     - Secondary on-call engineer
     - Manager on-call
     - Director on-call
   - Stores: escalation_chain list
   - Fails if: no on-call schedule configured

3. **Escalation Loop: Try Primary** (automated, type: iterate)
   - Max passes: 3
   - Interval: 5 minutes
   - Step 3a: Page primary on-call (type: extension)
     - Calls PagerDuty API: create high-urgency incident assigned to primary
     - Sends: SMS, phone call, push notification
   - Step 3b: Wait for acknowledgment (type: extension)
     - Polls PagerDuty API: check incident status
     - Wait: 5 minutes
   - Convergence: incident status == "acknowledged"
   - Early exit if: incident acknowledged before max passes
   - If not acknowledged after 3 passes → proceed to step 4

4. **Escalation Loop: Try Secondary** (automated, type: iterate)
   - Max passes: 3
   - Interval: 5 minutes
   - Same pattern as step 3, but page secondary on-call
   - If not acknowledged after 3 passes → proceed to step 5

5. **Escalation Loop: Try Manager** (automated, type: iterate)
   - Max passes: 2
   - Interval: 5 minutes
   - Same pattern, but page manager on-call
   - If not acknowledged after 2 passes → proceed to step 6

6. **Escalation Loop: Try Director** (automated, type: iterate)
   - Max passes: 2
   - Interval: 5 minutes
   - Same pattern, but page director on-call
   - If not acknowledged after 2 passes → proceed to step 7

7. **Emergency: Broadcast to All** (automated, type: extension)
   - Sends: SMS + phone call + push notification to ALL engineers in escalation chain simultaneously
   - Creates: war room Zoom meeting, sends link
   - Sends: Slack message to #critical-alerts channel mentioning @here
   - Calls: backup escalation contact (VP Engineering or CTO)

8. **Wait for Any Acknowledgment** (automated, type: extension, polling loop)
   - Polls PagerDuty API every 1 minute
   - Wait: up to 60 minutes
   - Convergence: any engineer acknowledges incident
   - If no acknowledgment after 60 minutes → proceed to step 9

9. **Executive Escalation** (automated, type: extension)
   - Sends: SMS + phone call to CTO and CEO
   - Creates: critical incident record in incident management system
   - Triggers: automated failover (if configured for service)

10. **Incident Acknowledged** (automated, convergent from steps 3-9)
    - Records: who acknowledged, timestamp, escalation level reached
    - Sends: Slack message to #critical-alerts: "<engineer> acknowledged incident"
    - Creates: incident response Slack channel
    - Invokes: incident response runbook (separate runbook)

11. **Post-Incident Escalation Report** (automated, type: extension)
    - Generates: escalation timeline (who was paged when, who acknowledged)
    - Sends: report to engineering leadership
    - Flags: if escalation reached manager+ level (indicates on-call issue)

## Complexity Tags
- Looping (retry escalation at each level)
- Multi-party (escalation chain)
- Early exit (stop escalating once acknowledged)
- Automated (no human interaction, fully automated escalation)

## Key Schema Challenges

1. **Nested iterate loops** — Steps 3, 4, 5, 6 are four sequential iterate blocks, each with
   convergence condition (acknowledged). Schema must support sequential iterates with early exit
   (once acknowledged, skip remaining escalation levels).

2. **Polling with convergence** — Each iterate polls PagerDuty API to check acknowledgment
   status. Schema must support polling pattern: call API, check condition, wait, repeat.

3. **Dynamic escalation chain** — Escalation chain (primary, secondary, manager, director) is
   looked up at runtime from PagerDuty API. Schema must support dynamic iteration over list
   (not hardcoded escalation levels).

4. **Cross-iterate convergence** — Once any iterate (step 3, 4, 5, 6, or 8) converges,
   runbook proceeds to step 10 (incident acknowledged). Schema must support convergence point
   across multiple loops.

5. **Fully automated runbook** — No human approval or manual steps. Schema must support fully
   automated runbooks (common for incident response).

6. **Time-based escalation** — Escalation timing is critical (5 min per attempt, 3 attempts =
   15 min per level). Schema must enforce precise timing (not just approximate).

7. **Simultaneous notification** — Step 7 pages all engineers at once (not sequential).
   Different from parallel step execution (no wait-all, just send all notifications).

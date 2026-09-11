# Runbook 7: Financial Approval for Large Purchase

**Domain:** Finance  
**Complexity:** Multi-level approval chain with escalation  
**Interaction:** Human data collection + multi-party approval

## Summary

Handles approval workflow for large capital expenditure ($50k+). Includes requester data
collection, manager approval, finance approval, executive approval (based on amount), and
vendor notification.

## Steps

1. **Purchase Request Form** (human, type: collector)
   - Prompts requester for:
     - Item description (text, max 500 chars)
     - Vendor name (text)
     - Amount (currency, USD)
     - Business justification (multi-line text)
     - Budget line item (dropdown from finance system)
     - Requested delivery date (date)
     - Requester email (email)
     - Department (dropdown)
   - Validations:
     - Amount > $50,000 (else route to simplified approval)
     - Budget line item has sufficient funds
   - Stores: purchase_request object
   - Fails if: budget insufficient

2. **Manager Approval: Justify Business Need** (human, type: approval)
   - Shows: purchase_request summary
   - Approver: requester's manager (lookup from HR system using requester email)
   - Question: "Approve purchase as necessary for business?"
   - Timeout: 2 business days → escalate to director
   - Stores: manager approval timestamp and comments

3. **Finance Review: Budget Verification** (human, type: approval)
   - Shows: purchase_request, manager approval, current budget status
   - Approver: finance-team-id
   - Question: "Confirm budget availability and procurement policy compliance?"
   - Checklist:
     - [ ] Budget line item has sufficient funds
     - [ ] Vendor is on approved vendor list
     - [ ] Purchase complies with procurement policy
   - Timeout: 3 business days → escalate to finance director

4. **Approval Tier Decision** (automated, type: decision)
   - Evaluates purchase_request.amount:
     - If amount < $100k → route to "Director Approval"
     - If amount >= $100k AND < $500k → route to "VP Approval"
     - If amount >= $500k → route to "CFO Approval"

5. **Branch: Director Approval** (if $50k-$100k)
   - Step 5a: Approver: department director (lookup from org chart)
   - Timeout: 3 business days → escalate to VP

6. **Branch: VP Approval** (if $100k-$500k)
   - Step 6a: Approver: department VP (lookup from org chart)
   - Timeout: 5 business days → escalate to CFO

7. **Branch: CFO Approval** (if $500k+)
   - Step 7a: Approver: CFO
   - Timeout: 7 business days → escalate to CEO
   - Additional check: Board approval required if amount >= $1M
   - Step 7b: Board Approval (if amount >= $1M) (human, type: approval)
     - Approvers: board-members-id (quorum: 3 of 5 must approve)
     - Timeout: 14 business days → defer to next board meeting

8. **Procurement: Vendor Notification** (automated, type: extension)
   - Sends: purchase order to vendor via email
   - Includes: PO number, item description, amount, delivery date
   - Calls: procurement system API to create PO record
   - Stores: PO number

9. **Contract Execution** (human, type: manual)
   - Instructions: "Upload signed contract and vendor quote"
   - File uploads:
     - Signed contract (PDF)
     - Vendor quote (PDF)
   - Assignee: procurement-team-id
   - Timeout: 10 business days → escalate to procurement manager

10. **Finance: Record Purchase** (automated, type: extension)
    - Calls: finance system API to record transaction
    - Debits: budget line item
    - Creates: accounts payable entry
    - Stores: transaction ID

11. **Notify Requester** (automated, type: extension)
    - Sends: email to requester with PO number, expected delivery date
    - Includes: link to track purchase status

12. **Close Approval Workflow** (automated, type: extension)
    - Updates: workflow status to "approved"
    - Archives: all approvals and documents
    - Sends: Slack notification to finance team

## Complexity Tags
- Multi-level approval chain (manager → finance → director/VP/CFO/board)
- Branching (approval tier by amount)
- Multi-party with quorum (board approval requires 3 of 5)
- Long-running (spans weeks)

## Key Schema Challenges

1. **Dynamic approver lookup** — Step 2 approver is requester's manager (looked up from HR
   system). Step 5/6/7 approvers are director/VP/CFO (looked up from org chart). Schema must
   support dynamic approver resolution via external API or database query.

2. **Multi-level escalation chain** — Each approval has timeout → escalate to next level.
   Schema must support escalation hierarchy: manager → director → VP → CFO → CEO.

3. **Amount-based branching** — Step 4 routes based on numeric comparison (amount thresholds).
   Schema must support numeric comparison in decision logic.

4. **Quorum approval** — Step 7b requires 3 of 5 board members to approve. Schema must support
   M-of-N approval logic (not just all-must-approve or any-one).

5. **Conditional nested approval** — Step 7b (board) only runs if amount >= $1M. Schema must
   support conditional step within a branch.

6. **Budget validation before proceeding** — Step 1 checks budget availability. If
   insufficient, runbook should fail early. Schema must support pre-condition checks with abort.

7. **Business day timeout** — All timeouts use business days. Schema must support
   calendar-aware timeout (already noted in Runbook 3).

8. **Artifact collection from human** — Step 9 collects two file uploads (contract, quote).
   Schema must support file upload in manual steps with metadata.

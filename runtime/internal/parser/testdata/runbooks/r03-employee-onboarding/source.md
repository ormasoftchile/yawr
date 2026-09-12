# Runbook 3: New Employee Onboarding

**Domain:** HR/IT  
**Complexity:** Parallel (independent provisioning tasks) + Multi-party approval  
**Interaction:** Human data collection + multi-party approval

## Summary

Onboards new employee by provisioning accounts, devices, and access. Includes HR data
collection, manager approval, parallel IT provisioning tasks (Okta, GitHub, Slack, laptop),
and compliance attestation.

## Steps

1. **Collect Employee Information** (human, type: collector)
   - Prompts HR for:
     - Full name (text input)
     - Email (email validation)
     - Start date (date picker)
     - Department (dropdown: Engineering, Sales, Marketing, Finance)
     - Manager (autocomplete from employee directory)
     - Job title (text input)
     - Office location (dropdown: SF, NYC, London, Remote)
   - Stores: employee_data object
   - Fails if: required fields missing, email already exists in system

2. **Manager Approval: Confirm Hire** (type: approval)
   - Shows: employee_data summary
   - Approver: manager specified in step 1
   - Timeout: 2 business days → escalate to HR director
   - Stores: manager approval timestamp and signature

3. **Background Check Verification** (human, type: manual)
   - Instructions: "Verify background check status in HireRight portal"
   - Checklist:
     - [ ] Background check completed
     - [ ] No disqualifying issues
     - [ ] Uploaded background check report to employee folder
   - Assignee: HR coordinator
   - Timeout: 5 business days → escalate to HR manager
   - Stores: attestation that background check passed

4. **Parallel Provisioning Tasks** (automated + human, parallel fan-out)
   - All tasks execute concurrently, wait-all before proceeding

   **Task 4a: Create Okta Account** (type: extension)
   - Calls Okta API: create user with employee_data
   - Assigns groups based on department
   - Sends welcome email with temporary password
   - Fails if: Okta API error, email already exists

   **Task 4b: Create GitHub Account** (type: extension)
   - Calls GitHub API: invite user to organization
   - Adds to teams based on department (engineering only)
   - Fails if: GitHub API error, user already exists

   **Task 4c: Create Slack Account** (type: extension)
   - Calls Slack API: invite user to workspace
   - Adds to channels: #general, #<department>, #announcements
   - Fails if: Slack API error, email already in workspace

   **Task 4d: Order Laptop** (human, type: manual)
   - Instructions: "Order laptop in IT procurement system"
   - Checklist:
     - [ ] Selected laptop model (MacBook Pro for eng, MacBook Air for others)
     - [ ] Entered shipping address: <office_location> or <home_address_if_remote>
     - [ ] Estimated delivery date: <date>
   - Assignee: IT procurement team
   - Timeout: 3 business days → escalate to IT manager

   **Task 4e: Schedule IT Onboarding Call** (human, type: manual)
   - Instructions: "Schedule 30-minute IT onboarding call on employee's first day"
   - Checklist:
     - [ ] Calendar invite sent to employee
     - [ ] Zoom link included
   - Assignee: IT onboarding coordinator

5. **Wait for All Provisioning** (automated, join point)
   - Waits for tasks 4a-4e to complete
   - Fails if: any task fails (abort onboarding)

6. **Security Training** (human, type: approval)
   - Instructions: "Employee must complete security training in LMS"
   - Checklist:
     - [ ] Completed security awareness training
     - [ ] Completed phishing simulation
     - [ ] Signed acceptable use policy
   - Assignee: employee (self-service)
   - Timeout: 7 business days → blocks access to sensitive systems
   - Stores: training completion date and certificate

7. **Access Provisioning** (automated, type: extension)
   - Reads department from employee_data
   - Provisions access based on role:
     - Engineering: GitHub repos, AWS dev account, Datadog
     - Sales: Salesforce, HubSpot
     - Marketing: Google Ads, Mailchimp
     - Finance: NetSuite, Bill.com
   - Calls respective APIs for each system
   - Fails if: any API call fails → retry 3 times with 1 minute backoff

8. **Manager Confirmation: Access Provisioned** (human, type: approval)
   - Shows: list of provisioned accounts and access levels
   - Approver: manager
   - Question: "Confirm access is appropriate for employee's role?"
   - Timeout: 1 business day → auto-approve (assume correct)

9. **Welcome Email** (automated, type: extension)
   - Sends personalized welcome email to employee
   - Includes: credentials, first-day instructions, org chart, manager contact
   - Stores: email sent timestamp

10. **Compliance Attestation** (human, type: approval)
    - Approvers: HR director AND IT security officer (both must approve)
    - Question: "Attest that onboarding completed per company policy?"
    - Shows: summary of all completed steps
    - Timeout: 2 business days → flag for compliance review
    - Stores: dual signatures

11. **Close Onboarding Ticket** (automated, type: extension)
    - Updates HR system: mark employee as onboarded
    - Closes Jira ticket
    - Sends Slack notification to HR and IT: "Onboarding complete for <name>"

## Complexity Tags
- Parallel (5 concurrent provisioning tasks)
- Multi-party (manager approval, dual attestation, employee self-service)
- Long-running (spans multiple days with timeout escalations)

## Key Schema Challenges

1. **Complex data collection form** — Step 1 collects 7 fields with different input types
   (text, email, date, dropdown, autocomplete). Schema must support rich input validation
   (email format, date ranges, dropdown options from external API).

2. **Parallel fan-out with heterogeneous tasks** — Step 4 runs 3 automated API calls + 2 human
   tasks concurrently. Schema must support parallel execution with mixed step types
   (extension + manual) and wait-all synchronization.

3. **Business day timeout calculations** — Steps 2, 3, 4d, 6, 8, 10 use business days for
   timeouts (not wall-clock time). Schema must support calendar-aware timeout calculation
   (skip weekends, holidays).

4. **Multi-party approval with AND logic** — Step 10 requires TWO approvers (HR director AND
   IT security officer). Schema must support M-of-N approval (both must approve, vs. any-one,
   vs. quorum).

5. **Conditional access provisioning based on data** — Step 7 provisions different systems based
   on department field from step 1. Schema must support conditional logic (if department ==
   "Engineering" then provision AWS) within a single step or as dynamic branch selection.

6. **Escalation chains** — Steps 2, 3, 4d, 6 have escalation on timeout (escalate to different
   approver, not fail). Schema must support escalation policy: primary approver, escalate to
   secondary after timeout.

7. **Self-service human step** — Step 6 is assigned to the employee (subject of the runbook),
   not the operator. Schema must support dynamic assignee (use value from step 1 email field).

8. **Failure handling for partial provisioning** — If task 4c (Slack) fails but 4a/4b/4d/4e
   succeed, what happens? Full rollback (delete Okta, GitHub accounts) or partial completion?
   Schema must define failure handling strategy for parallel blocks.

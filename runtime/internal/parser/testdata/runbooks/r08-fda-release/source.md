# Runbook 8: Medical Device Software Release (FDA/Quality)

**Domain:** Regulated Industry (Medical Device)  
**Complexity:** Linear + Multi-party attestation + Extensive evidence  
**Interaction:** Human attestation + automated validation

## Summary

Releases software update for FDA-regulated Class II medical device. Includes design
verification, risk analysis, clinical validation, quality assurance attestation, regulatory
documentation, and FDA submission preparation. Highly regulated with extensive evidence capture.

## Steps

1. **Design Verification Review** (human, type: approval)
   - Shows: design verification report (DVR) for software changes
   - Approver: quality-assurance-id
   - Question: "Attest that design verification testing is complete per IEC 62304 Level C?"
   - Checklist:
     - [ ] Unit tests passed (>95% coverage)
     - [ ] Integration tests passed
     - [ ] System tests passed
     - [ ] Traceability matrix updated (requirements → tests)
     - [ ] Test results uploaded to QMS
   - Timeout: 5 business days → escalate to QA manager
   - Stores: QA attestation signature

2. **Risk Analysis Verification** (human, type: approval)
   - Shows: risk analysis document (ISO 14971)
   - Approver: risk-management-id
   - Question: "Attest that risk analysis is complete and all risks are acceptable?"
   - Checklist:
     - [ ] Hazard analysis completed
     - [ ] Risk control measures implemented
     - [ ] Residual risks evaluated and accepted
     - [ ] Risk management file updated
   - Timeout: 5 business days → escalate to risk manager

3. **Clinical Validation** (human, type: approval)
   - Shows: clinical validation summary
   - Approver: clinical-affairs-id
   - Question: "Attest that clinical validation demonstrates safety and efficacy?"
   - Checklist:
     - [ ] Clinical study completed (if required)
     - [ ] Clinical data analyzed
     - [ ] No adverse events reported
     - [ ] Clinical validation report uploaded to QMS
   - Only required if: software changes affect clinical functionality
   - Timeout: 10 business days → escalate to clinical manager

4. **Cybersecurity Assessment** (human, type: approval)
   - Shows: cybersecurity assessment report
   - Approver: security-team-id
   - Question: "Attest that cybersecurity risks are mitigated per FDA guidance?"
   - Checklist:
     - [ ] Threat modeling completed
     - [ ] Vulnerability scanning passed
     - [ ] Penetration testing completed
     - [ ] Security controls implemented
     - [ ] Cybersecurity bill of materials (CBOM) generated
   - Timeout: 5 business days → escalate to security manager

5. **Regulatory Affairs Review** (human, type: approval)
   - Shows: software version, changes summary, validation summary
   - Approver: regulatory-affairs-id
   - Question: "Determine regulatory submission requirement:"
   - Options:
     - **No submission** — Minor software change, exempt per FDA guidance
     - **Letter to File** — Moderate change, document in DHF
     - **Special 510(k)** — Significant change, submit to FDA
   - Stores: submission determination and rationale

6. **Branch by Submission Type** (automated, type: decision)
   - Routes based on step 5 choice:
     - If "No submission" → skip to step 10
     - If "Letter to File" → step 7
     - If "Special 510(k)" → step 8

7. **Branch: Letter to File** (if moderate change)
   - Step 7a: Generate letter to file (automated, type: extension)
     - Template: software change summary, verification results, risk analysis
   - Step 7b: Upload to QMS (human, type: manual)
     - Instructions: "Upload letter to file to document control system"
   - Step 7c: Quality Manager signature (human, type: approval)
     - Approver: quality-manager-id

8. **Branch: Special 510(k) Submission** (if significant change)
   - Step 8a: Prepare 510(k) submission package (human, type: manual)
     - Instructions: "Compile 510(k) submission per FDA template"
     - File uploads:
       - Software description
       - Verification and validation report
       - Risk analysis
       - Cybersecurity documentation
       - Labeling
     - Assignee: regulatory-affairs-id
     - Timeout: 30 business days
   - Step 8b: Quality Manager review (human, type: approval)
     - Approver: quality-manager-id
   - Step 8c: CEO signature (human, type: approval)
     - Approver: CEO (legally responsible party)
   - Step 8d: Submit to FDA (automated, type: extension)
     - Uploads: 510(k) package to FDA eSTAR portal
     - Stores: FDA submission ID
   - Step 8e: Wait for FDA clearance (human, type: manual)
     - Instructions: "Monitor FDA review status, respond to questions"
     - Timeout: 90 days (FDA statutory review period)
     - Note: This step may pause runbook for months

9. **Document Control** (automated, type: extension)
   - Updates: document management system with new software version
   - Archives: design history file (DHF) documents
   - Generates: device history record (DHR) entry

10. **Manufacturing Release Approval** (human, type: approval)
    - Shows: summary of all attestations, submission status
    - Approvers: quality-manager-id AND ceo-id (both must approve)
    - Question: "Approve release to manufacturing?"
    - Stores: dual signatures

11. **Sign Software Bill of Materials** (automated, type: cli)
    - Generates: SBOM (SPDX format) for software release
    - Signs: SBOM with company code signing certificate
    - Uploads: signed SBOM to artifact repository

12. **Build Release Artifact** (automated, type: cli)
    - Executes: `./build-release.sh <version>`
    - Generates: signed binary, release notes, installation instructions
    - Computes: SHA256 hash of release artifact
    - Uploads: to secure artifact repository

13. **Release to Manufacturing** (automated, type: extension)
    - Notifies: manufacturing team via email
    - Creates: Jira ticket for manufacturing process
    - Sends: release package to manufacturing file share

14. **Post-Market Surveillance Setup** (automated, type: extension)
    - Configures: device telemetry monitoring for new version
    - Sets: alert rules for adverse events
    - Creates: post-market surveillance dashboard

15. **Close Release Record** (automated, type: extension)
    - Records: release in product lifecycle management (PLM) system
    - Stores: all attestation signatures, submission documents, evidence
    - Generates: release certificate (PDF with all signatures)
    - Archives: to QMS with 10-year retention

## Complexity Tags
- Linear with branching (submission type)
- Multi-party attestation (5+ approvers across quality, risk, clinical, security, regulatory, CEO)
- Extensive evidence capture (all documents, signatures, test results)
- Long-running (may pause for FDA review, 90+ days)

## Key Schema Challenges

1. **Conditional step execution based on device risk** — Step 3 (clinical validation) only
   required if software affects clinical functionality. Schema must support step-level
   conditional execution with explanation for audit trail.

2. **Human choice with regulatory implications** — Step 5 decision determines regulatory path.
   Choice must be recorded with rationale for FDA audit. Schema must support choice steps with
   mandatory rationale field.

3. **Long pause for external process** — Step 8e waits for FDA clearance (may take 90+ days).
   Runbook must pause and resume months later. Schema must support long-running pauses with
   external triggers (webhook from FDA portal).

4. **Extensive signature capture** — 7+ approvals with digital signatures for 21 CFR Part 11
   compliance. Schema must support signature metadata: signer identity, timestamp, signature
   algorithm, certificate thumbprint.

5. **Document versioning and archival** — All documents (DVR, risk analysis, 510(k)) must be
   versioned and archived for 10 years. Schema must support artifact versioning and retention
   policies.

6. **Cryptographic signing of release artifact** — Step 11 signs SBOM with code signing
   certificate. Schema must support cryptographic operations with private key management.

7. **Audit trail for entire process** — FDA requires complete audit trail: who did what, when,
   with what evidence. Schema must capture detailed provenance for every step, approval, and
   artifact.

8. **Quorum approval for manufacturing release** — Step 10 requires quality manager AND CEO
   (both must approve, not either). Same as Runbook 7 board approval.

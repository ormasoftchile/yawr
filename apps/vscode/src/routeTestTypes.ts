export interface RouteTestSelector {
  call_path?: string[];
  step: string;
  phase: 'before' | 'execute';
  invocation: number;
  attempt: number;
}

export interface RouteTestReview {
  state: 'draft' | 'reviewed';
  reviewed_by?: string;
  reviewed_at?: string;
  sensitivity_reviewed: boolean;
}

export interface RouteTestSource {
  kind: 'manual' | 'prior-run';
  run_id?: string;
  interaction_id?: string;
  answer_digest?: string;
  copied_at?: string;
}

export interface RouteTestStepResponse {
  at: RouteTestSelector;
  kind: 'cli' | 'tool';
  status: string;
  outcome?: string;
  output?: Record<string, unknown>;
  source: RouteTestSource;
  review: RouteTestReview;
}

export interface RouteTestHostActionResponse {
  at: RouteTestSelector;
  capability?: string;
  response: { status: string; result?: Record<string, unknown> };
  source: RouteTestSource;
  review: RouteTestReview;
}

export interface RouteTestInteractionAnswer {
  at: RouteTestSelector;
  kind: 'choice' | 'decision' | 'collector';
  selected?: string[];
  label?: string;
  values?: Record<string, unknown>;
  source: RouteTestSource;
  review: RouteTestReview;
}

export interface RouteTestApproval {
  at: RouteTestSelector;
  approved: boolean;
  approver: string;
  source: RouteTestSource;
  review: RouteTestReview;
}

export interface RouteTestArtifact {
  apiVersion: 'yawr.route-test/v1';
  id: string;
  name: string;
  runbook: string;
  plan_hash: string;
  sensitivity_reviewed: true;
  target: RouteTestSelector;
  inputs?: Record<string, string>;
  step_responses?: RouteTestStepResponse[];
  host_action_responses?: RouteTestHostActionResponse[];
  interaction_answers?: RouteTestInteractionAnswer[];
  test_approvals?: RouteTestApproval[];
  last_result?: {
    status: 'reached' | 'route-changed' | 'runtime-failed' | 'safety-failed' | 'stopped';
    target_reached: boolean;
    external_dispatches: number;
    ran_at: string;
    conditions_digest: string;
  };
}

export interface SavedRouteTest {
  filePath: string;
  artifact: RouteTestArtifact;
}
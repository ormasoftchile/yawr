export interface NamedRunResults {
  schema_version: 'yawr.run-results/v1';
  publication_id: string;
  plan_snapshot_digest: string;
  checkpoint_sequence: number;
  origin: { node_id: string; frame_id?: string; invocation: number };
  outputs: Record<string, { type: string; value: unknown }>;
  digest: string;
}
export type ResultsAvailability =
  | { state: 'available'; publication: NamedRunResults; canonicalJSON?: string }
  | { state: 'unavailable'; reason: string; status?: 'unavailable' | 'redacted' };

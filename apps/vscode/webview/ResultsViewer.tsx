import React from 'react';
import type { ResultsAvailability } from '../src/typedResultsTypes';
import { canonicalResultsJSON } from '../src/typedResultsCanonical';

export function ResultsViewer({ results, nodeID }: { results?: ResultsAvailability; nodeID: string }) {
  if (!results || results.state === 'unavailable') {
    return <section className="typed-results" aria-label="Results" data-results-state="unavailable">
      <h2>Results</h2><p role="status">Full structured result unavailable
        {results?.state === 'unavailable' ? ` — ${results.reason}` : ' — not published'}.</p>
      <p>Execution status is unchanged. A preview is not a complete result.</p>
    </section>;
  }
  // The canonical string survives VS Code's JSON bridge without changing -0.
  const publication: typeof results.publication = results.canonicalJSON ? JSON.parse(results.canonicalJSON) : results.publication;
  if (publication.origin.node_id !== nodeID) {
    return <section className="typed-results" data-results-state="unavailable">
      <h2>Results</h2><p>This scope has no retrieved public publication. Select the root Results operation for its named outputs.</p>
    </section>;
  }
  let rendered: Array<{ name: string; type: string; text: string }>;
  try {
    rendered = Object.entries(publication.outputs).map(([name, output]) =>
      ({ name, type: output.type, text: canonicalResultsJSON(output.value, true) }));
  } catch {
    return <section className="typed-results" data-results-state="unavailable">
      <h2>Results</h2><p>Full structured result unavailable — exceeds the viewer budget. Execution status is unchanged; no rerun was requested.</p>
    </section>;
  }
  return <section className="typed-results" aria-label="Results" data-results-state="available">
    <h2>Results</h2><p>Full structured JSON · verified canonical publication</p>
    <dl><dt>Publication</dt><dd><code>{publication.publication_id}</code></dd>
      <dt>Digest</dt><dd><code>{publication.digest}</code></dd></dl>
    {rendered.map(({ name, type, text }) =>
      <section key={name} data-result-name={name}><h3>{name} <small>({type})</small></h3>
        <pre className="json-block">{text}</pre>
      </section>)}
  </section>;
}

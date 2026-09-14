import React, { useEffect, useState } from 'react';
import { LocateFixed } from 'lucide-react';
import { isExecutionEnded, type CurrentActivity as Activity } from '../src/executionProgress';

export function CurrentActivity({ activities, runStatus, remaining, onLocate, locationNotice }: {
  activities: Activity[]; runStatus: string; remaining: number;
  onLocate: (activity: Activity) => void; locationNotice?: string;
}) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    if (!activities.some(value => value.lastActivityAt || value.startedAt)) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [activities]);
  const terminal = isExecutionEnded(runStatus);
  const leafCount = activities.filter(value => !value.container).length;
  const containerCount = activities.length - leafCount;
  const message = terminal ? remaining > 0 ? 'Run ended, some steps lack final status.' : `Run ${runStatus}.`
    : runStatus === 'idle' ? 'Not started.'
    : ['paused', 'paused_at_boundary', 'handoff_pending'].includes(runStatus) ? 'Paused — waiting for a resume command.'
    : runStatus === 'starting' ? 'Starting — waiting for the first step event.'
    : 'Waiting for runtime activity — no active step reported.';
  return <details className="current-activity">
    <summary>Execution activity ({activities.length})</summary>
    <section aria-label="Current activity">
    <div>
      <span>Current activity</span>
      <strong role="status">{activities.length
        ? `${leafCount} active runtime ${leafCount === 1 ? 'step' : 'steps'}${containerCount ? ` · ${containerCount} active ${containerCount === 1 ? 'container' : 'containers'}` : ''}`
        : message}</strong>
      {activities.slice(0, 4).map(activity => {
        const timestamp = activity.lastActivityAt || activity.startedAt;
        const time = timestamp ? Date.parse(timestamp) : NaN;
        return <div className="current-activity-item" key={`${activity.nodeID}:${activity.occurrenceID ?? ''}`}>
          <div>
            <strong>{activity.label}: {activity.title}</strong>
            <code title={activity.path}>{activity.path}</code>
            {activity.runID ? <small>Run {activity.runID}
              {activity.invocation !== undefined ? ` · invocation ${activity.invocation}` : ''}
              {activity.retryAttempt !== undefined ? ` · retry ${activity.retryAttempt}` : ''}
              {activity.executionLane ? ` · lane ${activity.executionLane}` : ''}</small> : null}
            {Number.isFinite(time) ? <small title={timestamp}>Last update {Math.max(0, Math.floor((now - time) / 1000))}s ago</small> : null}
            {!activity.inGraph ? <small>Executing outside the loaded graph — exact runtime path shown.</small> : null}
          </div>
          <button type="button" onClick={() => onLocate(activity)} title={`Locate ${activity.path}`}>
            <LocateFixed aria-hidden="true" /><span>Locate</span>
          </button>
        </div>;
      })}
      {activities.length > 4 ? <small>+{activities.length - 4} other active runtime locations</small> : null}
      {locationNotice ? <p role="status">{locationNotice}</p> : null}
    </div>
    </section>
  </details>;
}

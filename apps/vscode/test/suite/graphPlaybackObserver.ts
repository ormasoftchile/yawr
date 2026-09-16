export interface GraphPlaybackSample {
  at: number;
  ids: string[];
  progress: string[];
  status?: string;
  results?: string;
  documentID?: string;
  graphTitle?: string;
  visibility?: string;
  nodes?: string[];
  runbooks?: string[];
  history?: string[];
  historyIDs?: string[];
}

export async function connectGraphObserver(port: string) {
  const targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json() as Array<{
    type: string; webSocketDebuggerUrl?: string;
  }>;
  const target = targets.find(value => value.type === 'page' && value.webSocketDebuggerUrl);
  if (!target?.webSocketDebuggerUrl) throw new Error('Isolated editor CDP target is unavailable');
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise<void>((resolve, reject) => {
    socket.onopen = () => resolve();
    socket.onerror = () => reject(new Error('Cannot connect to isolated editor CDP'));
  });
  let sequence = 0;
  const pending = new Map<number, { resolve(value: unknown): void; reject(error: Error): void; timer: NodeJS.Timeout }>();
  const contexts = new Map<string, { id: number; sessionId?: string; observation?: string }>();
  const samples: GraphPlaybackSample[] = [];
  const errors: Error[] = [];
  let observedTitle: string | undefined;
  let observedDocumentID: string | undefined;
  let observing = false;
  let firstCurrent: (() => void) | undefined;
  let playbackChanged: (() => void) | undefined;
  let latestObserved: GraphPlaybackSample | undefined;
  const hasDrained = () => latestObserved?.ids.length === 0 && latestObserved.status === 'completed';
  const binding = '__yawrReadOnlyGraphPlaybackSample';
  const observationKey = `${binding}Observer`;
  const installObservation = `(() => {
    if (window.${observationKey}) return window.${observationKey};
    const sample = () => {
      const app = document.querySelector('.app[data-run-status]');
      if (!app) return;
      ${binding}(JSON.stringify({
        at: performance.timeOrigin + performance.now(),
        ids: Array.from(document.querySelectorAll('.step-node.execution-current')).map(node => node.closest('[data-id]').dataset.id),
        progress: Array.from(document.querySelectorAll('.step-node.execution-progress')).map(node => node.closest('[data-id]').dataset.id),
        status: app.dataset.runStatus, results: app.dataset.resultsState,
        graphTitle: document.querySelector('.identity strong')?.textContent,
        visibility: document.visibilityState,
        nodes: Array.from(document.querySelectorAll('.react-flow__node-yawrStep')).map(node => node.dataset.id),
        runbooks: Array.from(document.querySelectorAll('select[aria-label="Inspect runbook"] option')).slice(1).map(node => node.value),
        history: Array.from(document.querySelectorAll('select[aria-label="Inspect executed step"] option')).slice(1).map(node => node.textContent),
        historyIDs: Array.from(document.querySelectorAll('select[aria-label="Inspect executed step"] option')).slice(1).map(node => node.dataset.occurrenceId)
      }));
    };
    sample();
    const observer = new MutationObserver(sample);
    observer.observe(document, {
      subtree: true, childList: true, attributes: true,
      attributeFilter: ['class', 'data-run-status', 'data-results-state']
    });
    return window.${observationKey} = { timer: setInterval(sample, 10), observer };
  })()`;
  const scripts = new Map<string | undefined, string>();
  const retiredContext = (error: Error) => /Execution context was destroyed|Cannot find context|Session with given id not found/.test(error.message);
  function send<T>(method: string, params: object = {}, sessionId?: string): Promise<T> {
    return new Promise((resolve, reject) => {
      const id = ++sequence;
      const timer = setTimeout(() => {
        pending.delete(id); reject(new Error(`CDP ${method} timed out`));
      }, 20_000);
      pending.set(id, { resolve: value => resolve(value as T), reject, timer });
      socket.send(JSON.stringify({ id, method, params, sessionId }));
    });
  }
  socket.onmessage = event => {
    const message = JSON.parse(String(event.data));
    if (message.id) {
      const request = pending.get(message.id);
      if (!request) return;
      pending.delete(message.id); clearTimeout(request.timer);
      if (message.error) request.reject(new Error(message.error.message));
      else request.resolve(message.result);
    } else if (message.method === 'Target.attachedToTarget') {
      if (!['page', 'iframe'].includes(message.params.targetInfo.type)) return;
      const sessionId = message.params.sessionId;
      void send('Runtime.addBinding', { name: binding }, sessionId)
        .then(() => installForNewDocuments(sessionId))
        .then(() => send('Runtime.enable', {}, sessionId))
        .then(() => send('Target.setAutoAttach', { autoAttach: true, waitForDebuggerOnStart: false, flatten: true }, sessionId))
        .catch(error => { if (!retiredContext(error)) errors.push(error); });
    } else if (message.method === 'Runtime.executionContextCreated' && message.params.context.auxData?.isDefault) {
      const context: { id: number; sessionId?: string; observation?: string } =
        { id: message.params.context.id, sessionId: message.sessionId };
      contexts.set(`${context.sessionId ?? ''}:${context.id}`, context);
      void send<{ result: { objectId?: string }; exceptionDetails?: unknown }>('Runtime.evaluate', {
        contextId: context.id, returnByValue: false,
        expression: installObservation,
      }, context.sessionId).then(result => {
        if (result.exceptionDetails) errors.push(new Error(`Graph observer script failed: ${JSON.stringify(result.exceptionDetails)}`));
        context.observation = result.result.objectId;
      }).catch(error => { if (!retiredContext(error)) errors.push(error); });
    } else if (message.method === 'Runtime.bindingCalled' && message.params.name === binding) {
      const sample: GraphPlaybackSample = JSON.parse(message.params.payload);
      sample.documentID = `${message.sessionId ?? 'root'}:${message.params.executionContextId}`;
      if (observing && sample.graphTitle === observedTitle && sample.ids.length > 0 && !observedDocumentID) {
        observedDocumentID = sample.documentID;
        firstCurrent?.();
      }
      samples.push(sample);
      if (sample.documentID === observedDocumentID) latestObserved = sample;
      playbackChanged?.();
    } else if (message.method === 'Runtime.executionContextDestroyed') {
      if (observing && observedDocumentID === `${message.sessionId ?? 'root'}:${message.params.executionContextId}`) {
        errors.push(new Error('The observed graph document was destroyed during playback'));
      }
      contexts.delete(`${message.sessionId ?? ''}:${message.params.executionContextId}`);
    } else if (message.method === 'Runtime.executionContextsCleared' || message.method === 'Target.detachedFromTarget') {
      const sessionId = message.method === 'Target.detachedFromTarget' ? message.params.sessionId : message.sessionId;
      if (observing && observedDocumentID?.startsWith(`${sessionId ?? 'root'}:`)) {
        errors.push(new Error('The observed graph target was replaced during playback'));
      }
      for (const [key, context] of contexts) if (context.sessionId === sessionId) contexts.delete(key);
    }
  };
  async function installForNewDocuments(sessionId?: string) {
    const { identifier } = await send<{ identifier: string }>('Page.addScriptToEvaluateOnNewDocument', {
      source: installObservation,
    }, sessionId);
    scripts.set(sessionId, identifier);
  }
  async function describeContexts() {
    return Promise.all([...contexts.values()].map(async context => {
      try {
        const result = await send<{ result: { value?: unknown } }>('Runtime.evaluate', {
          contextId: context.id, returnByValue: true,
          expression: `({
            url: document.URL, ready: document.readyState, visibility: document.visibilityState,
            observer: !!window.${observationKey},
            app: document.querySelector('.app')?.outerHTML.slice(0, 500),
            body: document.body?.innerText.slice(0, 500),
            frames: Array.from(document.querySelectorAll('iframe')).map(frame => frame.src),
            scripts: Array.from(document.scripts).map(script => script.src)
          })`,
        }, context.sessionId);
        return { context: `${context.sessionId ?? 'root'}:${context.id}`, document: result.result.value };
      } catch (error) {
        return { context: `${context.sessionId ?? 'root'}:${context.id}`, error: String(error) };
      }
    }));
  }
  await send('Runtime.addBinding', { name: binding });
  await installForNewDocuments();
  await send('Runtime.enable');
  await send('Target.setAutoAttach', { autoAttach: true, waitForDebuggerOnStart: false, flatten: true });
  return {
    async waitForGraph(title: string) {
      const deadline = Date.now() + 20_000;
      while (!errors.length && Date.now() < deadline) {
        const ready = samples.slice().reverse().find(sample => sample.graphTitle === title && sample.status === 'idle' &&
          sample.visibility === 'visible' && sample.nodes?.length && sample.ids.length === 0);
        if (ready) {
          observedTitle = title;
          break;
        }
        await new Promise(resolve => setTimeout(resolve, 20));
      }
      if (errors.length) throw errors[0];
      if (!observedTitle) throw new Error(`Installed graph "${title}" was not ready before execution`);
    },
    async observe(durationMs = 8000): Promise<GraphPlaybackSample[]> {
      if (!observedTitle) throw new Error('Select the ready graph before observing execution');
      const start = samples.length;
      observedDocumentID = undefined;
      latestObserved = undefined;
      observing = true;
      try {
        await new Promise<void>((resolve, reject) => {
          const timer = setTimeout(() => {
            firstCurrent = undefined;
            const seen = samples.slice(start).map(sample =>
              `${sample.documentID}: ${sample.graphTitle}, ${sample.status}, ${sample.ids.join(',')}`);
            void describeContexts().then(contexts => reject(new Error(`Installed graph never displayed CURRENT: ${JSON.stringify({
              samples: [...new Set(seen)], contexts, errors: errors.map(error => error.message),
            })}`)));
          }, 20_000);
          firstCurrent = () => {
            clearTimeout(timer);
            firstCurrent = undefined;
            resolve();
          };
        });
        await new Promise(resolve => setTimeout(resolve, durationMs));
        if (errors.length) throw errors[0];
        if (!hasDrained()) {
          await new Promise<void>((resolve, reject) => {
            const timer = setTimeout(() => {
              playbackChanged = undefined;
              reject(new Error(`Installed graph playback did not drain: ${JSON.stringify(latestObserved)}`));
            }, 20_000);
            playbackChanged = () => {
              if (!hasDrained()) return;
              clearTimeout(timer);
              playbackChanged = undefined;
              resolve();
            };
          });
          await new Promise(resolve => setTimeout(resolve, 1000));
        }
      } finally {
        observing = false;
        playbackChanged = undefined;
      }
      if (errors.length) throw errors[0];
      // Run Current Runbook can replace the idle preview before execution.
      // Anchor to the first CURRENT, retaining any competing CURRENT stream.
      return samples.slice(start).filter(sample => sample.graphTitle === observedTitle &&
        (!observedDocumentID || sample.documentID === observedDocumentID || sample.ids.length > 0));
    },
    async close() {
      try {
        for (const [sessionId, identifier] of scripts) {
          try {
            await send('Page.removeScriptToEvaluateOnNewDocument', { identifier }, sessionId);
          } catch (error) {
            if (!(error instanceof Error) || !retiredContext(error)) throw error;
          }
        }
        for (const context of contexts.values()) {
          if (!context.observation) continue;
          try {
            await send('Runtime.callFunctionOn', {
              objectId: context.observation,
              functionDeclaration: `function() { clearInterval(this.timer); this.observer.disconnect(); delete window.${observationKey}; }`,
            }, context.sessionId);
            await send('Runtime.releaseObject', { objectId: context.observation }, context.sessionId);
          } catch (error) {
            if (!(error instanceof Error) || !retiredContext(error)) throw error;
          }
        }
      } finally {
        socket.close();
      }
    },
  };
}

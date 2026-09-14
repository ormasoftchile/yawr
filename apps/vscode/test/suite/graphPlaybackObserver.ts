export interface GraphPlaybackSample {
  at: number;
  ids: string[];
  progress: string[];
  status?: string;
  results?: string;
  documentID?: string;
  graphTitle?: string;
  visibility?: string;
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
  const binding = '__yawrReadOnlyGraphPlaybackSample';
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
      const sessionId = message.params.sessionId;
      void send('Runtime.addBinding', { name: binding }, sessionId)
        .then(() => send('Runtime.enable', {}, sessionId))
        .then(() => send('Target.setAutoAttach', { autoAttach: true, waitForDebuggerOnStart: false, flatten: true }, sessionId))
        .catch(error => { if (!retiredContext(error)) errors.push(error); });
    } else if (message.method === 'Runtime.executionContextCreated' && message.params.context.auxData?.isDefault) {
      const context: { id: number; sessionId?: string; observation?: string } =
        { id: message.params.context.id, sessionId: message.sessionId };
      contexts.set(`${context.sessionId ?? ''}:${context.id}`, context);
      void send<{ result: { objectId?: string }; exceptionDetails?: unknown }>('Runtime.evaluate', {
        contextId: context.id, returnByValue: false,
        expression: `(() => {
          const sample = () => {
            const app = document.querySelector('.app[data-run-status]');
            if (!app) return;
            ${binding}(JSON.stringify({
              at: performance.timeOrigin + performance.now(),
              ids: Array.from(document.querySelectorAll('.step-node.execution-current')).map(node => node.closest('[data-id]').dataset.id),
              progress: Array.from(document.querySelectorAll('.step-node.execution-progress')).map(node => node.closest('[data-id]').dataset.id),
              status: app.dataset.runStatus, results: app.dataset.resultsState,
              documentID: ${JSON.stringify(`${context.sessionId ?? 'root'}:${context.id}`)},
              graphTitle: document.querySelector('.identity strong')?.textContent,
              visibility: document.visibilityState
            }));
          };
          sample();
          const observer = new MutationObserver(sample);
          observer.observe(document, {
            subtree: true, childList: true, attributes: true,
            attributeFilter: ['class', 'data-run-status', 'data-results-state']
          });
          return { timer: setInterval(sample, 10), observer };
        })()`,
      }, context.sessionId).then(result => {
        if (result.exceptionDetails) errors.push(new Error(`Graph observer script failed: ${JSON.stringify(result.exceptionDetails)}`));
        context.observation = result.result.objectId;
      }).catch(error => { if (!retiredContext(error)) errors.push(error); });
    } else if (message.method === 'Runtime.bindingCalled' && message.params.name === binding) {
      samples.push(JSON.parse(message.params.payload));
    } else if (message.method === 'Runtime.executionContextDestroyed') {
      contexts.delete(`${message.sessionId ?? ''}:${message.params.executionContextId}`);
    } else if (message.method === 'Runtime.executionContextsCleared' || message.method === 'Target.detachedFromTarget') {
      const sessionId = message.method === 'Target.detachedFromTarget' ? message.params.sessionId : message.sessionId;
      for (const [key, context] of contexts) if (context.sessionId === sessionId) contexts.delete(key);
    }
  };
  await send('Runtime.addBinding', { name: binding });
  await send('Runtime.enable');
  await send('Target.setAutoAttach', { autoAttach: true, waitForDebuggerOnStart: false, flatten: true });
  return {
    async waitForGraph() {
      const deadline = Date.now() + 20_000;
      while (!samples.length && !errors.length && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 20));
      if (errors.length) throw errors[0];
      if (!samples.length) throw new Error('Installed graph was not observed before execution');
    },
    async observe(): Promise<GraphPlaybackSample[]> {
      await new Promise(resolve => setTimeout(resolve, 8000));
      if (errors.length) throw errors[0];
      return samples.slice();
    },
    async close() {
      try {
        for (const context of contexts.values()) {
          if (!context.observation) continue;
          try {
            await send('Runtime.callFunctionOn', {
              objectId: context.observation,
              functionDeclaration: 'function() { clearInterval(this.timer); this.observer.disconnect(); }',
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

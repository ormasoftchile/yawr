exports.connect = async function connect(port) {
  const targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json();
  const target = targets.find(item => item.type === 'page' && item.webSocketDebuggerUrl);
  if (!target) throw new Error('No isolated Electron page');
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
  let sequence = 0;
  const pending = new Map();
  socket.onmessage = event => {
    const value = JSON.parse(String(event.data));
    if (!value.id) return;
    const request = pending.get(value.id);
    if (!request) return;
    pending.delete(value.id);
    value.error ? request.reject(new Error(value.error.message)) : request.resolve(value.result);
  };
  const send = (method, params) => new Promise((resolve, reject) => {
    const id = ++sequence; pending.set(id, { resolve, reject }); socket.send(JSON.stringify({ id, method, params }));
  });
  return {
    evaluate: async expression => {
      const result = await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
      if (result.exceptionDetails) throw new Error('CDP evaluation failed');
      return result.result.value;
    },
    screenshot: async file => {
      const result = await send('Page.captureScreenshot', { format: 'png' });
      require('node:fs').writeFileSync(file, Buffer.from(result.data, 'base64'));
    },
    close: () => socket.close(),
  };
};

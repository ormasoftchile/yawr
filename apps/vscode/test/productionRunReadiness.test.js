const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(require.resolve('../src/extension.ts'), 'utf8');

test('production command waits for the visible graph receiver before spawning, in either readiness order', () => {
  const start = source.indexOf('  const tryStartProductionRun = () => {');
  const end = source.indexOf('\n  const requestReload', start);
  assert.ok(start >= 0 && end > start);
  for (const order of [
    ['currentDocument', 'ready', 'visible'],
    ['ready', 'visible', 'currentDocument'],
    ['visible', 'currentDocument', 'ready'],
  ]) {
    const calls = [];
    const context = vm.createContext({
      productionRun: { inputs: {} }, ready: false, panel: { visible: false },
      currentDocument: undefined, loadController: undefined, disposed: false,
      productionRunStarted: false, productionRunSettled: false,
      startRun: inputs => calls.push(inputs),
    });
    vm.runInContext(`${source.slice(start, end)}\nglobalThis.attempt = tryStartProductionRun;`, context);
    context.attempt();
    for (const [index, condition] of order.entries()) {
      if (condition === 'visible') context.panel.visible = true;
      else context[condition] = true;
      context.attempt();
      assert.equal(calls.length, index === 2 ? 1 : 0, `${order}: ${condition}`);
    }
    context.attempt();
    assert.equal(calls.length, 1, 'later ready/view/reload notifications must not launch twice');
  }
});

test('production startup is retried after graph publication, receiver readiness and panel visibility', () => {
  for (const [start, end] of [
    ['    } finally {\n      if (loadController === controller)', '  const requestReload'],
    ["    if (candidate.type === 'ready')", "    if (candidate.type === 'session.start')"],
    ['  const visibilitySub =', '  const configSub ='],
  ]) {
    const section = source.slice(source.indexOf(start), source.indexOf(end, source.indexOf(start)));
    assert.match(section, /tryStartProductionRun\(\)/);
  }
});

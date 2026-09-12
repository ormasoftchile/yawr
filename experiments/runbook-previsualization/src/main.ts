import './styles.css';
import { releaseReadiness } from './fixture.js';
import { edgeIsEmphasized, emphasizedNodeIDs, kindGroup } from './model.js';
import type { Lens } from './model.js';

const root = document.querySelector<HTMLElement>('#app');
if (!root) throw new Error('Missing application root');
const app: HTMLElement = root;

let lens: Lens = 'all';

function render(): void {
  const emphasized = emphasizedNodeIDs(releaseReadiness, lens);
  const nodeByID = new Map(releaseReadiness.nodes.map((node) => [node.id, node]));
  app.innerHTML = `
    <section class="shell">
      <aside>
        <p class="status">Design experiment · non-production</p>
        <h1>${releaseReadiness.name}</h1>
        <p>This sandbox evaluates semantic review lenses over a neutral Yawr-shaped graph. It does not parse runbooks or prove runtime behavior.</p>
        <div class="lenses" role="group" aria-label="Review lens">
          ${(['all', 'decisions', 'exceptions'] as Lens[]).map((value) =>
            `<button data-lens="${value}" aria-pressed="${lens === value}">${value}</button>`).join('')}
        </div>
        <ul class="legend">
          <li><span class="swatch control"></span>Control</li>
          <li><span class="swatch automation"></span>Automation</li>
          <li><span class="swatch outcome"></span>Outcome</li>
        </ul>
      </aside>
      <div class="canvas" aria-label="Runbook graph">
        <svg viewBox="0 0 700 760" role="img" aria-label="${releaseReadiness.name} structural preview">
          <defs><marker id="arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto"><path d="M0,0 L8,4 L0,8 z"></path></marker></defs>
          ${releaseReadiness.edges.map((edge) => {
            const source = nodeByID.get(edge.source)!;
            const target = nodeByID.get(edge.target)!;
            const active = edgeIsEmphasized(edge, lens);
            const x1 = source.x + 105, y1 = source.y + 72, x2 = target.x + 105, y2 = target.y;
            return `<g class="edge ${active ? '' : 'muted'}"><path d="M${x1} ${y1} C${x1} ${y1 + 45},${x2} ${y2 - 45},${x2} ${y2}" marker-end="url(#arrow)"></path>${edge.label ? `<text x="${(x1 + x2) / 2 + 8}" y="${(y1 + y2) / 2}">${edge.label}</text>` : ''}</g>`;
          }).join('')}
          ${releaseReadiness.nodes.map((node) => `<g class="node ${kindGroup(node.kind)} ${emphasized.has(node.id) ? '' : 'muted'}" transform="translate(${node.x} ${node.y})">
            <rect width="210" height="72" rx="9"></rect>
            <text class="kind" x="14" y="22">${node.kind}</text>
            <text class="title" x="14" y="49">${node.title}</text>
          </g>`).join('')}
        </svg>
      </div>
    </section>`;
  app.querySelectorAll<HTMLButtonElement>('[data-lens]').forEach((button) => {
    button.addEventListener('click', () => {
      lens = button.dataset.lens as Lens;
      render();
    });
  });
}

render();

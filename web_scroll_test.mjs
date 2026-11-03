/* Checks how the monitor page scrolls its process output, which is the one
   thing about the page that is both easy to get wrong and impossible to see
   in a Go test. web/app.js is a plain script whose top level is nothing but
   declarations and a DOMContentLoaded hook, so it loads straight into a vm
   context over a stub DOM and its own functions can be called.

   Run by TestWebFollowScroll, or on its own: node web_scroll_test.mjs */
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const LINE_H = 20, VIEW_H = 200;

// A scroll container clamps scrollTop to [0, scrollHeight - clientHeight].
// That clamp is what makes "scrollTop = scrollHeight" mean "go to the end",
// so the stub has to have it or the test measures something else.
function makeLogView() {
  let top = 0;
  const el = {
    children: [], clientHeight: VIEW_H, hidden: false,
    get scrollTop() { return top; },
    set scrollTop(v) { top = Math.max(0, Math.min(v, el.scrollHeight - el.clientHeight)); },
    get childElementCount() { return el.children.length; },
    get firstChild() { return el.children[0]; },
    get scrollHeight() { return Math.max(el.children.length * LINE_H, VIEW_H); },
    appendChild(frag) { el.children.push(...(frag.__kids || [frag])); },
    removeChild(c) { el.children.splice(el.children.indexOf(c), 1); },
    querySelectorAll: () => [],
  };
  return el;
}

const log = makeLogView();
const follow = { checked: true };
const stub = (extra = {}) => ({
  style: { setProperty() {}, height: '' }, classList: { toggle() {}, add() {}, remove() {} },
  dataset: {}, hidden: false, clientHeight: 800, offsetHeight: 0, scrollHeight: 0,
  textContent: '', innerHTML: '', value: '', checked: false, firstElementChild: null,
  querySelectorAll: () => [], addEventListener() {}, appendChild() {},
  removeAttribute() {}, getBoundingClientRect: () => ({ height: 300 }),
  parentElement: { hidden: false }, ...extra,
});
const byId = {
  logview: log, follow, content: stub({ clientHeight: 800 }),
  'monitor-resizer': stub({ offsetHeight: 6 }), detail: stub(),
};
const document = {
  getElementById: (id) => byId[id] || stub(),
  createElement: () => ({ innerHTML: '' }),
  createDocumentFragment: () => ({ __kids: [], appendChild(c) { this.__kids.push(c); } }),
  documentElement: { style: { setProperty() {} } },
  querySelectorAll: () => [], addEventListener() {},
};
const ctx = vm.createContext({
  document, window: { addEventListener() {} }, console,
  location: { hash: '#settings', href: 'http://pivot/', protocol: 'http:',
              hostname: 'pivot', pathname: '/', port: '8061' },
  history: { replaceState() {} }, fetch: () => Promise.reject(new Error('offline')),
  setTimeout: () => 0, clearTimeout: () => {}, setInterval: () => 0, clearInterval: () => {},
});
const here = path.dirname(fileURLToPath(import.meta.url));
vm.runInContext(fs.readFileSync(path.join(here, 'web', 'app.js'), 'utf8'), ctx);
const run = (expr) => vm.runInContext(expr, ctx);
run('init()'); // wire the handlers the way DOMContentLoaded would

const bottom = () => log.scrollHeight - log.clientHeight;
const lines = (n, from = 0) => JSON.stringify(Array.from({ length: n },
  (_, i) => ({ seq: from + i, time: 't', stream: 'stdout', text: 'line ' + (from + i) })));

let fails = 0;
const check = (name, got, want) => {
  const ok = got === want;
  if (!ok) fails++;
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}  (scrollTop=${got}, want ${want})`);
};

run(`appendLines(${lines(100)})`);
check('starts at the end', log.scrollTop, bottom());

// The output is re-fitted to the pane on every refresh, once a second. It
// used to re-pin to the end whatever the view was showing, so reading
// anything above it lasted about a second.
log.scrollTop = 400;
run('setMonitorHeight(500)');
check('a refresh does not drag the view down', log.scrollTop, 400);
run('setMonitorHeight(500)'); run('setMonitorHeight(500)');
check('nor do several refreshes', log.scrollTop, 400);
run(`appendLines(${lines(1, 100)})`);
check('new output does not move it', log.scrollTop, 400);
run('renderDetail()');
check('a full renderDetail does not move it', log.scrollTop, 400);

// Back at the end, following resumes on its own.
log.scrollTop = bottom();
run(`appendLines(${lines(1, 101)})`);
check('at the end it follows again', log.scrollTop, bottom());
run('setMonitorHeight(300)');
check('and a resize keeps the end in view', log.scrollTop, bottom());

// A last line only part-way into its row still counts as the end.
log.scrollTop = bottom() - 30;
run(`appendLines(${lines(1, 102)})`);
check('within the slack it still follows', log.scrollTop, bottom());

// Unticking Follow pins nothing, even sitting at the end.
follow.checked = false;
log.scrollTop = bottom();
const was = log.scrollTop;
run(`appendLines(${lines(5, 103)})`);
check('Follow off does not follow', log.scrollTop, was);

// Ticking it means "take me back to the end and keep me there".
log.scrollTop = 100; follow.checked = true;
run('$("follow").onchange()');
check('ticking Follow returns to the end', log.scrollTop, bottom());

if (fails) {
  console.error(`${fails} check(s) failed`);
  process.exit(1);
}

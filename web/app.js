/* llm-pivot monitor page. Vanilla JS, no build step. */
'use strict';

const S = {
  cfg: null,
  status: null,
  logSeq: 0,
  sel: { kind: 'settings' },
  treeSig: '',
  detailSig: '',
  monitorTab: 'http',
  pollTimer: null,
  banner: null,
  monitorDragged: false,
};

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s == null ? '' : s)
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  .replace(/"/g, '&quot;');

/* ------------------------------------------------------------------- api */

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  let data = null;
  try { data = await r.json(); } catch (e) { /* empty body */ }
  if (!r.ok) throw new Error((data && data.error) || `${r.status} ${r.statusText}`);
  return data;
}

function toast(msg, kind) {
  const t = document.createElement('div');
  t.className = 'toast' + (kind ? ' ' + kind : '');
  t.textContent = msg;
  $('toasts').appendChild(t);
  setTimeout(() => t.remove(), kind === 'error' ? 8000 : 4000);
}

/* ------------------------------------------------------------------ poll */

async function poll() {
  try {
    const d = await api('GET', '/api/state?since=' + S.logSeq);
    S.cfg = d.config;
    S.status = d.status;
    $('version').textContent = 'v' + d.version;
    $('config-path').textContent = d.config_path;
    if (d.log_gap) appendLines([{ seq: 0, time: '', stream: 'system', text: '… older output dropped …' }]);
    if (d.log && d.log.length) appendLines(d.log);
    S.logSeq = d.log_seq;
    renderTop();
    renderTree();
    renderDetail();
    renderMonitor();
  } catch (e) {
    $('top-text').textContent = 'llm-pivot is not answering';
    $('top-light').className = 'light failed';
  }
}

function startPolling() {
  poll();
  clearInterval(S.pollTimer);
  S.pollTimer = setInterval(poll, 1000);
}

/* -------------------------------------------------------------- top bar */

function lightClass(state) {
  switch (state) {
    case 'running': return 'light running';
    case 'starting': return 'light starting';
    case 'stopping': return 'light stopping';
    case 'failed': return 'light failed';
    default: return 'light';
  }
}

function humanAge(sec) {
  if (!sec || sec < 0) return '0s';
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

function renderTop() {
  const st = S.status || {};
  $('top-light').className = lightClass(st.state);
  let text;
  switch (st.state) {
    case 'running': text = `${st.engine} · ${st.model} — running ${humanAge(st.uptime_seconds)}`; break;
    case 'starting': text = `${st.engine} · ${st.model} — starting…`; break;
    case 'stopping': text = `${st.engine} · ${st.model} — stopping…`; break;
    case 'failed': text = st.last_error ? 'failed: ' + st.last_error : 'failed'; break;
    default: text = 'no engine running';
  }
  $('top-text').textContent = text;
  const c = S.cfg || {};
  $('chip-router').textContent = 'router :' + (c.router_port || '?');
  $('chip-monitor').textContent = 'monitor :' + (c.monitor_port || '?');
}

/* ----------------------------------------------------- left pane (tree) */

function modelLight(name) {
  const st = S.status || {};
  if (st.model === name) return lightClass(st.state);
  // The engine that died is gone from st.model; its failure still belongs to
  // the model it was serving, and that model's light stays red until it is
  // started again.
  if (st.state === 'failed' && st.failed_model === name) return 'light failed';
  return 'light';
}

function renderTree() {
  const c = S.cfg;
  if (!c) return;
  const st = S.status || {};
  const sig = JSON.stringify([c.engines, st.model, st.failed_model, st.state, S.sel]);
  if (sig === S.treeSig) return;
  S.treeSig = sig;

  const sel = S.sel;
  $('item-settings').classList.toggle('active', sel.kind === 'settings');

  let h = '';
  for (const e of c.engines || []) {
    const act = (sel.kind === 'engine' && sel.engine === e.name) ? ' active' : '';
    h += `<button class="side-item engine-row${act}" data-kind="engine" data-engine="${esc(e.name)}">
            <span class="side-icon">▸</span>
            <span class="side-label">${esc(e.name)}</span>
            <span class="side-sub">:${esc(e.port)}</span>
          </button>`;
    if (!e.models || !e.models.length) {
      h += `<div class="engine-empty">no models yet</div>`;
    }
    for (const m of e.models || []) {
      const ma = (sel.kind === 'model' && sel.model === m.name) ? ' active' : '';
      h += `<button class="side-item model-row${ma}" data-kind="model"
                    data-engine="${esc(e.name)}" data-model="${esc(m.name)}">
              <span class="${modelLight(m.name)}"></span>
              <span class="side-label">${esc(m.name)}</span>
            </button>`;
    }
    const na = (sel.kind === 'new-model' && sel.engine === e.name) ? ' active' : '';
    h += `<button class="side-item add-model-row${na}" data-kind="new-model" data-engine="${esc(e.name)}">
            <span class="side-icon">+</span><span class="side-label">Add model</span>
          </button>`;
  }
  if (!(c.engines || []).length) {
    h = `<div class="engine-empty">no engines configured</div>`;
  }
  const tree = $('tree');
  tree.innerHTML = h;
  for (const b of tree.querySelectorAll('.side-item')) {
    b.addEventListener('click', () => {
      select({ kind: b.dataset.kind, engine: b.dataset.engine, model: b.dataset.model });
    });
  }
}

function select(sel, keepHash) {
  S.sel = sel;
  S.banner = null;
  S.treeSig = '';
  S.detailSig = '';
  S.monitorDragged = false;
  if (!keepHash) {
    const h = selToHash(sel);
    if (location.hash !== h) history.replaceState(null, '', h);
  }
  renderTree();
  renderDetail();
  renderMonitor();
}

/* The selection lives in the URL hash, so a model page can be bookmarked. */
function selToHash(s) {
  const p = encodeURIComponent;
  switch (s.kind) {
    case 'engine': return `#engine/${p(s.engine)}`;
    case 'new-engine': return '#new-engine';
    case 'model': return `#model/${p(s.engine)}/${p(s.model)}`;
    case 'new-model': return `#new-model/${p(s.engine)}`;
    default: return '#settings';
  }
}

function hashToSel() {
  const parts = location.hash.replace(/^#/, '').split('/').map(decodeURIComponent);
  switch (parts[0]) {
    case 'engine': return parts[1] ? { kind: 'engine', engine: parts[1] } : null;
    case 'new-engine': return { kind: 'new-engine' };
    case 'model': return parts[2] ? { kind: 'model', engine: parts[1], model: parts[2] } : null;
    case 'new-model': return parts[1] ? { kind: 'new-model', engine: parts[1] } : null;
    case 'settings': return { kind: 'settings' };
    default: return null;
  }
}

/* ------------------------------------------------------- right: details */

function detailSig() {
  const s = S.sel;
  return [s.kind, s.engine || '', s.model || ''].join('\u0000');
}

function renderDetail(force) {
  if (!S.cfg) return;
  const sig = detailSig();
  if (!force && sig === S.detailSig) { refreshModelControls(); fitMonitor(); return; }
  S.detailSig = sig;

  const s = S.sel;
  switch (s.kind) {
    case 'settings': renderSettings(); break;
    case 'engine': renderEngineForm(findEngine(s.engine), false); break;
    case 'new-engine': renderEngineForm(null, true); break;
    case 'model': renderModelForm(s.engine, findModel(s.engine, s.model)); break;
    case 'new-model': renderModelForm(s.engine, null); break;
    default: $('detail').innerHTML = '';
  }
  initAutosize();
  fitMonitor();
}

// autosize gives a textarea exactly the height its text needs, so a command
// or an option list is never hidden behind an inner scrollbar.
function autosize(ta) {
  ta.style.height = 'auto';
  const borders = ta.offsetHeight - ta.clientHeight;
  ta.style.height = (ta.scrollHeight + borders) + 'px';
}

function autosizeAll() {
  for (const ta of $('detail').querySelectorAll('textarea.autosize')) autosize(ta);
}

function initAutosize() {
  for (const ta of $('detail').querySelectorAll('textarea.autosize')) {
    ta.addEventListener('input', () => { autosize(ta); fitMonitor(); });
  }
  autosizeAll();
}

function findEngine(name) {
  return (S.cfg.engines || []).find((e) => e.name === name) || null;
}
function findModel(engine, model) {
  const e = findEngine(engine);
  if (!e) return null;
  return (e.models || []).find((m) => m.name === model) || null;
}

function bannerHTML() {
  if (!S.banner) return '';
  return `<div class="banner ${S.banner.kind}">${esc(S.banner.text)}</div>`;
}

/* --------------------------------------------------------------- settings */

function renderSettings() {
  const c = S.cfg;
  $('detail').innerHTML = `
  <div class="card">
    <div class="card-head"><h2>Settings</h2></div>
    <p class="card-sub">Ports are reopened immediately after saving. Changing the
       monitor port reloads this page on the new address.</p>
    ${bannerHTML()}
    <div class="grid2">
      <div class="field">
        <label>Monitor port <span class="hint">— this page</span></label>
        <input type="number" id="f-monitor-port" min="1" max="65535" value="${esc(c.monitor_port)}">
      </div>
      <div class="field">
        <label>Router port <span class="hint">— OpenAI requests</span></label>
        <input type="number" id="f-router-port" min="1" max="65535" value="${esc(c.router_port)}">
      </div>
    </div>
    <div class="grid3">
      <div class="field">
        <label>Bind address</label>
        <input type="text" id="f-host" value="${esc(c.host)}" placeholder="127.0.0.1">
      </div>
      <div class="field">
        <label>Startup timeout <span class="hint">s</span></label>
        <input type="number" id="f-startup" min="1" value="${esc(c.startup_timeout_seconds)}">
      </div>
      <div class="field">
        <label>Graceful stop timeout <span class="hint">s (killed afterwards)</span></label>
        <input type="number" id="f-stop" min="1" value="${esc(c.graceful_stop_timeout_seconds)}">
      </div>
    </div>
    <div class="grid2">
      <div class="field">
        <label>Inactive stop timeout <span class="hint">s — -1 never stops</span></label>
        <input type="number" id="f-inactive" min="-1" value="${esc(c.inactive_stop_timeout_seconds)}">
      </div>
      <div class="field">
        <label>Log buffer <span class="hint">lines kept</span></label>
        <input type="number" id="f-loglines" min="100" value="${esc(c.log_lines)}">
      </div>
    </div>
    <div class="actions">
      <button class="btn primary" id="btn-save-settings">Save settings</button>
      <div class="spacer"></div>
      <button class="btn" id="btn-add-engine-2">Add engine</button>
    </div>
  </div>`;

  $('btn-add-engine-2').onclick = () => select({ kind: 'new-engine' });
  $('btn-save-settings').onclick = async () => {
    const body = {
      monitor_port: num('f-monitor-port'),
      router_port: num('f-router-port'),
      host: $('f-host').value.trim() || '127.0.0.1',
      startup_timeout_seconds: num('f-startup'),
      graceful_stop_timeout_seconds: num('f-stop'),
      inactive_stop_timeout_seconds: num('f-inactive'),
      log_lines: num('f-loglines'),
    };
    const oldMonitor = S.cfg.monitor_port;
    const oldHost = S.cfg.host;
    try {
      const res = await api('PUT', '/api/settings', body);
      (res.notes || []).forEach((n) => toast(n));
      toast('Settings saved', 'ok');
      S.banner = null;
      // The host may have moved too; 0.0.0.0 still serves the hostname
      // this page was opened on, so keep it for that case.
      const hostMoved = body.host && body.host !== oldHost && body.host !== '0.0.0.0';
      if (res.monitor_port !== oldMonitor || hostMoved) {
        toast('Moving this page to port ' + res.monitor_port + '…');
        const host = hostMoved ? body.host : location.hostname;
        const url = location.protocol + '//' + host + ':' + res.monitor_port
          + location.pathname + location.hash;
        setTimeout(() => moveTo(url), 1200);
      }
      await poll();
      // poll() refreshed S.cfg, so a smaller log_lines applies to what is
      // already on screen too, not only to lines arriving after it.
      trimLog();
      renderDetail(true);
    } catch (e) {
      S.banner = { kind: 'error', text: e.message };
      renderDetail(true);
    }
  };
}

function num(id) {
  const v = parseInt($(id).value, 10);
  return isNaN(v) ? 0 : v;
}

/* Moving the monitor port means moving this page. Assigning location.port
   alone never navigates (only href/assign do), and the old address stops
   serving once the rebind lands — so build a full URL, wait for the
   rebind, then probe the new address before navigating. If it never
   answers, the rebind failed (the port was taken): stay put and say so. */
async function moveTo(url) {
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  await wait(1200);
  for (let i = 0; i < 6; i++) {
    try {
      await fetch(url, { mode: 'no-cors' }); // opaque reply: bound, or network error
      location.href = url;
      return;
    } catch (e) {
      await wait(1000);
    }
  }
  toast('the new monitor address never answered; this page stays on the old one', 'error');
}

/* ----------------------------------------------------------------- engine */

function renderEngineForm(e, isNew) {
  if (!isNew && !e) { select({ kind: 'settings' }); return; }
  const d = e || {
    name: '', port: 8080, command: '', monitor: 'stdio', monitor_url: '',
    monitor_interval_seconds: 5, health_path: '/health', port_flag: '--port',
    startup_timeout_seconds: 0, work_dir: '', env: [],
  };
  const isHTTP = d.monitor === 'http';
  const isEmbed = d.monitor === 'embed';
  const hasURL = isHTTP || isEmbed;

  $('detail').innerHTML = `
  <div class="card">
    <div class="card-head"><h2>${isNew ? 'New engine' : 'Engine · ' + esc(d.name)}</h2></div>
    <p class="card-sub">Flags written here are the defaults for every model of this
       engine. A model may repeat a flag to override its value.</p>
    ${bannerHTML()}
    <div class="grid2">
      <div class="field">
        <label>Name</label>
        <input type="text" id="e-name" value="${esc(d.name)}" placeholder="llama.cpp">
      </div>
      <div class="field">
        <label>Port <span class="hint">— where the engine listens</span></label>
        <input type="number" id="e-port" min="1" max="65535" value="${esc(d.port)}">
      </div>
    </div>
    <div class="field">
      <label>Command <span class="hint">— program and default options</span></label>
      <textarea id="e-command" class="autosize" rows="2" spellcheck="false"
        placeholder="/path/to/llama-server --ctx-size 64000">${esc(d.command)}</textarea>
    </div>
    <div class="grid2">
      <div class="field">
        <label>Monitoring info from</label>
        <select id="e-monitor">
          <option value="stdio"${hasURL ? '' : ' selected'}>engine stdout / stderr</option>
          <option value="http"${isHTTP ? ' selected' : ''}>http port (fetch and show the text)</option>
          <option value="embed"${isEmbed ? ' selected' : ''}>the engine's own page (in a frame)</option>
        </select>
      </div>
      <div class="field" id="e-url-field"${hasURL ? '' : ' hidden'}>
        <label>Monitor URL <span class="hint">— {{port}} is substituted</span></label>
        <input type="text" id="e-monitor-url" value="${esc(d.monitor_url)}"
               placeholder="http://127.0.0.1:{{port}}/metrics">
      </div>
    </div>
    <div class="grid3">
      <div class="field" id="e-interval-field"${isHTTP ? '' : ' hidden'}>
        <!-- polling only applies to the fetch-and-show mode -->
        <label>Poll interval <span class="hint">s</span></label>
        <input type="number" id="e-interval" min="1" value="${esc(d.monitor_interval_seconds || 5)}">
      </div>
      <div class="field">
        <label>Health path <span class="hint">— empty: TCP only</span></label>
        <input type="text" id="e-health" value="${esc(d.health_path)}" placeholder="/health">
      </div>
      <div class="field">
        <label>Port flag <span class="hint">— "-" to not inject</span></label>
        <input type="text" id="e-portflag" value="${esc(d.port_flag || '--port')}">
      </div>
    </div>
    <div class="grid2">
      <div class="field">
        <label>Startup timeout <span class="hint">s — 0 uses the global one</span></label>
        <input type="number" id="e-startup" min="0" value="${esc(d.startup_timeout_seconds || 0)}">
      </div>
      <div class="field">
        <label>Working directory <span class="hint">— optional</span></label>
        <input type="text" id="e-workdir" value="${esc(d.work_dir || '')}">
      </div>
    </div>
    <div class="field">
      <label>Environment <span class="hint">— one KEY=VALUE per line</span></label>
      <textarea id="e-env" rows="3" spellcheck="false">${esc((d.env || []).join('\n'))}</textarea>
    </div>
    <div class="actions">
      <button class="btn primary" id="e-save">${isNew ? 'Create engine' : 'Save engine'}</button>
      ${isNew ? '' : '<button class="btn" id="e-add-model">Add model</button>'}
      <div class="spacer"></div>
      ${isNew ? '' : '<button class="btn danger" id="e-delete">Delete engine</button>'}
    </div>
  </div>`;

  const sync = () => {
    const mode = $('e-monitor').value;
    const needsURL = mode === 'http' || mode === 'embed';
    $('e-url-field').hidden = !needsURL;
    $('e-interval-field').hidden = mode !== 'http';
    if (needsURL && !$('e-monitor-url').value.trim()) {
      $('e-monitor-url').value = mode === 'embed'
        ? 'http://127.0.0.1:{{port}}/'
        : 'http://127.0.0.1:{{port}}/metrics';
    }
  };
  $('e-monitor').onchange = sync;

  $('e-save').onclick = async () => {
    const body = {
      original: isNew ? '' : d.name,
      name: $('e-name').value.trim(),
      port: num('e-port'),
      command: $('e-command').value.trim(),
      monitor: $('e-monitor').value,
      monitor_url: $('e-monitor-url').value.trim(),
      monitor_interval_seconds: num('e-interval'),
      health_path: $('e-health').value.trim(),
      port_flag: $('e-portflag').value.trim(),
      startup_timeout_seconds: num('e-startup'),
      work_dir: $('e-workdir').value.trim(),
      env: [$('e-env').value],
    };
    try {
      const res = await api('POST', '/api/engines', body);
      (res.notes || []).forEach((n) => toast(n));
      toast(isNew ? 'Engine created' : 'Engine saved', 'ok');
      S.banner = null;
      await poll();
      select({ kind: 'engine', engine: body.name });
    } catch (err) {
      S.banner = { kind: 'error', text: err.message };
      renderDetail(true);
    }
  };

  if (!isNew) {
    $('e-add-model').onclick = () => select({ kind: 'new-model', engine: d.name });
    $('e-delete').onclick = async () => {
      const n = (d.models || []).length;
      const extra = n ? ` and its ${n} model${n > 1 ? 's' : ''}` : '';
      if (!confirm(`Delete engine "${d.name}"${extra}?`)) return;
      try {
        const res = await api('DELETE', '/api/engines/' + encodeURIComponent(d.name));
        (res.notes || []).forEach((x) => toast(x));
        toast('Engine deleted', 'ok');
        await poll();
        select({ kind: 'settings' });
      } catch (err) {
        S.banner = { kind: 'error', text: err.message };
        renderDetail(true);
      }
    };
  }
}

/* ------------------------------------------------------------------ model */

function renderModelForm(engineName, m) {
  const isNew = !m;
  const d = m || { name: '', options: '' };
  const eng = findEngine(engineName);
  if (!eng) { select({ kind: 'settings' }); return; }

  $('detail').innerHTML = `
  <div class="card">
    <div class="card-head">
      <h2>${isNew ? 'New model' : 'Model · ' + esc(d.name)}</h2>
      <div class="card-head-right"><span class="side-sub">engine ${esc(eng.name)} :${esc(eng.port)}</span></div>
    </div>
    <p class="card-sub">Model names must be unique across all engines; the router
       dispatches on them.</p>
    ${bannerHTML()}
    <div id="model-status"></div>
    <div class="field">
      <label>Model name <span class="hint">— the "model" field of OpenAI requests</span></label>
      <input type="text" id="m-name" value="${esc(d.name)}" placeholder="qwen3-30b">
    </div>
    <div class="field">
      <label>Additional engine options <span class="hint">— override the engine's own flags</span></label>
      <textarea id="m-options" class="autosize" rows="2" spellcheck="false"
        placeholder="-m /models/qwen3-30b.gguf --ctx-size 32768">${esc(d.options)}</textarea>
    </div>
    ${isNew ? '' : `
    <div class="field">
      <label>Effective command line</label>
      <pre class="cmdbox" id="m-command">…</pre>
    </div>`}
    <div class="actions" id="model-actions"></div>
  </div>`;

  refreshModelControls();
  if (!isNew) loadEffectiveCommand(d.name);
}

// refreshModelControls redraws the parts of the model card that change on
// their own (the run state and the action buttons), leaving inputs alone.
function refreshModelControls() {
  const box = $('model-actions');
  if (!box) return;
  const s = S.sel;
  const isNew = s.kind === 'new-model';
  const st = S.status || {};
  const name = isNew ? '' : s.model;
  const isCurrent = !isNew && st.model === name;
  const busy = st.state === 'starting' || st.state === 'stopping';

  const statusBox = $('model-status');
  if (statusBox) {
    if (isCurrent) {
      statusBox.innerHTML = `
        <div class="statline">
          <span><b>State</b><span class="${lightClass(st.state)}"
                style="display:inline-block;vertical-align:middle;margin-right:6px"></span>
                <span class="v">${esc(st.state)}</span></span>
          <span><b>PID</b><span class="v">${esc(st.pid || '—')}</span></span>
          <span><b>Port</b><span class="v">${esc(st.port || '—')}</span></span>
          <span><b>Uptime</b><span class="v">${esc(humanAge(st.uptime_seconds))}</span></span>
          <span><b>Monitor</b><span class="v">${esc(st.monitor_mode || '—')}</span></span>
        </div>`;
    } else if (st.state === 'failed' && st.last_error && st.failed_model === name) {
      // The dead engine is no longer st.model, so the card is keyed on the
      // failure itself: only the model whose engine broke shows this banner.
      statusBox.innerHTML = `<div class="banner error">${esc(st.last_error)}</div>`;
    } else {
      statusBox.innerHTML = '';
    }
  }

  let h = '';
  h += `<button class="btn primary" id="m-save">${isNew ? 'Create model' : 'Save model'}</button>`;
  if (!isNew) {
    if (isCurrent) {
      h += `<button class="btn" id="m-restart"${busy ? ' disabled' : ''}>Restart</button>`;
      h += `<button class="btn" id="m-stop"${busy ? ' disabled' : ''}>Stop</button>`;
    } else {
      h += `<button class="btn green" id="m-start"${busy ? ' disabled' : ''}>Start</button>`;
    }
    h += `<button class="btn" id="m-copy">Copy model</button>`;
    h += `<div class="spacer"></div><button class="btn danger" id="m-delete">Delete model</button>`;
  }
  box.innerHTML = h;

  $('m-save').onclick = () => saveModel(isNew);
  if (!isNew) {
    const act = async (action) => {
      const label = { start: 'Starting', restart: 'Restarting', stop: 'Stopping' }[action];
      toast(`${label} ${action === 'stop' ? 'the engine' : name}…`);
      try {
        await api('POST', '/api/control', { action, model: name });
        toast(action === 'stop' ? 'Stopped' : `${name} is ready`, 'ok');
      } catch (e) {
        toast(e.message, 'error');
      }
      poll();
    };
    const bind = (id, a) => { const b = $(id); if (b) b.onclick = () => act(a); };
    bind('m-start', 'start');
    bind('m-restart', 'restart');
    bind('m-stop', 'stop');
    $('m-copy').onclick = async () => {
      const src = findModel(s.engine, name);
      if (!src) return;
      const copy = name + '-copy';
      try {
        const res = await api('POST', '/api/models',
          { engine: s.engine, original: '', name: copy, options: src.options || '' });
        (res.notes || []).forEach((x) => toast(x));
        toast(`Copied to ${copy}`, 'ok');
        await poll();
        select({ kind: 'model', engine: s.engine, model: copy });
      } catch (e) {
        toast(e.message, 'error');
      }
    };
    $('m-delete').onclick = async () => {
      if (!confirm(`Delete model "${name}"?`)) return;
      try {
        const res = await api('DELETE',
          `/api/engines/${encodeURIComponent(s.engine)}/models/${encodeURIComponent(name)}`);
        (res.notes || []).forEach((x) => toast(x));
        toast('Model deleted', 'ok');
        await poll();
        select({ kind: 'engine', engine: s.engine });
      } catch (e) {
        toast(e.message, 'error');
      }
    };
  }
}

async function saveModel(isNew) {
  const s = S.sel;
  const body = {
    engine: s.engine,
    original: isNew ? '' : s.model,
    name: $('m-name').value.trim(),
    options: $('m-options').value.trim(),
  };
  try {
    const res = await api('POST', '/api/models', body);
    (res.notes || []).forEach((n) => toast(n));
    toast(isNew ? 'Model created' : 'Model saved', 'ok');
    S.banner = null;
    await poll();
    select({ kind: 'model', engine: s.engine, model: body.name });
  } catch (e) {
    S.banner = { kind: 'error', text: e.message };
    renderDetail(true);
  }
}

async function loadEffectiveCommand(name) {
  const box = $('m-command');
  if (!box) return;
  try {
    const d = await api('GET', '/api/command?model=' + encodeURIComponent(name));
    if ($('m-command')) $('m-command').textContent = d.command;
  } catch (e) {
    if ($('m-command')) $('m-command').textContent = '(' + e.message + ')';
  }
  fitMonitor();
}

/* ---------------------------------------------------------------- monitor */

function appendLines(lines) {
  const pre = $('logview');
  const follow = $('follow').checked;
  const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 40;
  const frag = document.createDocumentFragment();
  for (const l of lines) {
    const div = document.createElement('div');
    let cls = '';
    if (l.stream === 'system') cls = 'sys';
    else if (l.stream === 'stderr') cls = 'err';
    div.innerHTML = `<span class="t">${esc(l.time)}</span> <span class="${cls}">${esc(l.text)}</span>`;
    frag.appendChild(div);
  }
  pre.appendChild(frag);
  trimLog();
  if (follow && atBottom) pre.scrollTop = pre.scrollHeight;
}

/* The view keeps what the log buffer keeps. Hard-coding a limit here made
   "Log buffer — lines kept" a lie in both directions: a bigger setting still
   lost lines on screen, a smaller one kept showing lines the server had
   already dropped. DefaultLogLines is the fallback for a state we have not
   fetched yet. */
function trimLog() {
  const pre = $('logview');
  if (!pre) return;
  const keep = (S.cfg && S.cfg.log_lines) || 4000;
  while (pre.childElementCount > keep) pre.removeChild(pre.firstChild);
}

function renderMonitor() {
  const st = S.status || {};
  const mode = st.monitor_mode || '';
  const hasOwnView = mode === 'http' || mode === 'embed';

  $('monitor-tabs').hidden = !hasOwnView;
  if (!hasOwnView) S.monitorTab = 'log';
  else if (S.monitorTab !== 'log') S.monitorTab = 'engine';

  const showEngine = hasOwnView && S.monitorTab === 'engine';
  const showEmbed = showEngine && mode === 'embed';
  $('httpview').hidden = !(showEngine && mode === 'http');
  $('embedview').hidden = !showEmbed;
  $('logview').hidden = showEngine;
  $('btn-reload-embed').hidden = !showEmbed;
  $('btn-open-embed').hidden = !showEmbed;
  $('btn-clear-log').hidden = showEngine && mode === 'embed';
  $('follow').parentElement.hidden = showEngine;

  for (const t of document.querySelectorAll('#monitor-tabs .tab')) {
    // The first tab is the engine's own view whatever its flavour.
    const key = t.dataset.tab === 'log' ? 'log' : 'engine';
    t.classList.toggle('active', key === S.monitorTab);
  }

  let title = 'Engine monitor';
  let meta = 'Nothing is running. Start a model from the left pane.';
  if (st.model) {
    title = `Engine monitor · ${st.engine} · ${st.model}`;
    meta = `pid ${st.pid || '—'} · port ${st.port || '—'} · ${st.state}`;
    if (st.command) meta += `\n$ ${st.command}`;
  }
  $('monitor-title').textContent = title;
  $('monitor-meta').textContent = meta;

  if (mode === 'embed') renderEmbed(st);
  if (showEngine && mode === 'http') {
    const m = st.monitor;
    const view = $('httpview');
    if (!m) {
      view.textContent = 'waiting for the first sample…';
    } else {
      const head = `GET ${m.url}\n${m.error ? 'error: ' + m.error : 'HTTP ' + m.status} ` +
        `· sample #${m.samples} at ${m.time}\n${'─'.repeat(40)}\n`;
      view.textContent = head + (m.body || '');
    }
  }
}

// renderEmbed points the frame at the engine's own monitoring page. The src
// is assigned only when it actually changes: re-assigning it on every poll
// would reload the page once a second and throw away its state.
function renderEmbed(st) {
  const frame = $('embedview');
  const want = st.monitor_url || '';
  if (frame.dataset.src === want) return;
  frame.dataset.src = want;
  if (want) {
    frame.src = want;
  } else {
    frame.removeAttribute('src');
  }
  $('btn-open-embed').href = want;
}

/* ------------------------------------------------- monitor pane resizing */

const MONITOR_MIN = 170;

function monitorMax() {
  const content = $('content');
  const avail = content ? content.clientHeight : window.innerHeight;
  return Math.max(MONITOR_MIN, avail - 150); // always leave room for the form
}

function setMonitorHeight(px) {
  const h = Math.round(Math.min(Math.max(px, MONITOR_MIN), monitorMax()));
  document.documentElement.style.setProperty('--monitor-h', h + 'px');
  // Keep the newest output in view while the pane grows or shrinks.
  const pre = $('logview');
  if (pre && $('follow').checked) pre.scrollTop = pre.scrollHeight;
}

// fitMonitor gives the monitor every pixel the form above does not need, so
// it is as tall as the selected engine or model page permits. A drag opts
// out of this until another page is selected.
function fitMonitor() {
  if (S.monitorDragged) return;
  const content = $('content');
  const form = $('detail').firstElementChild;
  if (!content) return;
  const avail = content.clientHeight - $('monitor-resizer').offsetHeight;
  // Twice: the first pass may retract the form's scrollbar, which widens the
  // text and makes the form shorter again. 2px of slack, because a form one
  // pixel too tall brings that scrollbar straight back.
  for (let i = 0; i < 2; i++) {
    setMonitorHeight(avail - (form ? form.offsetHeight : 0) - 2);
  }
}

function initResizer() {
  const bar = $('monitor-resizer');
  const card = $('monitor-card');
  let dragging = false, startY = 0, startH = 0;

  const drag = (px) => { S.monitorDragged = true; setMonitorHeight(px); };

  bar.addEventListener('pointerdown', (e) => {
    dragging = true;
    startY = e.clientY;
    startH = card.getBoundingClientRect().height;
    bar.classList.add('dragging');
    bar.setPointerCapture(e.pointerId);
    document.body.style.userSelect = 'none';
    e.preventDefault();
  });
  bar.addEventListener('pointermove', (e) => {
    if (dragging) drag(startH - (e.clientY - startY));
  });
  const stop = () => {
    if (!dragging) return;
    dragging = false;
    bar.classList.remove('dragging');
    document.body.style.userSelect = '';
  };
  bar.addEventListener('pointerup', stop);
  bar.addEventListener('pointercancel', stop);

  // Keyboard: the handle is focusable, arrows resize it.
  bar.addEventListener('keydown', (e) => {
    const step = e.shiftKey ? 60 : 20;
    const h = card.getBoundingClientRect().height;
    if (e.key === 'ArrowUp') drag(h + step);
    else if (e.key === 'ArrowDown') drag(h - step);
    else return;
    e.preventDefault();
  });

  // A narrower window rewraps the textareas, so measure them again.
  window.addEventListener('resize', () => {
    autosizeAll();
    if (S.monitorDragged) setMonitorHeight(card.getBoundingClientRect().height);
    else fitMonitor();
  });
}

/* ------------------------------------------------------------------- init */

function init() {
  S.sel = hashToSel() || { kind: 'settings' };
  window.addEventListener('hashchange', () => {
    const s = hashToSel();
    if (s && selToHash(s) !== selToHash(S.sel)) select(s, true);
  });
  $('item-settings').onclick = () => select({ kind: 'settings' });
  $('btn-add-engine').onclick = () => select({ kind: 'new-engine' });
  $('btn-clear-log').onclick = async () => {
    await api('POST', '/api/log/clear');
    $('logview').innerHTML = '';
    S.logSeq = 0;
    poll();
  };
  for (const t of document.querySelectorAll('#monitor-tabs .tab')) {
    t.onclick = () => {
      S.monitorTab = t.dataset.tab === 'log' ? 'log' : 'engine';
      renderMonitor();
    };
  }
  $('btn-reload-embed').onclick = () => {
    const frame = $('embedview');
    const src = frame.dataset.src;
    if (src) frame.src = src; // re-assigning the same url reloads it
  };
  initResizer();
  startPolling();
}

document.addEventListener('DOMContentLoaded', init);

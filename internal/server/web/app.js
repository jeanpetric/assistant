'use strict';

// ---------- helpers ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'value') el.value = v;
    else if (k === 'checked' || k === 'disabled' || k === 'selected') el[k] = !!v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 204 || res.status === 202) return null;
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    const msg = (data && data.error) || res.statusText;
    toast(msg, true);
    throw new Error(msg);
  }
  return data;
}

function toast(msg) {
  const el = h('div', { class: 'toast' }, msg);
  document.body.append(el);
  setTimeout(() => el.remove(), 3000);
}

function ago(ts) {
  if (!ts) return '';
  const s = Math.round((Date.now() - new Date(ts)) / 1000);
  if (Math.abs(s) < 60) return 'just now';
  const a = Math.abs(s);
  const txt = a < 3600 ? Math.floor(a / 60) + 'm' : a < 86400 ? Math.floor(a / 3600) + 'h' : Math.floor(a / 86400) + 'd';
  return s < 0 ? 'in ' + txt : txt + ' ago';
}

function fmtTime(ts) {
  return ts ? new Date(ts).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '';
}

function select(options, value, attrs) {
  return h('select', attrs, options.map(([v, label]) => h('option', { value: v, selected: String(v) === String(value) }, label)));
}

// Working-directory picker: the configured choices, plus value if it is not one of them.
function workdirSelect(value, defaultLabel, attrs) {
  const dirs = state.meta.workdirs.slice();
  if (value && !dirs.includes(value)) dirs.unshift(value);
  return select([['', defaultLabel], ...dirs.map(d => [d, d])], value || '', attrs);
}

function modeOptions(includeDefault) {
  const labels = {
    bypassPermissions: 'Autopilot (bypass permissions)',
    acceptEdits: 'Accept edits',
    plan: 'Plan only (read-only)',
    default: 'Default (unapproved tools denied)',
  };
  const opts = state.meta.modes.map(m => [m, labels[m] || m]);
  return includeDefault ? [['', `App default (${state.meta.default_mode})`], ...opts] : opts;
}

// Minimal rendering: fenced code blocks become <pre>, the rest stays plain text.
function richText(text) {
  const out = [];
  const parts = text.split(/```[^\n]*\n?/);
  parts.forEach((p, i) => {
    if (!p) return;
    out.push(i % 2 ? h('pre', {}, p.replace(/\n$/, '')) : document.createTextNode(p));
  });
  return out;
}

function trunc(s, n) {
  s = (s || '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// ---------- state & routing ----------

const state = { meta: null, board: null, timers: [] };
const view = document.getElementById('view');

function every(ms, fn) {
  state.timers.push(setInterval(fn, ms));
}

function route() {
  state.timers.forEach(clearInterval);
  state.timers = [];
  const parts = (location.hash.replace(/^#\/?/, '') || 'board').split('/');
  document.querySelectorAll('[data-nav]').forEach(a => a.classList.toggle('active', a.dataset.nav === parts[0]));
  view.replaceChildren();
  const r = {
    board: renderBoard,
    list: renderList,
    task: () => renderTask(Number(parts[1]), Number(parts[3]) || 0),
    sessions: renderSessions,
    settings: renderSettings,
  }[parts[0]] || renderBoard;
  r().catch(err => view.replaceChildren(h('div', { class: 'panel' }, 'Error: ' + err.message)));
}

async function loadBoard() {
  state.board = await api('GET', '/api/board');
  return state.board;
}

function laneName(id) {
  const l = state.board && state.board.lanes.find(l => l.id === id);
  return l ? l.name : '?';
}

function fieldLinks(task) {
  return state.board.fields
    .filter(f => task.fields[f.key])
    .map(f => f.type === 'url'
      ? h('a', { class: 'badge', href: task.fields[f.key], target: '_blank', rel: 'noopener', onclick: e => e.stopPropagation() }, f.label)
      : h('span', { class: 'badge', title: f.label }, task.fields[f.key]));
}

function kindBadge(kind) {
  return kind && kind !== 'task' ? h('span', { class: 'badge kind-' + kind }, kind) : null;
}

function runningBadge(n) {
  return n ? h('span', { class: 'badge running' }, h('span', { class: 'pulse' }), n > 1 ? `${n} agents` : 'agent') : null;
}

// ---------- board ----------

let dragId = null;

async function renderBoard() {
  const b = await loadBoard();
  const board = h('div', { class: 'board' });
  for (const lane of b.lanes) {
    const tasks = b.tasks.filter(t => t.lane_id === lane.id).sort((a, c) => a.position - c.position);
    const cards = h('div', { class: 'lane-cards' }, tasks.map(card));
    const laneEl = h('section', { class: 'lane' },
      h('div', { class: 'lane-head' },
        h('span', { class: 'dot', style: lane.color ? `background:${lane.color}` : null }),
        lane.name, h('span', { class: 'count' }, tasks.length)),
      cards,
      h('div', { class: 'lane-add' }, h('input', {
        placeholder: '+ Add task', 'data-lane': lane.id, onkeydown: async e => {
          if (e.key !== 'Enter' || !e.target.value.trim()) return;
          const title = e.target.value.trim();
          e.target.value = '';
          await api('POST', '/api/tasks', { title, lane_id: lane.id });
          renderBoardInPlace();
        },
      })));
    wireDrop(laneEl, cards, lane.id);
    board.append(laneEl);
  }
  view.replaceChildren(board);
  every(5000, () => { if (!dragId) renderBoardInPlace(); });
}

async function renderBoardInPlace() {
  const scroll = view.querySelector('.board')?.scrollLeft || 0;
  const active = document.activeElement;
  const focusedLane = active && active.closest('.lane-add') ? active.dataset.lane : null;
  if (focusedLane && active.value) return; // don't clobber typing
  state.timers.forEach(clearInterval);
  state.timers = [];
  await renderBoard();
  view.querySelector('.board').scrollLeft = scroll;
  // The re-render replaces the inputs; keep the cursor in the lane's add box.
  const input = focusedLane && view.querySelector(`.lane-add input[data-lane="${focusedLane}"]`);
  if (input) { input.value = active.value; input.focus(); }
}

function card(t) {
  const el = h('article', { class: 'card', draggable: 'true', onclick: () => { location.hash = `#/task/${t.id}`; } },
    h('div', { class: 'card-title' }, t.title),
    h('div', { class: 'card-meta' }, kindBadge(t.kind), runningBadge(state.board.running[t.id]), fieldLinks(t)));
  el.dataset.id = t.id;
  el.addEventListener('dragstart', e => {
    dragId = t.id;
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', String(t.id));
    requestAnimationFrame(() => el.classList.add('dragging'));
  });
  el.addEventListener('dragend', () => {
    dragId = null;
    el.classList.remove('dragging');
    document.querySelectorAll('.drop-marker').forEach(m => m.remove());
    document.querySelectorAll('.lane.drag-over').forEach(l => l.classList.remove('drag-over'));
  });
  return el;
}

function dropIndex(container, y) {
  const cards = [...container.querySelectorAll('.card:not(.dragging)')];
  const i = cards.findIndex(c => {
    const r = c.getBoundingClientRect();
    return y < r.top + r.height / 2;
  });
  return { index: i < 0 ? cards.length : i, before: cards[i] };
}

function wireDrop(laneEl, container, laneId) {
  const marker = h('div', { class: 'drop-marker' });
  laneEl.addEventListener('dragover', e => {
    if (dragId == null) return;
    e.preventDefault();
    laneEl.classList.add('drag-over');
    const { before } = dropIndex(container, e.clientY);
    if (before) container.insertBefore(marker, before); else container.append(marker);
  });
  laneEl.addEventListener('dragleave', e => {
    if (!laneEl.contains(e.relatedTarget)) {
      laneEl.classList.remove('drag-over');
      marker.remove();
    }
  });
  laneEl.addEventListener('drop', async e => {
    e.preventDefault();
    const id = dragId;
    if (id == null) return;
    const { index, before } = dropIndex(container, e.clientY);
    marker.remove();
    laneEl.classList.remove('drag-over');
    const el = document.querySelector(`.card[data-id="${id}"]`);
    if (el) { if (before) container.insertBefore(el, before); else container.append(el); }
    const res = await api('POST', `/api/tasks/${id}/move`, { lane_id: laneId, index });
    if (res && res.started.length) toast(`Started ${res.started.map(s => s.title).join(', ')}`);
    renderBoardInPlace();
  });
}

// ---------- list ----------

async function renderList() {
  const b = await loadBoard();
  const laneOrder = Object.fromEntries(b.lanes.map(l => [l.id, l.position]));
  const filter = h('input', { placeholder: 'Filter…', oninput: draw });
  const laneSel = select([['', 'All lanes'], ...b.lanes.map(l => [l.id, l.name])], '', { onchange: draw });
  const tbody = h('tbody');
  function draw() {
    const q = filter.value.toLowerCase();
    const rows = b.tasks
      .filter(t => !laneSel.value || t.lane_id === Number(laneSel.value))
      .filter(t => !q || (t.title + ' ' + t.description).toLowerCase().includes(q))
      .sort((a, c) => laneOrder[a.lane_id] - laneOrder[c.lane_id] || a.position - c.position);
    tbody.replaceChildren(...rows.map(t => h('tr', { class: 'clickable', onclick: () => { location.hash = `#/task/${t.id}`; } },
      h('td', {}, h('div', {}, t.title), t.description ? h('div', { class: 'muted small' }, trunc(t.description, 120)) : null),
      h('td', {}, laneName(t.lane_id)),
      h('td', {}, kindBadge(t.kind) || 'task'),
      h('td', {}, runningBadge(b.running[t.id]), fieldLinks(t)),
      h('td', { class: 'muted small' }, ago(t.updated_at)))));
  }
  draw();
  view.replaceChildren(h('div', { class: 'panel' },
    h('div', { class: 'toolbar' }, filter, laneSel),
    h('table', {}, h('thead', {}, h('tr', {}, ['Task', 'Lane', 'Kind', 'Links', 'Updated'].map(x => h('th', {}, x)))), tbody)));
  filter.focus();
}

// ---------- task page ----------

async function renderTask(id, focusSession) {
  await loadBoard();
  const data = await api('GET', `/api/tasks/${id}`);
  const t = data.task;
  const save = patch => api('PATCH', `/api/tasks/${id}`, patch).then(nt => Object.assign(t, nt));

  // --- side panel ---
  const side = h('div', { class: 'task-side' });
  const fields = state.board.fields.map(f => {
    const input = h('input', { value: t.fields[f.key] || '', placeholder: f.type === 'url' ? 'https://…' : '' });
    const open = h('a', { class: 'btn ghost small', target: '_blank', rel: 'noopener', href: t.fields[f.key] || '#' }, '↗');
    open.classList.toggle('hidden', f.type !== 'url' || !t.fields[f.key]);
    input.addEventListener('change', async () => {
      await save({ fields: { [f.key]: input.value } });
      open.href = input.value;
      open.classList.toggle('hidden', f.type !== 'url' || !input.value.trim());
    });
    return [h('label', {}, f.label), h('div', { class: 'field-row' }, input, open)];
  });

  side.append(h('div', { class: 'panel' },
    h('input', { class: 'title-input', value: t.title, onchange: e => save({ title: e.target.value }) }),
    h('label', {}, 'Lane'),
    select(state.board.lanes.map(l => [l.id, l.name]), t.lane_id, {
      onchange: async e => {
        const res = await api('POST', `/api/tasks/${id}/move`, { lane_id: Number(e.target.value), index: -1 });
        if (res.started.length) toast(`Started ${res.started.map(s => s.title).join(', ')}`);
      },
    }),
    h('label', {}, 'Kind'),
    select(state.meta.kinds.map(k => [k, k]), t.kind, { onchange: e => save({ kind: e.target.value }) }),
    h('label', {}, 'Description'),
    h('textarea', { rows: 6, onchange: e => save({ description: e.target.value }) }, t.description),
    h('label', {}, 'Working directory for agents'),
    workdirSelect(t.workdir, `App default (${state.meta.workdir})`, { onchange: e => save({ workdir: e.target.value }) }),
    fields,
    h('div', { class: 'row', style: 'margin-top:14px' },
      h('span', { class: 'muted small grow' }, 'Created ' + fmtTime(t.created_at)),
      h('button', {
        class: 'danger', onclick: async () => {
          if (!confirm('Delete this task and all its sessions?')) return;
          await api('DELETE', `/api/tasks/${id}`);
          location.hash = '#/board';
        },
      }, 'Delete'))));

  side.append(schedulesPanel(id, data.schedules));

  // --- sessions ---
  const sessionsEl = h('div');
  const components = new Map();
  const addSession = s => {
    const c = sessionComponent(s, s.messages || []);
    components.set(s.id, c);
    sessionsEl.prepend(c.el); // newest first
  };
  data.sessions.forEach(addSession);

  const main = h('div', {}, newSessionPanel(t, addSession), sessionsEl,
    data.sessions.length ? null : h('p', { class: 'muted' }, 'No agent sessions yet.'));
  view.replaceChildren(h('div', { class: 'task-page' }, side, main));

  if (focusSession && components.has(focusSession)) components.get(focusSession).el.scrollIntoView();

  every(2000, async () => {
    const list = await api('GET', `/api/sessions?task_id=${id}`);
    for (const s of list) {
      const c = components.get(s.id);
      if (!c) {
        const full = await api('GET', `/api/sessions/${s.id}`);
        addSession(Object.assign(full.session, { messages: full.messages }));
      } else if (s.status === 'running' || s.status !== c.session.status || s.updated_at !== c.session.updated_at) {
        const upd = await api('GET', `/api/sessions/${s.id}?after=${c.lastId()}`);
        c.update(upd.session, upd.messages);
      }
    }
  });
}

function newSessionPanel(task, onStarted) {
  const prompt = h('textarea', { rows: 4, placeholder: 'What should the agent do? Skills work too, e.g. /p-dev:review 123' });
  const mode = select(modeOptions(false), state.meta.default_mode);
  const ctx = h('input', { type: 'checkbox', checked: true });
  const workdir = workdirSelect('', `Default (${task.workdir || state.meta.workdir})`);
  const start = async () => {
    if (!prompt.value.trim()) return;
    const s = await api('POST', `/api/tasks/${task.id}/sessions`, {
      prompt: prompt.value, mode: mode.value, workdir: workdir.value, include_context: ctx.checked,
    });
    prompt.value = '';
    const full = await api('GET', `/api/sessions/${s.id}`);
    onStarted(Object.assign(full.session, { messages: full.messages }));
  };
  prompt.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) start(); });
  return h('div', { class: 'panel' },
    h('h3', {}, 'New agent session'),
    prompt,
    h('div', { class: 'row', style: 'margin-top:8px; flex-wrap:wrap' },
      h('div', { class: 'grow', style: 'min-width:200px' }, mode),
      h('div', { class: 'grow', style: 'min-width:200px' }, workdir),
      h('label', { class: 'row', style: 'margin:0' }, ctx, 'Include task context'),
      h('button', { onclick: start }, 'Start ⌘↵')));
}

function messageEl(m) {
  switch (m.kind) {
    case 'tool_use': {
      const sp = m.content.indexOf(' ');
      const name = sp > 0 ? m.content.slice(0, sp) : m.content;
      const input = sp > 0 ? m.content.slice(sp + 1) : '';
      let pretty = input;
      try { pretty = JSON.stringify(JSON.parse(input), null, 2); } catch (_) { /* keep raw */ }
      return h('details', { class: 'tool' }, h('summary', {}, `🔧 ${name} ${trunc(input, 120)}`), h('pre', {}, pretty));
    }
    case 'tool_result':
      return h('details', { class: 'tool' }, h('summary', {}, `↳ ${trunc(m.content, 120) || '(empty)'}`), h('pre', {}, m.content));
    case 'result':
      return h('div', { class: 'msg result' }, `✓ Turn finished · ${fmtTime(m.created_at)}`);
    case 'error':
      return h('div', { class: 'msg error' }, '✗ ' + m.content);
    default:
      return h('div', { class: 'msg ' + m.role }, richText(m.content));
  }
}

function sessionComponent(session, messages) {
  let last = 0;
  const body = h('div', { class: 'session-body' });
  const head = h('div', { class: 'session-head' });
  const input = h('textarea', { rows: 2, placeholder: 'Reply to continue this session… (⌘↵ to send)' });
  const send = h('button', {}, 'Send');
  const foot = h('div', { class: 'session-foot' }, input, h('div', { class: 'row', style: 'margin-top:6px; justify-content:flex-end' }, send));
  const el = h('div', { class: 'session' }, head, body, foot);
  const c = { el, session, lastId: () => last };

  const doSend = async () => {
    if (!input.value.trim()) return;
    await api('POST', `/api/sessions/${c.session.id}/messages`, { text: input.value });
    input.value = '';
    c.session.status = 'running';
    drawHead();
  };
  send.addEventListener('click', doSend);
  input.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) doSend(); });

  function drawHead() {
    const s = c.session;
    const running = s.status === 'running';
    const resumeCmd = s.claude_session_id ? `cd ${JSON.stringify(s.workdir)} && claude --resume ${s.claude_session_id}` : '';
    head.replaceChildren(...[
      h('span', { class: 'title' }, s.title || `Session ${s.id}`),
      h('span', { class: 'badge status-' + s.status }, running ? h('span', { class: 'pulse' }) : null, s.status),
      h('span', { class: 'badge' }, s.mode),
      s.trigger !== 'manual' ? h('span', { class: 'badge' }, s.trigger) : null,
      s.cost_usd ? h('span', { class: 'muted small' }, '$' + s.cost_usd.toFixed(2)) : null,
      h('span', { class: 'muted small' }, fmtTime(s.created_at)),
      resumeCmd ? h('button', {
        class: 'ghost small', title: resumeCmd,
        onclick: () => navigator.clipboard.writeText(resumeCmd).then(() => toast('Resume command copied')),
      }, 'Copy resume cmd') : null,
      running ? h('button', { class: 'danger small', onclick: () => api('POST', `/api/sessions/${s.id}/cancel`) }, 'Stop') : null,
      h('button', { class: 'ghost small', onclick: () => el.classList.toggle('collapsed') }, '▾'),
    ].filter(Boolean));
    send.disabled = running || !s.claude_session_id;
  }

  c.update = (s, msgs) => {
    c.session = s;
    for (const m of msgs) {
      body.append(messageEl(m));
      last = Math.max(last, m.id);
    }
    drawHead();
  };
  c.update(session, messages);
  return c;
}

function schedulesPanel(taskId, schedules) {
  const list = h('div');
  const draw = xs => list.replaceChildren(...xs.map(x => h('div', { class: 'sched' },
    h('input', {
      type: 'checkbox', checked: x.enabled, title: 'Enabled',
      onchange: async e => draw(await reload(await api('PUT', `/api/schedules/${x.id}`, { ...x, enabled: e.target.checked }))),
    }),
    h('div', { class: 'grow' },
      h('code', {}, x.cron), ' ', h('span', { class: 'badge' }, x.action),
      h('div', { class: 'muted small' }, trunc(x.prompt, 80)),
      h('div', { class: 'muted small' }, x.enabled && x.next_run_at ? 'Next: ' + fmtTime(x.next_run_at) : 'Paused')),
    h('button', { class: 'ghost small', onclick: async () => { await api('DELETE', `/api/schedules/${x.id}`); draw(await reload()); } }, '✕'))));
  const reload = async () => api('GET', `/api/tasks/${taskId}/schedules`);
  draw(schedules);

  const cron = h('input', { placeholder: '0 9 * * 1-5' });
  const action = select([['agent', 'Run agent'], ['remind', 'Remind me']], 'agent');
  const prompt = h('textarea', { rows: 2, placeholder: 'Prompt (agent) or reminder text. Template vars: {{.Context}}, {{.Task.Title}}, {{.Fields.pull_request}}' });
  const mode = select(modeOptions(true), '');
  return h('div', { class: 'panel' },
    h('h3', {}, 'Recurring'),
    list,
    h('label', {}, 'Cron (min hour day month weekday) or @daily, @hourly'),
    cron, h('label', {}, 'Action'), action, h('label', {}, 'Prompt / message'), prompt, h('label', {}, 'Agent mode'), mode,
    h('div', { class: 'row', style: 'margin-top:8px; justify-content:flex-end' }, h('button', {
      onclick: async () => {
        await api('POST', `/api/tasks/${taskId}/schedules`, {
          cron: cron.value, action: action.value, prompt: prompt.value, mode: mode.value, enabled: true,
        });
        cron.value = prompt.value = '';
        draw(await reload());
      },
    }, 'Add schedule')));
}

// ---------- sessions overview ----------

async function renderSessions() {
  const statusSel = select([['', 'All statuses'], ['running', 'Running'], ['idle', 'Idle'], ['error', 'Error'],
    ['cancelled', 'Cancelled'], ['interrupted', 'Interrupted']], '', { onchange: draw });
  const tbody = h('tbody');
  async function draw() {
    const xs = await api('GET', '/api/sessions' + (statusSel.value ? `?status=${statusSel.value}` : ''));
    tbody.replaceChildren(...xs.map(s => h('tr', { class: 'clickable', onclick: () => { location.hash = `#/task/${s.task_id}/s/${s.id}`; } },
      h('td', {}, h('span', { class: 'badge status-' + s.status }, s.status === 'running' ? h('span', { class: 'pulse' }) : null, s.status)),
      h('td', {}, h('div', {}, s.task_title), h('div', { class: 'muted small' }, s.title)),
      h('td', { class: 'small' }, trunc(s.last_message, 220)),
      h('td', { class: 'muted small' }, s.trigger, h('br'), s.mode),
      h('td', { class: 'muted small' }, ago(s.updated_at)))));
  }
  view.replaceChildren(h('div', { class: 'panel' },
    h('div', { class: 'toolbar' }, h('h2', { class: 'grow', style: 'flex:1' }, 'Agent sessions'), statusSel),
    h('table', {}, h('thead', {}, h('tr', {}, ['Status', 'Task / session', 'Latest', 'Trigger', 'Updated'].map(x => h('th', {}, x)))), tbody)));
  await draw();
  every(3000, draw);
}

// ---------- settings ----------

async function renderSettings() {
  const b = await loadBoard();
  const rules = await api('GET', '/api/rules');
  const wd = await api('GET', '/api/workdirs');
  const rerender = () => renderSettings();

  const lanes = h('div', { class: 'panel' }, h('h2', {}, 'Lanes'),
    h('p', { class: 'muted small' }, 'Board columns, left to right.'),
    b.lanes.map((l, i) => {
      const name = h('input', { value: l.name });
      const color = h('input', { type: 'color', value: l.color || '#888888', style: 'width:42px;padding:2px' });
      const saveLane = () => api('PATCH', `/api/lanes/${l.id}`, { name: name.value, color: color.value });
      name.addEventListener('change', saveLane);
      color.addEventListener('change', saveLane);
      const move = async d => {
        const ids = b.lanes.map(x => x.id);
        [ids[i], ids[i + d]] = [ids[i + d], ids[i]];
        await api('POST', '/api/lanes/reorder', { ids });
        rerender();
      };
      return h('div', { class: 'row', style: 'margin-bottom:6px' }, color, h('div', { class: 'grow' }, name),
        h('button', { class: 'ghost', disabled: i === 0, onclick: () => move(-1) }, '↑'),
        h('button', { class: 'ghost', disabled: i === b.lanes.length - 1, onclick: () => move(1) }, '↓'),
        h('button', { class: 'danger', onclick: async () => { await api('DELETE', `/api/lanes/${l.id}`); rerender(); } }, 'Delete'));
    }),
    h('input', {
      placeholder: '+ New lane (Enter)', onkeydown: async e => {
        if (e.key === 'Enter' && e.target.value.trim()) { await api('POST', '/api/lanes', { name: e.target.value }); rerender(); }
      },
    }));

  const saveWorkdirs = async dirs => {
    state.meta.workdirs = (await api('PUT', '/api/workdirs', { workdirs: dirs })).workdirs;
    rerender();
  };
  const newRepos = wd.repos.filter(r => !wd.workdirs.includes(r));
  const workdirs = h('div', { class: 'panel' },
    h('div', { class: 'row' }, h('h2', { class: 'grow' }, 'Working directories'),
      h('button', { class: 'ghost', disabled: !newRepos.length, onclick: () => saveWorkdirs([...wd.workdirs, ...newRepos]) },
        newRepos.length ? `+ Add ${newRepos.length} new repos from ${wd.workspace}` : `All repos from ${wd.workspace} added`)),
    h('p', { class: 'muted small' }, 'Choices in the working-directory dropdowns on tasks, sessions and rules.'),
    wd.workdirs.map(d => h('div', { class: 'row', style: 'margin-bottom:6px' }, h('code', { class: 'grow' }, d),
      h('button', { class: 'danger small', onclick: () => saveWorkdirs(wd.workdirs.filter(x => x !== d)) }, 'Remove'))),
    h('input', {
      placeholder: '+ Add directory, absolute path (Enter)', onkeydown: e => {
        if (e.key === 'Enter' && e.target.value.trim()) saveWorkdirs([...wd.workdirs, e.target.value]);
      },
    }));

  const fKey = h('input', { placeholder: 'key, e.g. figma' });
  const fLabel = h('input', { placeholder: 'Label' });
  const fType = select([['url', 'Link'], ['text', 'Text']], 'url');
  const fields = h('div', { class: 'panel' }, h('h2', {}, 'Task fields'),
    h('p', { class: 'muted small' }, 'Extra attributes every task can have: links to Productive, PRs, Slack threads…'),
    h('table', {}, h('tbody', {}, b.fields.map(f => h('tr', {},
      h('td', {}, h('code', {}, f.key)), h('td', {}, f.label), h('td', { class: 'muted' }, f.type),
      h('td', {}, h('button', {
        class: 'danger small', onclick: async () => {
          if (confirm(`Delete field ${f.key} and its values on all tasks?`)) { await api('DELETE', `/api/fields/${f.key}`); rerender(); }
        },
      }, 'Delete')))))),
    h('div', { class: 'row', style: 'margin-top:10px' }, h('div', { class: 'grow' }, fKey), h('div', { class: 'grow' }, fLabel), fType,
      h('button', { onclick: async () => { await api('POST', '/api/fields', { key: fKey.value, label: fLabel.value, type: fType.value }); rerender(); } }, 'Add')));

  const laneLabel = id => id == null ? 'Any lane' : (b.lanes.find(l => l.id === id) || {}).name || '?';
  const rulesPanel = h('div', { class: 'panel' },
    h('div', { class: 'row' }, h('h2', { class: 'grow' }, 'Transition rules'), h('button', { onclick: () => ruleDialog(null, b.lanes, rerender) }, '+ Add rule')),
    h('p', { class: 'muted small' }, 'Start an agent session automatically when a task moves between lanes.'),
    rules.length ? h('table', {}, h('thead', {}, h('tr', {}, ['', 'Rule', 'Transition', 'Prompt', 'Mode', ''].map(x => h('th', {}, x)))),
      h('tbody', {}, rules.map(r => h('tr', {},
        h('td', {}, h('input', { type: 'checkbox', checked: r.enabled, onchange: async e => { await api('PUT', `/api/rules/${r.id}`, { ...r, enabled: e.target.checked }); } })),
        h('td', {}, r.name),
        h('td', {}, `${laneLabel(r.from_lane_id)} → ${laneLabel(r.to_lane_id)}`),
        h('td', { class: 'small' }, h('code', {}, trunc(r.prompt, 80))),
        h('td', { class: 'muted small' }, r.mode || 'app default'),
        h('td', {}, h('button', { class: 'ghost small', onclick: () => ruleDialog(r, b.lanes, rerender) }, 'Edit'), ' ',
          h('button', { class: 'danger small', onclick: async () => { await api('DELETE', `/api/rules/${r.id}`); rerender(); } }, 'Delete'))))))
      : h('p', { class: 'muted' }, 'No rules yet.'));

  view.replaceChildren(h('div', { style: 'max-width:900px' }, lanes, rulesPanel, workdirs, fields));
}

function ruleDialog(rule, lanes, done) {
  rule = rule || { name: '', from_lane_id: null, to_lane_id: lanes[0].id, prompt: '{{.Context}}\n\n', mode: '', workdir: '', enabled: true };
  const dlg = document.getElementById('dialog');
  const name = h('input', { value: rule.name, placeholder: 'e.g. Start coding' });
  const from = select([['', 'Any lane'], ...lanes.map(l => [l.id, l.name])], rule.from_lane_id ?? '');
  const to = select(lanes.map(l => [l.id, l.name]), rule.to_lane_id);
  const prompt = h('textarea', { rows: 6 }, rule.prompt);
  const mode = select(modeOptions(true), rule.mode);
  const workdir = workdirSelect(rule.workdir, 'Task workdir, else app default');
  dlg.replaceChildren(h('h2', {}, rule.id ? 'Edit rule' : 'New rule'),
    h('label', {}, 'Name'), name,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'From'), from), h('div', { class: 'grow' }, h('label', {}, 'To'), to)),
    h('label', {}, 'Prompt template — use a skill like /p-dev:code, and {{.Context}}, {{.Task.Title}}, {{.Task.Description}}, {{.Fields.pull_request}}'),
    prompt,
    h('label', {}, 'Agent mode'), mode,
    h('label', {}, 'Working directory'), workdir,
    h('div', { class: 'dialog-actions' },
      h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel'),
      h('button', {
        onclick: async () => {
          const body = {
            ...rule, name: name.value, prompt: prompt.value, mode: mode.value, workdir: workdir.value,
            from_lane_id: from.value ? Number(from.value) : null, to_lane_id: Number(to.value),
          };
          await api(rule.id ? 'PUT' : 'POST', rule.id ? `/api/rules/${rule.id}` : '/api/rules', body);
          dlg.close();
          done();
        },
      }, 'Save')));
  dlg.showModal();
}

// ---------- new task dialog ----------

async function newTaskDialog() {
  if (!state.board) await loadBoard();
  const dlg = document.getElementById('dialog');
  const title = h('input', { placeholder: 'Title' });
  const desc = h('textarea', { rows: 4, placeholder: 'Description' });
  const kind = select(state.meta.kinds.map(k => [k, k]), 'task');
  const lane = select(state.board.lanes.map(l => [l.id, l.name]), state.board.lanes[0]?.id);
  const create = async () => {
    if (!title.value.trim()) return;
    const t = await api('POST', '/api/tasks', { title: title.value, description: desc.value, kind: kind.value, lane_id: Number(lane.value) });
    dlg.close();
    location.hash = `#/task/${t.id}`;
  };
  title.addEventListener('keydown', e => { if (e.key === 'Enter') create(); });
  dlg.replaceChildren(h('h2', {}, 'New task'), h('label', {}, 'Title'), title, h('label', {}, 'Description'), desc,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'Kind'), kind), h('div', { class: 'grow' }, h('label', {}, 'Lane'), lane)),
    h('div', { class: 'dialog-actions' }, h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel'), h('button', { onclick: create }, 'Create')));
  dlg.showModal();
  title.focus();
}

// ---------- notifications & global counters ----------

async function refreshGlobal() {
  try {
    const [notes, running] = await Promise.all([api('GET', '/api/notifications?unread=1'), api('GET', '/api/sessions?status=running')]);
    const nc = document.getElementById('notif-count');
    nc.textContent = notes.length;
    nc.classList.toggle('hidden', !notes.length);
    const rc = document.getElementById('running-count');
    rc.textContent = running.length;
    rc.classList.toggle('hidden', !running.length);
  } catch (_) { /* server restarting */ }
}

async function toggleNotifications() {
  const panel = document.getElementById('notif-panel');
  if (!panel.classList.contains('hidden')) { panel.classList.add('hidden'); return; }
  const notes = await api('GET', '/api/notifications');
  panel.replaceChildren(...(notes.length ? notes.map(n => h('div', {
    class: 'notif' + (n.read_at ? '' : ' unread'), style: 'cursor:pointer',
    onclick: async () => {
      if (!n.read_at) await api('POST', `/api/notifications/${n.id}/read`);
      panel.classList.add('hidden');
      refreshGlobal();
      if (n.task_id) location.hash = `#/task/${n.task_id}`;
    },
  }, h('div', {}, n.message), h('div', { class: 'muted small' }, (n.task_title ? n.task_title + ' · ' : '') + ago(n.created_at))))
    : [h('div', { class: 'notif muted' }, 'Nothing here.')]));
  panel.classList.remove('hidden');
}

// ---------- boot ----------

(async function boot() {
  state.meta = await api('GET', '/api/meta');
  document.getElementById('new-task-btn').addEventListener('click', newTaskDialog);
  document.getElementById('notif-btn').addEventListener('click', toggleNotifications);
  window.addEventListener('hashchange', route);
  route();
  refreshGlobal();
  setInterval(refreshGlobal, 5000);
})();

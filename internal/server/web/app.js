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

// <input type="datetime-local"> works in local time without a zone.
function toLocalInput(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  d.setMinutes(d.getMinutes() - d.getTimezoneOffset());
  return d.toISOString().slice(0, 16);
}

function fromLocalInput(v) {
  return v ? new Date(v).toISOString() : null;
}

function select(options, value, attrs) {
  return h('select', attrs, options.map(([v, label]) => h('option', { value: v, selected: String(v) === String(value) }, label)));
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

// Where the task goes when an agent turn finishes successfully; '' keeps it in place.
function doneLaneSelect(lanes, value, attrs) {
  return select([['', 'When done: stay in lane'], ...lanes.map(l => [l.id, `When done: move to ${l.name}`])], value ?? '', attrs);
}
const doneLaneValue = sel => sel.value ? Number(sel.value) : null;

// Free-text input with suggestions; empty means the CLI's default model.
function modelInput(value) {
  const list = h('datalist', { id: 'models-' + Math.random().toString(36).slice(2) }, state.meta.models.map(m => h('option', { value: m })));
  return [h('input', { value: value || '', placeholder: 'Model — CLI default', list: list.id }), list];
}

// Directory input suggesting recently used working directories.
function workdirInput(value, placeholder) {
  const list = h('datalist', { id: 'workdirs-' + Math.random().toString(36).slice(2) });
  api('GET', '/api/workdirs').then(dirs => list.replaceChildren(...dirs.map(d => h('option', { value: d })))).catch(() => {});
  return [h('input', { value: value || '', placeholder, list: list.id }), list];
}

function remember(key, value) {
  try { localStorage.setItem(key, value); } catch (_) { /* storage unavailable */ }
}

function recall(key) {
  try { return localStorage.getItem(key) || ''; } catch (_) { return ''; }
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
  closeTaskDialog();
  const parts = (location.hash.replace(/^#\/?/, '') || 'board').split('/');
  document.querySelectorAll('[data-nav]').forEach(a => a.classList.toggle('active', a.dataset.nav === parts[0]));
  view.replaceChildren();
  const r = {
    board: renderBoard,
    list: renderList,
    task: () => renderTask(Number(parts[1]), Number(parts[3]) || 0),
    sessions: renderSessions,
    session: () => renderSession(Number(parts[1])),
    archive: renderArchive,
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

function dueBadge(t) {
  if (!t.due_at) return null;
  const overdue = new Date(t.due_at) <= Date.now();
  return h('span', { class: 'badge due' + (overdue ? ' overdue' : ''), title: 'Due ' + fmtTime(t.due_at) }, '⏰ ' + ago(t.due_at));
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
  // A click opens the task in a dialog over the board; ⌘/Ctrl-click goes straight to its full page.
  const el = h('article', {
    class: 'card', draggable: 'true',
    onclick: e => { if (e.metaKey || e.ctrlKey) location.hash = `#/task/${t.id}`; else openTaskDialog(t.id); },
  },
    h('div', { class: 'card-title' }, t.title),
    h('div', { class: 'card-meta' }, kindBadge(t.kind), dueBadge(t), runningBadge(state.board.running[t.id]), fieldLinks(t)));
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
      h('td', { class: 'small', title: fmtTime(t.due_at) }, dueBadge(t)),
      h('td', {}, runningBadge(b.running[t.id]), fieldLinks(t)),
      h('td', { class: 'muted small' }, ago(t.updated_at)))));
  }
  draw();
  view.replaceChildren(h('div', { class: 'panel' },
    h('div', { class: 'toolbar' }, filter, laneSel),
    h('table', {}, h('thead', {}, h('tr', {}, ['Task', 'Lane', 'Kind', 'Due', 'Links', 'Updated'].map(x => h('th', {}, x)))), tbody)));
  filter.focus();
}

// ---------- archive ----------

async function renderArchive() {
  const [b, tasks] = await Promise.all([loadBoard(), api('GET', '/api/archive')]);
  const filter = h('input', { placeholder: 'Filter…', oninput: draw });
  const tbody = h('tbody');
  function draw() {
    const q = filter.value.toLowerCase();
    const rows = tasks.filter(t => !q || (t.title + ' ' + t.description).toLowerCase().includes(q));
    tbody.replaceChildren(...rows.map(t => h('tr', { class: 'clickable', onclick: () => { location.hash = `#/task/${t.id}`; } },
      h('td', {}, h('div', {}, t.title), t.description ? h('div', { class: 'muted small' }, trunc(t.description, 120)) : null),
      h('td', {}, laneName(t.lane_id)),
      h('td', {}, kindBadge(t.kind) || 'task'),
      h('td', {}, fieldLinks(t)),
      h('td', { class: 'muted small', title: fmtTime(t.archived_at) }, ago(t.archived_at)),
      h('td', {}, h('button', {
        class: 'ghost small', title: 'Back to the end of its lane',
        onclick: async e => {
          e.stopPropagation();
          await api('POST', `/api/tasks/${t.id}/unarchive`);
          tasks.splice(tasks.indexOf(t), 1);
          toast(`Restored to ${laneName(t.lane_id)}`);
          draw();
        },
      }, 'Restore')))));
  }
  draw();
  view.replaceChildren(h('div', { class: 'panel' },
    h('div', { class: 'toolbar' }, h('h2', { class: 'grow', style: 'flex:1' }, 'Archive'), filter),
    tasks.length
      ? h('table', {}, h('thead', {}, h('tr', {}, ['Task', 'Lane', 'Kind', 'Links', 'Archived', ''].map(x => h('th', {}, x)))), tbody)
      : h('p', { class: 'muted' }, 'Nothing archived yet.')));
  filter.focus();
}

// ---------- task page ----------

async function renderTask(id, focusSession) {
  const page = await taskView(id, {
    every,
    leave: () => { location.hash = '#/board'; },
    reload: route,
  });
  view.replaceChildren(page.el);
  page.focus(focusSession);
}

// ---------- task dialog: the board opens a task here first ----------

const taskDialog = { el: null, timers: [] };

async function openTaskDialog(id) {
  closeTaskDialog();
  const dlg = taskDialog.el || (taskDialog.el = document.getElementById('task-dialog'));
  if (!dlg.dataset.wired) {
    dlg.dataset.wired = '1';
    dlg.addEventListener('close', () => {
      taskDialog.timers.forEach(clearInterval);
      taskDialog.timers = [];
      dlg.replaceChildren();
      if (location.hash === '' || location.hash.startsWith('#/board')) renderBoardInPlace();
    });
    // A click on the backdrop, outside the dialog's box, closes it.
    dlg.addEventListener('click', e => {
      const r = dlg.getBoundingClientRect();
      if (e.target === dlg && (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom)) dlg.close();
    });
  }
  const page = await taskView(id, {
    every: (ms, fn) => taskDialog.timers.push(setInterval(fn, ms)),
    leave: () => dlg.close(),
    reload: () => openTaskDialog(id),
  });
  dlg.replaceChildren(
    h('div', { class: 'task-dialog-bar' },
      h('a', { class: 'btn ghost small', href: `#/task/${id}`, title: 'Open the task as a full page' }, '⤢ Full page'),
      h('button', { class: 'ghost small', title: 'Close (Esc)', onclick: () => dlg.close() }, '✕')),
    page.el);
  if (!dlg.open) dlg.showModal();
}

function closeTaskDialog() {
  if (taskDialog.el && taskDialog.el.open) taskDialog.el.close();
}

// The task's editor and its sessions, for the full page or the board's dialog.
// `every` polls for as long as the host shows it, `leave` runs once the task is
// archived or deleted, and `reload` redraws it after a restore.
async function taskView(id, { every, leave, reload }) {
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

  const due = h('input', { type: 'datetime-local', value: toLocalInput(t.due_at), onchange: () => save({ due_at: fromLocalInput(due.value) }) });
  const clearDue = h('button', { class: 'ghost small', title: 'No due time', onclick: async () => { due.value = ''; await save({ due_at: null }); } }, '✕');

  const archived = t.archived_at
    ? h('div', { class: 'row archived-note' }, h('span', { class: 'grow' }, 'Archived ' + ago(t.archived_at)),
      h('button', { class: 'small', onclick: async () => { await api('POST', `/api/tasks/${id}/unarchive`); reload(); } }, 'Restore'))
    : null;
  side.append(h('div', { class: 'panel' },
    archived,
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
    h('label', {}, 'Due — you get a notification at this time'),
    h('div', { class: 'field-row' }, due, clearDue),
    h('label', {}, 'Description'),
    h('textarea', { rows: 6, onchange: e => save({ description: e.target.value }) }, t.description),
    h('label', {}, 'Working directory for agents'),
    h('input', { value: t.workdir, placeholder: state.meta.workdir, onchange: e => save({ workdir: e.target.value }) }),
    fields,
    h('div', { class: 'row', style: 'margin-top:14px' },
      h('span', { class: 'muted small grow' }, 'Created ' + fmtTime(t.created_at)),
      t.archived_at ? null : h('button', {
        class: 'ghost', title: 'Take it off the board; find it under Archive',
        onclick: async () => { await api('POST', `/api/tasks/${id}/archive`); leave(); },
      }, 'Archive'),
      h('button', {
        class: 'danger', onclick: async () => {
          if (!confirm('Delete this task and all its sessions?')) return;
          await api('DELETE', `/api/tasks/${id}`);
          leave();
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

  return {
    el: h('div', { class: 'task-page' }, side, main),
    focus: sessionId => { if (sessionId && components.has(sessionId)) components.get(sessionId).el.scrollIntoView(); },
  };
}

function newSessionPanel(task, onStarted) {
  const prompt = h('textarea', { rows: 4, placeholder: 'What should the agent do? Skills work too, e.g. /p-dev:review 123' });
  const mode = select(modeOptions(false), state.meta.default_mode);
  const [model, modelList] = modelInput('');
  const ctx = h('input', { type: 'checkbox', checked: true });
  const [workdir, workdirList] = workdirInput('', task.workdir || state.meta.workdir);
  const doneLane = doneLaneSelect(state.board.lanes, '');
  const start = async () => {
    if (!prompt.value.trim()) return;
    const s = await api('POST', `/api/tasks/${task.id}/sessions`, {
      prompt: prompt.value, mode: mode.value, model: model.value, workdir: workdir.value, include_context: ctx.checked,
      done_lane_id: doneLaneValue(doneLane),
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
      h('div', { class: 'grow', style: 'min-width:140px' }, model, modelList),
      h('div', { class: 'grow', style: 'min-width:200px' }, workdir, workdirList),
      h('div', { class: 'grow', style: 'min-width:200px' }, doneLane),
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
  // Task sessions can change where the task goes when the agent finishes, even mid-run.
  const doneLane = session.task_id && state.board ? doneLaneSelect(state.board.lanes, session.done_lane_id, {
    class: 'small', style: 'width:auto',
    onchange: async e => { c.session = await api('PATCH', `/api/sessions/${c.session.id}`, { done_lane_id: doneLaneValue(e.target) }); },
  }) : null;
  const foot = h('div', { class: 'session-foot' }, input,
    h('div', { class: 'row', style: 'margin-top:6px; justify-content:flex-end' }, doneLane, send));
  const el = h('div', { class: 'session' }, head, body, foot);
  const c = { el, session, lastId: () => last };

  const doResume = async () => {
    await api('POST', `/api/sessions/${c.session.id}/resume`);
    c.session.status = 'running';
    drawHead();
  };

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
    const resumable = ['interrupted', 'cancelled', 'error'].includes(s.status);
    const resumeCmd = s.claude_session_id ? `cd ${JSON.stringify(s.workdir)} && claude --resume ${s.claude_session_id}` : '';
    head.replaceChildren(...[
      h('span', { class: 'title' }, s.title || `Session ${s.id}`),
      h('span', { class: 'badge status-' + s.status }, running ? h('span', { class: 'pulse' }) : null, s.status),
      h('span', { class: 'badge' }, s.mode),
      s.model ? h('span', { class: 'badge' }, s.model) : null,
      s.trigger !== 'manual' ? h('span', { class: 'badge' }, s.trigger) : null,
      s.cost_usd ? h('span', { class: 'muted small' }, '$' + s.cost_usd.toFixed(2)) : null,
      h('span', { class: 'muted small' }, fmtTime(s.created_at)),
      resumeCmd ? h('button', {
        class: 'ghost small', title: resumeCmd,
        onclick: () => navigator.clipboard.writeText(resumeCmd).then(() => toast('Resume command copied')),
      }, 'Copy resume cmd') : null,
      running ? h('button', { class: 'danger small', onclick: () => api('POST', `/api/sessions/${s.id}/cancel`) }, 'Stop') : null,
      resumable ? h('button', { class: 'small', title: 'Continue this session where it stopped', onclick: doResume }, 'Resume') : null,
      h('button', { class: 'ghost small', onclick: () => el.classList.toggle('collapsed') }, '▾'),
    ].filter(Boolean));
    send.disabled = running || !s.claude_session_id;
  }

  c.update = (s, msgs) => {
    c.session = s;
    if (doneLane && document.activeElement !== doneLane) doneLane.value = s.done_lane_id ?? '';
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
      x.done_lane_id ? h('span', { class: 'badge' }, '→ ' + laneName(x.done_lane_id)) : null,
      h('div', { class: 'muted small' }, trunc(x.prompt, 80)),
      h('div', { class: 'muted small' }, x.enabled && x.next_run_at ? 'Next: ' + fmtTime(x.next_run_at) : 'Paused')),
    h('button', { class: 'ghost small', onclick: async () => { await api('DELETE', `/api/schedules/${x.id}`); draw(await reload()); } }, '✕'))));
  const reload = async () => api('GET', `/api/tasks/${taskId}/schedules`);
  draw(schedules);

  const cron = h('input', { placeholder: '0 9 * * 1-5' });
  const action = select([['agent', 'Run agent'], ['remind', 'Remind me']], 'agent');
  const prompt = h('textarea', { rows: 2, placeholder: 'Prompt (agent) or reminder text. Template vars: {{.Context}}, {{.Task.Title}}, {{.Fields.pull_request}}' });
  const mode = select(modeOptions(true), '');
  const doneLane = doneLaneSelect(state.board.lanes, '');
  return h('div', { class: 'panel' },
    h('h3', {}, 'Recurring'),
    list,
    h('label', {}, 'Cron (min hour day month weekday) or @daily, @hourly'),
    cron, h('label', {}, 'Action'), action, h('label', {}, 'Prompt / message'), prompt, h('label', {}, 'Agent mode'), mode,
    h('label', {}, 'After the agent finishes'), doneLane,
    h('div', { class: 'row', style: 'margin-top:8px; justify-content:flex-end' }, h('button', {
      onclick: async () => {
        await api('POST', `/api/tasks/${taskId}/schedules`, {
          cron: cron.value, action: action.value, prompt: prompt.value, mode: mode.value, enabled: true,
          done_lane_id: action.value === 'agent' ? doneLaneValue(doneLane) : null,
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
    tbody.replaceChildren(...xs.map(s => h('tr', { class: 'clickable', onclick: () => { location.hash = sessionHref(s); } },
      h('td', {}, h('span', { class: 'badge status-' + s.status }, s.status === 'running' ? h('span', { class: 'pulse' }) : null, s.status)),
      h('td', {}, s.task_id ? h('div', {}, s.task_title) : h('div', { class: 'muted' }, 'No task'), h('div', { class: 'muted small' }, s.title)),
      h('td', { class: 'small' }, trunc(s.last_message, 220)),
      h('td', { class: 'muted small' }, s.trigger, h('br'), s.mode),
      h('td', { class: 'muted small' }, ago(s.updated_at)))));
  }
  view.replaceChildren(h('div', { class: 'panel' },
    h('div', { class: 'toolbar' }, h('h2', { class: 'grow', style: 'flex:1' }, 'Agent sessions'), statusSel,
      h('button', { onclick: sessionDialog }, '▶ New session')),
    h('table', {}, h('thead', {}, h('tr', {}, ['Status', 'Task / session', 'Latest', 'Trigger', 'Updated'].map(x => h('th', {}, x)))), tbody)));
  await draw();
  every(3000, draw);
}

function sessionHref(s) {
  return s.task_id ? `#/task/${s.task_id}/s/${s.id}` : `#/session/${s.id}`;
}

// ---------- standalone session ----------

async function renderSession(id) {
  const data = await api('GET', `/api/sessions/${id}`);
  if (data.session.task_id) { location.hash = sessionHref(data.session); return; }
  const c = sessionComponent(data.session, data.messages);
  view.replaceChildren(h('div', { class: 'session-page' },
    h('div', { class: 'toolbar' },
      h('a', { href: '#/sessions', class: 'btn ghost small' }, '← Sessions'),
      h('span', { class: 'muted small grow', style: 'flex:1' }, data.session.workdir),
      h('button', { onclick: sessionDialog }, '▶ New session')),
    c.el));
  every(2000, async () => {
    const upd = await api('GET', `/api/sessions/${id}?after=${c.lastId()}`);
    if (upd.messages.length || upd.session.status !== c.session.status || upd.session.updated_at !== c.session.updated_at) {
      c.update(upd.session, upd.messages);
    }
  });
}

function sessionDialog() {
  const dlg = document.getElementById('dialog');
  const prompt = h('textarea', { rows: 6, placeholder: 'What should the agent do? Skills work too, e.g. /p-dev:review 123' });
  const [workdir, workdirList] = workdirInput(recall('session.workdir'), state.meta.workdir);
  const [model, modelList] = modelInput(recall('session.model'));
  const mode = select(modeOptions(false), recall('session.mode') || state.meta.default_mode);
  const start = async () => {
    if (!prompt.value.trim()) return;
    const s = await api('POST', '/api/sessions', { prompt: prompt.value, workdir: workdir.value, model: model.value, mode: mode.value });
    remember('session.workdir', workdir.value);
    remember('session.model', model.value);
    remember('session.mode', mode.value);
    dlg.close();
    location.hash = `#/session/${s.id}`;
  };
  prompt.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) start(); });
  dlg.replaceChildren(h('h2', {}, 'New agent session'),
    h('label', {}, 'Prompt'), prompt,
    h('label', {}, 'Directory'), workdir, workdirList,
    h('div', { class: 'row' },
      h('div', { class: 'grow' }, h('label', {}, 'Model'), model, modelList),
      h('div', { class: 'grow' }, h('label', {}, 'Mode'), mode)),
    h('div', { class: 'dialog-actions' }, h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel'), h('button', { onclick: start }, 'Start ⌘↵')));
  dlg.showModal();
  prompt.focus();
}

// ---------- settings ----------

async function renderSettings() {
  const b = await loadBoard();
  const [rules, settings] = await Promise.all([api('GET', '/api/rules'), api('GET', '/api/settings')]);
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
    rules.length ? h('table', {}, h('thead', {}, h('tr', {}, ['', 'Rule', 'Transition', 'Prompt', 'Mode', 'When done', ''].map(x => h('th', {}, x)))),
      h('tbody', {}, rules.map(r => h('tr', {},
        h('td', {}, h('input', { type: 'checkbox', checked: r.enabled, onchange: async e => { await api('PUT', `/api/rules/${r.id}`, { ...r, enabled: e.target.checked }); } })),
        h('td', {}, r.name),
        h('td', {}, `${laneLabel(r.from_lane_id)} → ${laneLabel(r.to_lane_id)}`),
        h('td', { class: 'small' }, h('code', {}, trunc(r.prompt, 80))),
        h('td', { class: 'muted small' }, r.mode || 'app default'),
        h('td', { class: 'muted small' }, r.done_lane_id ? '→ ' + laneLabel(r.done_lane_id) : 'stay'),
        h('td', {}, h('button', { class: 'ghost small', onclick: () => ruleDialog(r, b.lanes, rerender) }, 'Edit'), ' ',
          h('button', { class: 'danger small', onclick: async () => { await api('DELETE', `/api/rules/${r.id}`); rerender(); } }, 'Delete'))))))
      : h('p', { class: 'muted' }, 'No rules yet.'));

  view.replaceChildren(h('div', { style: 'max-width:900px' }, lanes, rulesPanel, archivePanel(b.lanes, settings), fields));
}

// Auto-archive: which lane, and how long a task stays there first.
function archivePanel(lanes, settings) {
  const units = [[1440, 'days'], [60, 'hours'], [1, 'minutes']];
  const mins = settings.archive_after_minutes;
  const [unit] = units.find(([u]) => mins > 0 && mins % u === 0) || units[0];
  const lane = select([['', 'Off — never archive automatically'], ...lanes.map(l => [l.id, l.name])], settings.archive_lane_id ?? '');
  const amount = h('input', { type: 'number', min: 1, step: 1, value: mins > 0 ? mins / unit : 7, style: 'width:90px' });
  const unitSel = select(units, unit, { style: 'width:auto' });
  const save = async () => {
    const n = Math.round(Number(amount.value));
    if (!(n > 0)) { toast('Enter a positive number'); return; }
    await api('PUT', '/api/settings', { archive_lane_id: lane.value ? Number(lane.value) : null, archive_after_minutes: n * Number(unitSel.value) });
    toast('Saved');
  };
  [lane, amount, unitSel].forEach(el => el.addEventListener('change', save));
  return h('div', { class: 'panel' }, h('h2', {}, 'Archive'),
    h('p', { class: 'muted small' }, 'Tasks that stay in this lane long enough move to the Archive (top menu). Restore brings one back.'),
    h('div', { class: 'row' },
      h('div', { class: 'grow' }, h('label', {}, 'Lane'), lane),
      h('div', {}, h('label', {}, 'After'), h('div', { class: 'row' }, amount, unitSel))));
}

function ruleDialog(rule, lanes, done) {
  // Automated sessions usually end in review, so new rules default to a Review lane if there is one.
  const review = lanes.find(l => /review/i.test(l.name));
  rule = rule || { name: '', from_lane_id: null, to_lane_id: lanes[0].id, prompt: '{{.Context}}\n\n', mode: '', workdir: '',
    done_lane_id: review ? review.id : null, enabled: true };
  const dlg = document.getElementById('dialog');
  const name = h('input', { value: rule.name, placeholder: 'e.g. Start coding' });
  const from = select([['', 'Any lane'], ...lanes.map(l => [l.id, l.name])], rule.from_lane_id ?? '');
  const to = select(lanes.map(l => [l.id, l.name]), rule.to_lane_id);
  const prompt = h('textarea', { rows: 6 }, rule.prompt);
  const mode = select(modeOptions(true), rule.mode);
  const workdir = h('input', { value: rule.workdir, placeholder: 'Task workdir, else app default' });
  const doneLane = doneLaneSelect(lanes, rule.done_lane_id);
  dlg.replaceChildren(h('h2', {}, rule.id ? 'Edit rule' : 'New rule'),
    h('label', {}, 'Name'), name,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'From'), from), h('div', { class: 'grow' }, h('label', {}, 'To'), to)),
    h('label', {}, 'Prompt template — use a skill like /p-dev:code, and {{.Context}}, {{.Task.Title}}, {{.Task.Description}}, {{.Fields.pull_request}}'),
    prompt,
    h('label', {}, 'Agent mode'), mode,
    h('label', {}, 'Working directory'), workdir,
    h('label', {}, 'After the agent finishes (skipped if the task was moved meanwhile)'), doneLane,
    h('div', { class: 'dialog-actions' },
      h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel'),
      h('button', {
        onclick: async () => {
          const body = {
            ...rule, name: name.value, prompt: prompt.value, mode: mode.value, workdir: workdir.value,
            from_lane_id: from.value ? Number(from.value) : null, to_lane_id: Number(to.value), done_lane_id: doneLaneValue(doneLane),
          };
          await api(rule.id ? 'PUT' : 'POST', rule.id ? `/api/rules/${rule.id}` : '/api/rules', body);
          dlg.close();
          done();
        },
      }, 'Save')));
  dlg.showModal();
}

// ---------- quick reminder ----------

function askNotificationPermission() {
  if ('Notification' in window && Notification.permission === 'default') Notification.requestPermission();
}

function reminderPresets() {
  const inMin = m => new Date(Date.now() + m * 60000);
  const tomorrow9 = new Date();
  tomorrow9.setDate(tomorrow9.getDate() + 1);
  tomorrow9.setHours(9, 0, 0, 0);
  return [['15 min', inMin(15)], ['30 min', inMin(30)], ['1 h', inMin(60)], ['3 h', inMin(180)], ['Tomorrow 9:00', tomorrow9]];
}

async function reminderDialog() {
  if (!state.board) await loadBoard();
  askNotificationPermission();
  const dlg = document.getElementById('dialog');
  const title = h('input', { placeholder: 'e.g. Call Ana about the release' });
  const when = h('input', { type: 'datetime-local', value: toLocalInput(Date.now() + 3600000) });
  const create = async () => {
    if (!title.value.trim() || !when.value) return;
    await api('POST', '/api/tasks', { title: title.value, kind: 'reminder', due_at: fromLocalInput(when.value) });
    dlg.close();
    toast('Reminder set for ' + fmtTime(fromLocalInput(when.value)));
    if (location.hash === '' || location.hash.startsWith('#/board')) renderBoardInPlace();
  };
  title.addEventListener('keydown', e => { if (e.key === 'Enter') create(); });
  when.addEventListener('keydown', e => { if (e.key === 'Enter') create(); });
  dlg.replaceChildren(h('h2', {}, 'Remind me'),
    h('label', {}, 'What'), title,
    h('label', {}, 'When'), when,
    h('div', { class: 'presets' }, reminderPresets().map(([label, d]) => h('button', {
      class: 'ghost small', onclick: () => { when.value = toLocalInput(d); title.focus(); },
    }, label))),
    h('div', { class: 'dialog-actions' }, h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel'), h('button', { onclick: create }, 'Set reminder')));
  dlg.showModal();
  title.focus();
}

// ---------- new task dialog ----------

async function newTaskDialog() {
  if (!state.board) await loadBoard();
  const dlg = document.getElementById('dialog');
  const title = h('input', { placeholder: 'Title' });
  const desc = h('textarea', { rows: 4, placeholder: 'Description' });
  const kind = select(state.meta.kinds.map(k => [k, k]), 'task');
  const lane = select(state.board.lanes.map(l => [l.id, l.name]), state.board.lanes[0]?.id);
  const due = h('input', { type: 'datetime-local' });
  const create = async () => {
    if (!title.value.trim()) return;
    if (due.value) askNotificationPermission();
    const t = await api('POST', '/api/tasks', {
      title: title.value, description: desc.value, kind: kind.value, lane_id: Number(lane.value), due_at: fromLocalInput(due.value),
    });
    dlg.close();
    location.hash = `#/task/${t.id}`;
  };
  title.addEventListener('keydown', e => { if (e.key === 'Enter') create(); });
  dlg.replaceChildren(h('h2', {}, 'New task'), h('label', {}, 'Title'), title, h('label', {}, 'Description'), desc,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'Kind'), kind), h('div', { class: 'grow' }, h('label', {}, 'Lane'), lane)),
    h('label', {}, 'Due (optional) — you get a notification at this time'), due,
    h('div', { class: 'dialog-actions' }, h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel'), h('button', { onclick: create }, 'Create')));
  dlg.showModal();
  title.focus();
}

// ---------- notifications & global counters ----------

let lastNoteId = null; // highest notification id already announced; null until the first poll

function announce(n) {
  toast(n.message);
  if (!('Notification' in window) || Notification.permission !== 'granted') return;
  // The tag makes the OS show it once even with several tabs open.
  const desktop = new Notification(n.message, { body: n.task_title || '', tag: 'assistant-' + n.id });
  desktop.onclick = () => {
    window.focus();
    if (n.task_id) location.hash = `#/task/${n.task_id}`;
  };
}

async function refreshGlobal() {
  try {
    const [notes, running] = await Promise.all([api('GET', '/api/notifications?unread=1'), api('GET', '/api/sessions?status=running')]);
    const maxId = notes.reduce((m, n) => Math.max(m, n.id), 0);
    if (lastNoteId !== null) notes.filter(n => n.id > lastNoteId).reverse().forEach(announce);
    lastNoteId = Math.max(lastNoteId ?? 0, maxId);
    document.title = notes.length ? `(${notes.length}) Assistant` : 'Assistant';
    const nc = document.getElementById('notif-count');
    nc.textContent = notes.length;
    nc.classList.toggle('hidden', !notes.length);
    const rc = document.getElementById('running-count');
    rc.textContent = running.length;
    rc.classList.toggle('hidden', !running.length);
  } catch (_) { /* server restarting */ }
}

// Short countdown: 45m, 2h10m, 3d5h.
function untilShort(ts) {
  const m = Math.max(0, Math.floor((new Date(ts) - Date.now()) / 60000));
  if (m < 60) return m + 'm';
  if (m < 1440) return Math.floor(m / 60) + 'h' + (m % 60) + 'm';
  return Math.floor(m / 1440) + 'd' + Math.floor((m % 1440) / 60) + 'h';
}

// Footer: usage left in the 5h and 7d windows, e.g. "5h 72% (2h10m) · 7d 83% (3d5h)".
async function refreshLimits() {
  let lim = {};
  try { lim = await api('GET', '/api/limits'); } catch (_) { /* show unknown */ }
  const part = (label, w) => {
    if (!w || typeof w.used_percent !== 'number') return h('span', {}, label + ' ?');
    const left = Math.max(0, 100 - w.used_percent);
    const cls = left <= 5 ? 'danger' : left <= 20 ? 'warn' : null;
    return h('span', { class: cls, title: 'Resets ' + fmtTime(w.resets_at) },
      `${label} ${Math.round(left)}% (${untilShort(w.resets_at)})`);
  };
  document.getElementById('limits').replaceChildren(part('5h', lim && lim.five_hour), ' · ', part('7d', lim && lim.seven_day));
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
  document.getElementById('reminder-btn').addEventListener('click', reminderDialog);
  document.getElementById('new-session-btn').addEventListener('click', sessionDialog);
  // "r" (reminder) and "s" (session) anywhere outside a text field.
  const shortcuts = { r: reminderDialog, s: sessionDialog };
  document.addEventListener('keydown', e => {
    if (!shortcuts[e.key] || e.metaKey || e.ctrlKey || e.altKey || document.getElementById('dialog').open) return;
    if (e.target.closest('input, textarea, select, [contenteditable]')) return;
    e.preventDefault();
    shortcuts[e.key]();
  });
  document.getElementById('notif-btn').addEventListener('click', toggleNotifications);
  window.addEventListener('hashchange', route);
  route();
  refreshGlobal();
  setInterval(refreshGlobal, 3000);
  refreshLimits();
  setInterval(refreshLimits, 15000);
})();

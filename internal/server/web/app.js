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

// A greyed key hint next to a control, e.g. hint('g b') or hint('⌘↵'); keys are space-separated.
function hint(keys) {
  return h('span', { class: 'kbd-hint' }, keys.split(' ').map(k => h('kbd', {}, k)));
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

// Working directory picked from the list in Settings; '' falls back to defaultLabel's directory.
// A value no longer in the list stays selectable so it isn't lost silently.
function workdirSelect(value, defaultLabel, attrs) {
  const dirs = state.workdirs.includes(value) || !value ? state.workdirs : [...state.workdirs, value];
  return select([['', defaultLabel], ...dirs.map(d => [d, d])], value || '', attrs);
}

function remember(key, value) {
  try { localStorage.setItem(key, value); } catch (_) { /* storage unavailable */ }
}

function recall(key) {
  try { return localStorage.getItem(key) || ''; } catch (_) { return ''; }
}

// Minimal rendering: fenced code blocks become <pre>, the rest stays plain text.
function trunc(s, n) {
  s = (s || '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// ---------- state & routing ----------

// projectId: the project whose tasks the board, list and archive show; remembered across reloads.
const state = { meta: null, board: null, workdirs: [], skills: [], timers: [], projectId: Number(recall('project')) || 0 };
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
    memory: () => parts[1] ? renderMemory(Number(parts[1])) : renderMemories(),
    settings: renderSettings,
  }[parts[0]] || renderBoard;
  r().catch(err => view.replaceChildren(h('div', { class: 'panel' }, 'Error: ' + err.message)));
}

async function loadBoard() {
  state.board = await api('GET', '/api/board' + (state.projectId ? `?project_id=${state.projectId}` : ''));
  state.projectId = state.board.project_id; // the server falls back to the first project
  drawProjectSelect();
  return state.board;
}

function drawProjectSelect() {
  const sel = document.getElementById('project-select');
  const ps = state.board.projects;
  const same = sel.options.length === ps.length && ps.every((p, i) => sel.options[i].value === String(p.id) && sel.options[i].text === p.name);
  if (!same) sel.replaceChildren(...ps.map(p => h('option', { value: p.id }, p.name)));
  sel.value = state.projectId;
}

function switchProject(id) {
  state.projectId = id;
  remember('project', id);
  // Pages showing one task stay on it; the others redraw for the project.
  if (!/^#\/(task|session|sessions|memory|settings)\b/.test(location.hash)) route();
  else loadBoard();
}

function projectName(id) {
  const p = state.board && state.board.projects.find(p => p.id === id);
  return p ? p.name : '?';
}

// Where a task's agents run when no directory is picked: the task's own, else its project's, else the app's.
function taskWorkdir(t) {
  const p = state.board && state.board.projects.find(p => p.id === t.project_id);
  return t.workdir || (p && p.workdir) || state.meta.workdir;
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

function buildsOnBadge(t) {
  if (!t.context_from.length) return null;
  const ids = t.context_from.map(i => '#' + i).join(' ');
  return h('span', { class: 'badge', title: `Builds on ${ids}: their agents' results go into this task's agent context` }, '↳ ' + ids);
}

// Sessions whose agent waits for an answer, a permission or a plan approval.
function needsInputBadge(n, reason) {
  return n ? h('span', { class: 'badge needs-input', title: reason || 'An agent waits for your input' }, n > 1 ? `✋ ${n} need you` : '✋ needs you') : null;
}

function runningBadge(n) {
  return n ? h('span', { class: 'badge running' }, h('span', { class: 'pulse' }), n > 1 ? `${n} agents` : 'agent') : null;
}

// Short countdown: 45m, 2h10m, 3d5h.
function untilShort(ts) {
  const m = Math.max(0, Math.floor((new Date(ts) - Date.now()) / 60000));
  if (m < 60) return m + 'm';
  if (m < 1440) return Math.floor(m / 60) + 'h' + (m % 60) + 'm';
  return Math.floor(m / 1440) + 'd' + Math.floor((m % 1440) / 60) + 'h';
}

// Usage left in the 5h and 7d windows, as last reported by a session's turns:
// "5h 72% (2h10m) · 7d 83% (3d5h)". A window that is unknown or has reset since is "?".
function limitsEl(lim) {
  const part = (label, w) => {
    if (!w || new Date(w.resets_at) <= Date.now()) return h('span', {}, label + ' ?');
    const left = Math.max(0, 100 - w.used_percent);
    const cls = left <= 5 ? 'danger' : left <= 20 ? 'warn' : null;
    return h('span', { class: cls, title: 'Resets ' + fmtTime(w.resets_at) },
      `${label} ${Math.round(left)}% (${untilShort(w.resets_at)})`);
  };
  return h('span', { class: 'limits small muted', title: 'Claude usage left in the 5-hour and weekly windows' },
    part('5h', lim && lim.five_hour), ' · ', part('7d', lim && lim.seven_day));
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
          await api('POST', '/api/tasks', { title, lane_id: lane.id, project_id: state.projectId });
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
    h('div', { class: 'card-meta' }, kindBadge(t.kind), dueBadge(t), buildsOnBadge(t), needsInputBadge(state.board.needs_input[t.id]), runningBadge(state.board.running[t.id]), fieldLinks(t)),
    state.board.running_limits[t.id] ? h('div', { class: 'card-meta' }, limitsEl(state.board.running_limits[t.id])) : null);
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
      h('td', {}, needsInputBadge(b.needs_input[t.id]), runningBadge(b.running[t.id]), fieldLinks(t)),
      h('td', { class: 'muted small' }, ago(t.updated_at)))));
  }
  draw();
  view.replaceChildren(h('div', { class: 'panel' },
    h('div', { class: 'toolbar' }, filter, hint('/'), laneSel),
    h('table', {}, h('thead', {}, h('tr', {}, ['Task', 'Lane', 'Kind', 'Due', 'Links', 'Updated'].map(x => h('th', {}, x)))), tbody)));
  filter.focus();
}

// ---------- archive ----------

async function renderArchive() {
  const b = await loadBoard();
  const tasks = await api('GET', `/api/archive?project_id=${b.project_id}`);
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
    h('div', { class: 'toolbar' }, h('h2', { class: 'grow', style: 'flex:1' }, 'Archive'), filter, hint('/')),
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
  const [data, allMemories] = await Promise.all([api('GET', `/api/tasks/${id}`), api('GET', '/api/memories')]);
  const t = data.task;
  const save = patch => api('PATCH', `/api/tasks/${id}`, patch).then(nt => Object.assign(t, nt));

  // --- side panel ---
  const side = h('div', { class: 'task-side' });
  const sessionPanel = newSessionPanel(t, s => addSession(s));
  const buildsOn = buildsOnEditor(t, data.builds_on, data.used_by, async ids => {
    const refs = await save({ context_from: ids }).then(() => api('GET', `/api/tasks/${id}`)).then(d => d.builds_on);
    sessionPanel.drawContextLabel();
    return refs;
  });
  const memories = memoryEditor(data.memories, allMemories, async ids => {
    const refs = await save({ memories: ids }).then(() => api('GET', `/api/tasks/${id}`)).then(d => d.memories);
    sessionPanel.drawContextLabel();
    return refs;
  });
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
    h('label', {}, 'Project'),
    select(state.board.projects.map(p => [p.id, p.name]), t.project_id, {
      onchange: async e => { await save({ project_id: Number(e.target.value) }); toast('Moved to ' + projectName(t.project_id)); },
    }),
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
    workdirSelect(t.workdir, `Project default (${taskWorkdir({ ...t, workdir: '' })})`, { onchange: e => save({ workdir: e.target.value }) }),
    fields,
    memories.el,
    buildsOn.el,
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

  const artifacts = artifactsPanel(id, data.artifacts);
  const main = h('div', {}, sessionPanel.el, artifacts.el, sessionsEl,
    data.sessions.length ? null : h('p', { class: 'muted' }, 'No agent sessions yet.'));
  every(2000, async () => {
    components.forEach(c => c.drawLimits());
    const list = await api('GET', `/api/sessions?task_id=${id}`);
    let finished = false; // a turn ended: a skill may have left an artifact
    for (const s of list) {
      const c = components.get(s.id);
      if (!c) {
        const full = await api('GET', `/api/sessions/${s.id}`);
        addSession(Object.assign(full.session, { messages: full.messages }));
      } else if (s.status === 'running' || s.status !== c.session.status || s.updated_at !== c.session.updated_at || summaryChanged(s, c.session)) {
        finished = finished || (c.session.status === 'running' && s.status !== 'running');
        const upd = await api('GET', `/api/sessions/${s.id}?after=${c.lastId()}`);
        c.update(upd.session, upd.messages);
      }
    }
    if (finished) artifacts.reload();
  });

  return {
    el: h('div', { class: 'task-page' }, side, main),
    focus: sessionId => { if (sessionId && components.has(sessionId)) components.get(sessionId).el.scrollIntoView(); },
  };
}

// The tasks this one builds on, editable, and the tasks that build on it.
// setIds saves a new list and resolves to the updated refs.
function buildsOnEditor(task, refs, usedBy, setIds) {
  const el = h('div');
  const taskLink = r => h('a', { href: `#/task/${r.id}`, onclick: closeTaskDialog }, `#${r.id} ${r.title}`, r.archived ? ' (archived)' : '');
  const add = taskSearch({
    placeholder: 'Add an earlier task — search any project by words or #id…',
    exclude: x => x.id === task.id || refs.some(r => r.id === x.id),
    note: x => [x.project_id !== task.project_id ? projectName(x.project_id) : null, x.archived_at ? 'archived' : null],
    onPick: async x => { refs = await setIds([...refs.map(r => r.id), x.id]); draw(); add.input.focus(); },
  });
  const draw = () => {
    el.replaceChildren(...[
      h('label', {}, 'Builds on — their agents\' results go into this task\'s agent context'),
      refs.length ? null : h('div', { class: 'muted small' }, 'Nothing: agents start from scratch.'),
      refs.map(r => h('div', { class: 'field-row' }, h('span', { class: 'link-ref' }, taskLink(r)),
        h('button', {
          class: 'ghost small', title: 'Stop building on this task',
          onclick: async () => { refs = await setIds(refs.filter(x => x.id !== r.id).map(x => x.id)); draw(); },
        }, '✕'))),
      add.el,
      h('label', {}, 'Used by'),
      usedBy.map(r => h('div', { class: 'link-ref' }, taskLink(r))),
      h('button', {
        class: 'ghost small', title: 'A new task whose agents get this task\'s results',
        onclick: () => newTaskDialog({ buildsOn: task }),
      }, '+ Follow-up task'),
    ].flat().filter(c => c != null));
  };
  draw();
  return { el };
}

// The memories the task is linked to, editable. setIds saves a new list and
// resolves to the updated refs.
function memoryEditor(refs, all, setIds) {
  const el = h('div');
  const draw = () => {
    const free = all.filter(m => !refs.some(r => r.id === m.id));
    el.replaceChildren(...[
      h('label', {}, 'Memory — agents read it and update it after each turn'),
      refs.length ? null : h('div', { class: 'muted small' }, 'None: what agents find out stays in their sessions.'),
      refs.map(r => h('div', { class: 'field-row' },
        h('span', { class: 'link-ref' }, h('a', { href: `#/memory/${r.id}`, onclick: closeTaskDialog }, r.name)),
        h('button', {
          class: 'ghost small', title: 'Unlink this memory',
          onclick: async () => { refs = await setIds(refs.filter(x => x.id !== r.id).map(x => x.id)); draw(); },
        }, '✕'))),
      free.length ? select([['', 'Link a memory…'], ...free.map(m => [m.id, m.name])], '', {
        onchange: async e => {
          const id = Number(e.target.value);
          if (id) { refs = await setIds([...refs.map(r => r.id), id]); draw(); }
        },
      }) : h('a', { class: 'small', href: '#/memory', onclick: closeTaskDialog }, all.length ? 'All memories are linked' : 'Create a memory…'),
    ].flat().filter(c => c != null));
  };
  draw();
  return { el };
}

// A search box over every task (archived ones included) with a list of matches:
// ↑/↓ choose, Enter (or a click) picks one, Esc closes the list. Focusing the
// empty box lists the most recently updated tasks.
function taskSearch({ placeholder, exclude, note, onPick }) {
  const input = h('input', { type: 'search', placeholder });
  const list = h('ul', { class: 'picker task-search', hidden: true });
  let found = [], i = 0, timer = 0, seq = 0;
  const draw = () => {
    list.replaceChildren(...found.map((x, j) => h('li', {
      class: j === i ? 'selected' : '',
      onmousedown: e => { e.preventDefault(); pick(x); },
      onmousemove: () => { if (i !== j) { i = j; draw(); } },
    }, `#${x.id} ${x.title}`, ...note(x).filter(Boolean).map(n => h('span', { class: 'small task-search-note' }, ' · ' + n)))));
    if (!found.length) list.append(h('li', { class: 'muted small' }, 'No matching tasks'));
    list.hidden = false;
    list.children[i]?.scrollIntoView({ block: 'nearest' });
  };
  const close = () => { clearTimeout(timer); seq++; list.hidden = true; };
  const search = async () => {
    const n = ++seq;
    const ts = await api('GET', '/api/tasks?limit=50&q=' + encodeURIComponent(input.value.trim()));
    if (n !== seq) return;
    found = ts.filter(x => !exclude(x)).slice(0, 20);
    i = 0;
    draw();
  };
  const pick = x => { input.value = ''; close(); onPick(x); };
  input.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(search, 150); });
  input.addEventListener('focus', search);
  input.addEventListener('blur', close);
  input.addEventListener('keydown', e => {
    const by = { ArrowDown: 1, ArrowUp: -1 }[e.key];
    if (by && !list.hidden && found.length) { e.preventDefault(); i = (i + by + found.length) % found.length; draw(); }
    else if (e.key === 'Enter' && !list.hidden && found[i]) { e.preventDefault(); pick(found[i]); }
    else if (e.key === 'Escape' && !list.hidden) { e.preventDefault(); e.stopPropagation(); close(); }
  });
  return { el: h('div', { class: 'task-search-box' }, input, list), input };
}

function newSessionPanel(task, onStarted) {
  const prompt = h('textarea', { rows: 4, placeholder: 'What should the agent do? Skills work too, e.g. /p-dev:review 123' });
  const mode = select(modeOptions(false), state.meta.default_mode);
  const [model, modelList] = modelInput('');
  const ctx = h('input', { type: 'checkbox', checked: true });
  const ctxLabel = h('span');
  const drawContextLabel = () => {
    const n = task.context_from.length, m = task.memories.length;
    ctxLabel.textContent = 'Include task context' + (m ? `, ${m} memor${m > 1 ? 'ies' : 'y'}` : '') +
      (n ? ` and session summaries of ${n} earlier task${n > 1 ? 's' : ''}` : '');
  };
  drawContextLabel();
  const workdir = workdirSelect('', `Default (${taskWorkdir(task)})`);
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
  const el = h('div', { class: 'panel' },
    h('h3', {}, 'New agent session'),
    prompt,
    h('div', { class: 'row', style: 'margin-top:8px; flex-wrap:wrap' },
      h('div', { class: 'grow', style: 'min-width:200px' }, mode),
      h('div', { class: 'grow', style: 'min-width:140px' }, model, modelList),
      h('div', { class: 'grow', style: 'min-width:200px' }, workdir),
      h('div', { class: 'grow', style: 'min-width:200px' }, doneLane),
      h('label', { class: 'row', style: 'margin:0' }, ctx, ctxLabel),
      h('button', { onclick: start }, 'Start', hint('⌘↵'))));
  return { el, drawContextLabel };
}

function messageEl(m) {
  switch (m.kind) {
    case 'tool_use': {
      const sp = m.content.indexOf(' ');
      const name = sp > 0 ? m.content.slice(0, sp) : m.content;
      const input = sp > 0 ? m.content.slice(sp + 1) : '';
      let pretty = input;
      try { pretty = JSON.stringify(JSON.parse(input), null, 2); } catch (_) { /* keep raw */ }
      return h('details', { class: 'tool tool-use' }, h('summary', {}, `🔧 ${name} ${trunc(input, 120)}`), h('pre', {}, pretty));
    }
    case 'context':
      return h('details', { class: 'tool' }, h('summary', {}, `📄 ${trunc(m.content, 120)}`), h('pre', {}, m.content));
    case 'tool_result':
      return h('details', { class: 'tool' }, h('summary', {}, `↳ ${trunc(m.content, 120) || '(empty)'}`), h('pre', {}, m.content));
    case 'result':
      return h('div', { class: 'msg result' }, `✓ Turn finished · ${fmtTime(m.created_at)}`);
    case 'error':
      return h('div', { class: 'msg error' }, '✗ ' + m.content);
    default:
      return h('div', { class: 'msg md ' + m.role }, markdown(m.content));
  }
}

const isToolMsg = m => m.kind === 'tool_use' || m.kind === 'tool_result' || m.kind === 'context';

// Tool calls, their output and CLI-injected context between two messages fold into one collapsed group,
// so a session reads as the conversation; the group keeps growing while the agent works.
function appendMessage(body, m) {
  if (!isToolMsg(m)) { body.append(messageEl(m)); return; }
  let group = body.lastElementChild;
  if (!group || !group.classList.contains('tools')) {
    group = h('details', { class: 'tools' }, h('summary', {}), h('div', { class: 'tools-body' }));
    body.append(group);
  }
  group.lastElementChild.append(messageEl(m));
  const n = group.querySelectorAll('.tools-body > .tool-use').length;
  group.firstElementChild.textContent = n === 1 ? '1 tool call' : `${n} tool calls`;
}

function sessionComponent(session, messages) {
  let last = 0;
  const body = h('div', { class: 'session-body' });
  const head = h('div', { class: 'session-head' });
  const limits = h('div', { class: 'session-limits' });
  const input = h('textarea', { rows: 2, placeholder: 'Reply to continue this session… (⌘↵ to send)' });
  const send = h('button', {}, 'Send', hint('⌘↵'));
  // Task sessions can change where the task goes when the agent finishes, even mid-run.
  const doneLane = session.task_id && state.board ? doneLaneSelect(state.board.lanes, session.done_lane_id, {
    class: 'small', style: 'width:auto',
    onchange: async e => { c.session = await api('PATCH', `/api/sessions/${c.session.id}`, { done_lane_id: doneLaneValue(e.target) }); },
  }) : null;
  // Applies a skill to the session's work as its next turn.
  const runSkill = state.skills.length ? select([['', 'Run skill…'], ...state.skills.map(sk => [sk.id, sk.name])], '', {
    class: 'small', style: 'width:auto', title: 'Apply a skill to the work of this session',
    onchange: async e => {
      const skillId = Number(e.target.value);
      e.target.value = '';
      if (!skillId) return;
      await api('POST', `/api/sessions/${c.session.id}/skill`, { skill_id: skillId });
      c.session.status = 'running';
      drawHead();
    },
  }) : null;
  const foot = h('div', { class: 'session-foot' }, input,
    h('div', { class: 'row', style: 'margin-top:6px; justify-content:flex-end' }, runSkill, doneLane, send));
  // What tasks building on this one get instead of the transcript; collapsed
  // until opened, and the open state survives redraws.
  const summary = h('details', { class: 'session-summary' });
  const el = h('div', { class: 'session' }, head, summary, body, limits, foot);
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
      needsInputBadge(s.needs_input ? 1 : 0, 'The agent ' + s.needs_input),
      s.needs_input ? h('button', {
        class: 'ghost small', title: 'No answer needed: clear the tag',
        onclick: async () => { await api('DELETE', `/api/sessions/${s.id}/needs-input`); c.session.needs_input = ''; drawHead(); },
      }, 'Dismiss') : null,
      h('span', { class: 'badge' }, s.mode),
      s.model ? h('span', { class: 'badge' }, s.model) : null,
      s.trigger !== 'manual' ? h('span', { class: 'badge' }, s.trigger) : null,
      s.parent_id ? h('a', { class: 'small', href: sessionHref({ task_id: s.task_id, id: s.parent_id }) }, `← from #${s.parent_id}`) : null,
      s.child_id ? h('a', { class: 'small', href: sessionHref({ task_id: s.task_id, id: s.child_id }) }, `continued in #${s.child_id} →`) : null,
      s.cost_usd ? h('span', { class: 'muted small' }, '$' + s.cost_usd.toFixed(2)) : null,
      h('span', { class: 'muted small' }, fmtTime(s.created_at)),
      resumeCmd ? h('button', {
        class: 'ghost small', title: resumeCmd,
        onclick: () => navigator.clipboard.writeText(resumeCmd).then(() => toast('Resume command copied')),
      }, 'Copy resume cmd') : null,
      running ? h('button', { class: 'danger small', onclick: () => api('POST', `/api/sessions/${s.id}/cancel`) }, 'Stop') : null,
      resumable ? h('button', { class: 'small', title: 'Continue this session where it stopped', onclick: doResume }, 'Resume') : null,
      s.memorizing ? h('span', { class: 'badge' }, h('span', { class: 'pulse' }), 'updating memory') : null,
      !running && !s.memorizing && s.task_id && s.claude_session_id ? h('button', {
        class: 'ghost small', title: 'Update the task\'s memories with what this session found out (it also happens after every turn)',
        onclick: async () => {
          await api('POST', `/api/sessions/${s.id}/memory`);
          c.session.memorizing = true;
          drawHead();
          toast('Updating the memories…');
        },
      }, 'Update memory') : null,
      !running && s.claude_session_id ? h('button', {
        class: 'ghost small', title: 'Have the agent write a brief and continue in a new session with a fresh context',
        onclick: () => handoffDialog(s, () => { c.session.status = 'running'; drawHead(); }),
      }, 'Hand off') : null,
      h('button', { class: 'ghost small', onclick: () => el.classList.toggle('collapsed') }, '▾'),
    ].filter(Boolean));
    send.disabled = running || !s.claude_session_id;
    if (runSkill) runSkill.disabled = send.disabled;
    drawLimits();
    drawSummary();
  }

  function drawSummary() {
    const s = c.session;
    const status = s.summarizing ? 'writing…' : !s.summary ? 'none yet' : s.summary_stale ? 'out of date' : 'up to date';
    const doSummarize = async e => {
      e.preventDefault(); // a button in <summary> would toggle the details
      await api('POST', `/api/sessions/${s.id}/summary`);
      c.session.summarizing = true;
      drawSummary();
      toast('Writing the summary…');
    };
    summary.replaceChildren(
      h('summary', {},
        h('span', {}, 'Summary'),
        h('span', { class: 'badge' + (s.summary_stale ? ' stale' : '') }, s.summarizing ? h('span', { class: 'pulse' }) : null, status),
        s.summary_at && !s.summarizing ? h('span', { class: 'muted small' }, fmtTime(s.summary_at)) : null,
        h('button', {
          class: 'ghost small', disabled: s.summarizing || s.status === 'running',
          title: 'Have a copy of the agent sum up this session for tasks that build on this one',
          onclick: doSummarize,
        }, s.summary ? 'Regenerate summary' : 'Summarize')),
      s.summary ? h('div', { class: 'msg md' }, markdown(s.summary))
        : h('p', { class: 'muted small' }, 'Tasks that build on this one get the summary of this session instead of its final reply. ' +
          'It is written when a turn finishes and another task builds on this one, or on request.'));
  }

  // Shown while the session runs and kept after the turn as the last known value.
  function drawLimits() {
    const s = c.session;
    const known = s.limits && (s.limits.five_hour || s.limits.seven_day);
    limits.replaceChildren(...(s.status === 'running' || known ? [limitsEl(s.limits)] : []));
  }
  c.drawLimits = drawLimits; // the countdowns tick even when nothing else changes

  c.update = (s, msgs) => {
    c.session = s;
    if (doneLane && document.activeElement !== doneLane) doneLane.value = s.done_lane_id ?? '';
    for (const m of msgs) {
      appendMessage(body, m);
      last = Math.max(last, m.id);
    }
    drawHead();
  };
  c.update(session, messages);
  return c;
}

// A summary or memory update being written or written does not count as activity in the session.
function summaryChanged(a, b) {
  return a.summarizing !== b.summarizing || a.summary_at !== b.summary_at || a.summary_stale !== b.summary_stale ||
    a.memorizing !== b.memorizing;
}

// Hand a session off to a fresh one: its agent writes a brief, and a new
// session starts from it. The new one shows up once the brief is written.
function handoffDialog(session, onStarted) {
  const dlg = document.getElementById('dialog');
  const note = h('textarea', { rows: 4, placeholder: 'Optional: what the new session should do next' });
  const start = async () => {
    await api('POST', `/api/sessions/${session.id}/handoff`, { note: note.value });
    dlg.close();
    toast('Writing the handoff brief…');
    onStarted();
  };
  note.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) start(); });
  dlg.replaceChildren(h('h2', {}, 'Continue in a new session'),
    h('p', { class: 'muted small' }, 'The agent writes a brief of its work in this session; a new session with the same settings starts from that brief and a fresh context.'),
    h('label', {}, 'Next'), note,
    h('div', { class: 'dialog-actions' }, cancelButton(dlg), h('button', { onclick: start }, 'Hand off', hint('⌘↵'))));
  dlg.showModal();
  note.focus();
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
    tbody.replaceChildren(...xs.map(s => h('tr', { class: 'clickable', onclick: () => openFromSessions(s) },
      h('td', {}, h('span', { class: 'badge status-' + s.status }, s.status === 'running' ? h('span', { class: 'pulse' }) : null, s.status),
        needsInputBadge(s.needs_input ? 1 : 0, 'The agent ' + s.needs_input)),
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

// A task's session opens with the task's project in focus.
function openFromSessions(s) {
  location.hash = sessionHref(s);
  if (s.task_project_id && s.task_project_id !== state.projectId) switchProject(s.task_project_id);
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
    c.drawLimits();
    const upd = await api('GET', `/api/sessions/${id}?after=${c.lastId()}`);
    if (upd.messages.length || upd.session.status !== c.session.status || upd.session.updated_at !== c.session.updated_at ||
      summaryChanged(upd.session, c.session)) {
      c.update(upd.session, upd.messages);
    }
  });
}

function sessionDialog() {
  const dlg = document.getElementById('dialog');
  const prompt = h('textarea', { rows: 6, placeholder: 'What should the agent do? Skills work too, e.g. /p-dev:review 123' });
  const lastDir = recall('session.workdir');
  const workdir = workdirSelect(state.workdirs.includes(lastDir) ? lastDir : '', `App default (${state.meta.workdir})`);
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
    h('label', {}, 'Directory'), workdir,
    h('div', { class: 'row' },
      h('div', { class: 'grow' }, h('label', {}, 'Model'), model, modelList),
      h('div', { class: 'grow' }, h('label', {}, 'Mode'), mode)),
    h('div', { class: 'dialog-actions' }, cancelButton(dlg), h('button', { onclick: start }, 'Start', hint('⌘↵'))));
  dlg.showModal();
  prompt.focus();
}

// ---------- memory ----------

const sourceLabel = { user: 'edited by hand', agent: 'updated by an agent', note: 'note added' };

async function renderMemories() {
  if (!state.board) await loadBoard();
  const xs = await api('GET', '/api/memories');
  view.replaceChildren(h('div', { class: 'panel', style: 'max-width:900px' },
    h('div', { class: 'toolbar' }, h('h2', { class: 'grow', style: 'flex:1' }, 'Memory'),
      h('button', { onclick: memoryDialog }, '+ New memory')),
    h('p', { class: 'muted small' }, 'Lasting knowledge kept across tasks. Link a memory to tasks: their agents read it with the task context, ' +
      'and after each turn a copy of the agent rewrites it with what the session found out. Every change is kept as a revision.'),
    xs.length ? h('table', {}, h('thead', {}, h('tr', {}, ['Memory', 'Tasks', 'New tasks of', 'Updated'].map(x => h('th', {}, x)))),
      h('tbody', {}, xs.map(m => h('tr', { class: 'clickable', onclick: () => { location.hash = `#/memory/${m.id}`; } },
        h('td', {}, m.name, m.description ? h('div', { class: 'muted small' }, m.description) : null),
        h('td', { class: 'muted small' }, m.tasks),
        h('td', { class: 'muted small' }, m.project_id ? projectName(m.project_id) : '—'),
        h('td', { class: 'muted small' }, ago(m.updated_at))))))
      : h('p', { class: 'muted' }, 'No memories yet.')));
}

function memoryDialog() {
  const dlg = document.getElementById('dialog');
  const name = h('input', { placeholder: 'e.g. Productive API' });
  const description = h('input', { placeholder: 'What belongs in it — agents go by this' });
  const project = select([['', 'No project'], ...state.board.projects.map(p => [p.id, p.name])], '');
  const create = async () => {
    if (!name.value.trim()) return;
    const m = await api('POST', '/api/memories', { name: name.value, description: description.value, project_id: Number(project.value) || null });
    dlg.close();
    location.hash = `#/memory/${m.id}`;
  };
  submitOnCmdEnter([name, description, project], create);
  dlg.replaceChildren(h('h2', {}, 'New memory'),
    h('label', {}, 'Name'), name, h('label', {}, 'About'), description,
    h('label', {}, 'Link it to the new tasks of'), project,
    h('div', { class: 'dialog-actions' }, cancelButton(dlg), h('button', { onclick: create }, 'Create', hint('⌘↵'))));
  dlg.showModal();
  name.focus();
}

async function renderMemory(id) {
  if (!state.board) await loadBoard();
  let data = await api('GET', `/api/memories/${id}`);
  let m = data.memory;
  let base = m.revision, loaded = m.body; // the revision and text the editor started from
  const save = patch => api('PATCH', `/api/memories/${id}`, patch).then(nm => { m = nm; return nm; });

  const name = h('input', { class: 'title-input', value: m.name, onchange: e => save({ name: e.target.value }) });
  const description = h('input', { value: m.description, placeholder: 'What belongs in it — agents go by this', onchange: e => save({ description: e.target.value }) });
  const project = select([['', 'No project'], ...state.board.projects.map(p => [p.id, p.name])], m.project_id ?? '', {
    onchange: e => save({ project_id: Number(e.target.value) || 0 }),
  });
  const body = h('textarea', { rows: 26, class: 'mono', placeholder: 'Markdown. Agents fill it in after their turns; you can write in it too.' }, m.body);
  const preview = h('div', { class: 'md memory-preview hidden' });
  const note = h('div', { class: 'muted small grow' });
  const dirty = () => body.value !== loaded;
  const drawNote = () => {
    note.textContent = base !== m.revision ? 'The memory changed while you were editing; reload to see it, or your save is refused.'
      : dirty() ? 'Unsaved changes' : '';
  };
  body.addEventListener('input', drawNote);
  const saveBody = async () => {
    await save({ body: body.value, base_revision: base });
    reloadBody();
    toast('Saved');
    reload();
  };
  const reloadBody = () => { body.value = loaded = m.body; base = m.revision; drawNote(); };
  submitOnCmdEnter([body], saveBody);
  const togglePreview = () => {
    const show = preview.classList.contains('hidden');
    if (show) preview.replaceChildren(...markdown(body.value));
    preview.classList.toggle('hidden', !show);
    body.classList.toggle('hidden', show);
  };

  const revisions = h('div');
  const tasks = h('div');
  const drawSide = () => {
    revisions.replaceChildren(...(data.revisions.length ? data.revisions.map((r, i) => h('div', { class: 'revision' },
      h('a', { href: '#', onclick: e => { e.preventDefault(); revisionDialog(r.id, i === 0, reload); } }, fmtTime(r.created_at)),
      h('div', { class: 'muted small' }, sourceLabel[r.source] || r.source,
        r.task_id ? [' · ', h('a', { href: r.session_id ? `#/task/${r.task_id}/s/${r.session_id}` : `#/task/${r.task_id}` }, `#${r.task_id}`)] : null)))
      : [h('p', { class: 'muted small' }, 'No revisions yet.')]));
    tasks.replaceChildren(...(data.tasks.length ? data.tasks.map(t => h('div', { class: 'link-ref' },
      h('a', { href: `#/task/${t.id}` }, `#${t.id} ${t.title}`), t.archived ? ' (archived)' : ''))
      : [h('p', { class: 'muted small' }, 'No task is linked to it yet; link it from a task.')]));
  };
  // Fetches the memory again; the editor follows unless it has unsaved changes.
  const reload = async () => {
    data = await api('GET', `/api/memories/${id}`);
    m = data.memory;
    if (!dirty() && base !== m.revision) reloadBody();
    drawNote();
    drawSide();
  };
  drawSide();
  every(5000, reload); // agents update it after their turns

  view.replaceChildren(h('div', { class: 'task-page' },
    h('div', { class: 'task-side' },
      h('div', { class: 'panel' },
        h('a', { href: '#/memory', class: 'btn ghost small' }, '← Memory'),
        name, h('label', {}, 'About'), description,
        h('label', {}, 'Link it to the new tasks of'), project,
        h('div', { class: 'row', style: 'margin-top:14px' },
          h('span', { class: 'muted small grow' }, 'Created ' + fmtTime(m.created_at)),
          h('button', {
            class: 'danger', onclick: async () => {
              if (!confirm(`Delete memory ${m.name} with all its revisions?`)) return;
              await api('DELETE', `/api/memories/${id}`);
              location.hash = '#/memory';
            },
          }, 'Delete'))),
      h('div', { class: 'panel' }, h('h3', {}, 'Tasks'), tasks),
      h('div', { class: 'panel' }, h('h3', {}, 'Revisions'), revisions)),
    h('div', { class: 'panel' },
      body, preview,
      h('div', { class: 'row', style: 'margin-top:8px' }, note,
        h('button', { class: 'ghost', onclick: reloadBody, title: 'Drop your changes and show the latest version' }, 'Reload'),
        h('button', { class: 'ghost', onclick: togglePreview }, 'Preview'),
        h('button', { onclick: saveBody }, 'Save', hint('⌘↵'))))));
}

// One revision as a diff to the one before, with a way back to it.
async function revisionDialog(revisionId, latest, done) {
  const data = await api('GET', `/api/memory-revisions/${revisionId}`);
  const r = data.revision;
  const dlg = document.getElementById('dialog');
  dlg.replaceChildren(h('h2', {}, 'Revision of ' + fmtTime(r.created_at)),
    h('p', { class: 'muted small' }, (sourceLabel[r.source] || r.source) + '; the changes to the revision before.'),
    h('pre', { class: 'diff' }, data.diff.map(l => h('div', { class: { '+': 'diff-add', '-': 'diff-del' }[l.op] || '' }, l.op + ' ' + l.text))),
    h('div', { class: 'dialog-actions' }, cancelButton(dlg),
      latest ? null : h('button', {
        title: 'Make this version the memory again, as a new revision',
        onclick: async () => {
          await api('POST', `/api/memory-revisions/${r.id}/restore`);
          dlg.close();
          toast('Restored');
          done();
        },
      }, 'Restore this version')));
  dlg.showModal();
}

// ---------- settings ----------

async function renderSettings() {
  const b = await loadBoard();
  const [rules, settings, skills] = await Promise.all([api('GET', '/api/rules'), api('GET', '/api/settings'), api('GET', '/api/skills')]);
  state.skills = skills;
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

  const projects = h('div', { class: 'panel' }, h('h2', {}, 'Projects'),
    h('p', { class: 'muted small' }, 'Each project is a board of its own on the same lanes; pick one at the top. Rules and auto-archive apply to all of them. ' +
      "Agents on a project's tasks run in its directory unless the task or session picks another."),
    b.projects.map((p, i) => {
      const name = h('input', { value: p.name });
      const color = h('input', { type: 'color', value: p.color || '#888888', style: 'width:42px;padding:2px' });
      const workdir = workdirSelect(p.workdir, `App default (${state.meta.workdir})`, { title: 'Default working directory for agents' });
      const saveProject = async () => {
        await api('PATCH', `/api/projects/${p.id}`, { name: name.value, color: color.value, workdir: workdir.value });
        loadBoard();
      };
      name.addEventListener('change', saveProject);
      color.addEventListener('change', saveProject);
      workdir.addEventListener('change', saveProject);
      const move = async d => {
        const ids = b.projects.map(x => x.id);
        [ids[i], ids[i + d]] = [ids[i + d], ids[i]];
        await api('POST', '/api/projects/reorder', { ids });
        rerender();
      };
      return h('div', { class: 'row', style: 'margin-bottom:6px' }, color, h('div', { class: 'grow' }, name), h('div', { class: 'grow' }, workdir),
        h('button', { class: 'ghost', disabled: i === 0, onclick: () => move(-1) }, '↑'),
        h('button', { class: 'ghost', disabled: i === b.projects.length - 1, onclick: () => move(1) }, '↓'),
        h('button', {
          class: 'danger', title: 'Only a project without tasks (archived ones included) can be deleted',
          onclick: async () => { await api('DELETE', `/api/projects/${p.id}`); rerender(); },
        }, 'Delete'));
    }),
    h('input', {
      placeholder: '+ New project (Enter)', onkeydown: async e => {
        if (e.key === 'Enter' && e.target.value.trim()) { await api('POST', '/api/projects', { name: e.target.value }); rerender(); }
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

  view.replaceChildren(h('div', { style: 'max-width:900px' }, projects, lanes, workdirsPanel(settings.workdirs, rerender), rulesPanel,
    skillsPanel(skills, b, rerender), archivePanel(b.lanes, settings), fields));
}

// ---------- skills ----------

// One automation in words, e.g. "research tasks · Assistant · in Review".
function automationLabel(a) {
  return [a.kind ? `${a.kind} tasks` : 'any task', a.project_id ? projectName(a.project_id) : null,
    a.lane_id ? 'in ' + laneName(a.lane_id) : null].filter(Boolean).join(' · ');
}

function skillsPanel(skills, b, rerender) {
  const runsWhen = sk => {
    const on = sk.automations.filter(a => a.enabled);
    return on.length ? 'After a turn on ' + on.map(automationLabel).join('; ') : 'By hand only';
  };
  return h('div', { class: 'panel' },
    h('div', { class: 'row' }, h('h2', { class: 'grow' }, 'Skills'), h('button', { onclick: () => skillDialog(null, b, rerender) }, '+ Add skill')),
    h('p', { class: 'muted small' }, 'Instructions an agent applies to the work of its session: automatically after a successful turn on a ' +
      'matching task, or by hand from a session ("Run skill"). A skill can keep the agent\'s reply as an artifact on the task.'),
    skills.length ? h('table', {}, h('thead', {}, h('tr', {}, ['Skill', 'Runs', 'Model', 'Artifact', ''].map(x => h('th', {}, x)))),
      h('tbody', {}, skills.map(sk => h('tr', {},
        h('td', {}, sk.name, sk.description ? h('div', { class: 'muted small' }, sk.description) : null),
        h('td', { class: 'small' }, runsWhen(sk)),
        h('td', { class: 'muted small' }, sk.model || 'session\'s'),
        h('td', { class: 'muted small' }, sk.artifact ? 'kept' : '—'),
        h('td', {}, h('button', { class: 'ghost small', onclick: () => skillDialog(sk, b, rerender) }, 'Edit'), ' ',
          h('button', {
            class: 'danger small', onclick: async () => {
              if (confirm(`Delete skill ${sk.name}? Artifacts it made stay.`)) { await api('DELETE', `/api/skills/${sk.id}`); rerender(); }
            },
          }, 'Delete'))))))
      : h('p', { class: 'muted' }, 'No skills yet.'));
}

function skillDialog(skill, b, done) {
  skill = skill || { name: '', description: '', body: '', artifact: true, model: '', automations: [] };
  const dlg = document.getElementById('dialog');
  const name = h('input', { value: skill.name, placeholder: 'e.g. Research summary' });
  const description = h('input', { value: skill.description, placeholder: 'What it is for' });
  const body = h('textarea', { rows: 14, class: 'mono' }, skill.body);
  const artifact = h('input', { type: 'checkbox', checked: skill.artifact });
  const [model, modelList] = modelInput(skill.model);
  model.placeholder = 'Model — the session\'s';
  const nullableID = sel => sel.value ? Number(sel.value) : null;
  const rows = h('div');
  const addRow = a => {
    const enabled = h('input', { type: 'checkbox', checked: a.enabled, title: 'Enabled' });
    const kind = select([['', 'Any kind'], ...state.meta.kinds.map(k => [k, k])], a.kind);
    const project = select([['', 'Any project'], ...b.projects.map(p => [p.id, p.name])], a.project_id ?? '');
    const lane = select([['', 'In any lane'], ...b.lanes.map(l => [l.id, 'In ' + l.name])], a.lane_id ?? '');
    const row = h('div', { class: 'row automation', style: 'margin-bottom:6px' }, enabled,
      h('div', { class: 'grow' }, kind), h('div', { class: 'grow' }, project), h('div', { class: 'grow' }, lane),
      h('button', { class: 'ghost small', title: 'Remove', onclick: () => row.remove() }, '✕'));
    row.value = () => ({ enabled: enabled.checked, kind: kind.value, project_id: nullableID(project), lane_id: nullableID(lane) });
    rows.append(row);
  };
  skill.automations.forEach(addRow);
  dlg.replaceChildren(h('h2', {}, skill.id ? 'Edit skill' : 'New skill'),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'Name'), name),
      h('div', { class: 'grow' }, h('label', {}, 'Description'), description)),
    h('label', {}, 'Instructions — what the agent does with its session\'s work, e.g. the shape of a summary. ' +
      'Template vars: {{.Task.Title}}, {{.Task.ID}}, {{.Fields.pull_request}}, {{.Context}}'),
    body,
    h('label', {}, 'Model for the skill\'s turn, e.g. fable — empty keeps the session\'s model'), model, modelList,
    h('label', { class: 'row', style: 'margin-top:8px' }, artifact, 'Keep the agent\'s reply as an artifact on the task'),
    h('label', {}, 'Run automatically after an agent turn finishes successfully, on a task that matches any of:'),
    rows,
    h('button', { class: 'ghost small', onclick: () => addRow({ enabled: true, kind: '', project_id: null, lane_id: null }) }, '+ Add condition'),
    h('div', { class: 'dialog-actions' },
      cancelButton(dlg),
      h('button', {
        onclick: async () => {
          const out = {
            name: name.value, description: description.value, body: body.value, artifact: artifact.checked, model: model.value,
            automations: [...rows.children].map(r => r.value()),
          };
          await api(skill.id ? 'PATCH' : 'POST', skill.id ? `/api/skills/${skill.id}` : '/api/skills', out);
          dlg.close();
          done();
        },
      }, 'Save')));
  dlg.showModal();
  name.focus();
}

// The task's artifacts, newest first; reload() fetches them again.
function artifactsPanel(taskId, artifacts) {
  const el = h('div');
  const draw = xs => el.replaceChildren(...(xs.length ? [h('div', { class: 'panel' }, h('h3', {}, 'Artifacts'),
    xs.map((x, i) => h('details', { class: 'artifact', open: i === 0 },
      h('summary', {}, h('strong', {}, x.skill || 'Artifact'), ' ', h('span', { class: 'muted small' }, fmtTime(x.created_at))),
      h('div', { class: 'md' }, markdown(x.content)),
      h('div', { class: 'row', style: 'justify-content:flex-end; margin-top:6px' },
        h('button', { class: 'ghost small', onclick: () => navigator.clipboard.writeText(x.content).then(() => toast('Copied')) }, 'Copy'),
        h('button', {
          class: 'danger small', onclick: async () => {
            if (confirm('Delete this artifact?')) { await api('DELETE', `/api/artifacts/${x.id}`); reload(); }
          },
        }, 'Delete')))))] : []));
  const reload = async () => draw(await api('GET', `/api/tasks/${taskId}/artifacts`));
  draw(artifacts);
  return { el, reload };
}

// The directories agents can be started in; every directory field is a dropdown of these.
function workdirsPanel(dirs, rerender) {
  const save = async next => {
    const cur = await api('GET', '/api/settings');
    state.workdirs = (await api('PUT', '/api/settings', { ...cur, workdirs: next })).workdirs;
    rerender();
  };
  const move = (i, d) => {
    const next = [...dirs];
    [next[i], next[i + d]] = [next[i + d], next[i]];
    save(next);
  };
  return h('div', { class: 'panel' }, h('h2', {}, 'Working directories'),
    h('p', { class: 'muted small' }, `Where agents can run, offered as a dropdown wherever you start one. Unset means the app default (${state.meta.workdir}).`),
    dirs.map((d, i) => h('div', { class: 'row', style: 'margin-bottom:6px' },
      h('code', { class: 'grow' }, d),
      h('button', { class: 'ghost', disabled: i === 0, onclick: () => move(i, -1) }, '↑'),
      h('button', { class: 'ghost', disabled: i === dirs.length - 1, onclick: () => move(i, 1) }, '↓'),
      h('button', { class: 'danger', onclick: () => save(dirs.filter(x => x !== d)) }, 'Remove'))),
    h('input', {
      placeholder: '+ Add directory, e.g. ~/workspace/assistant (Enter)', onkeydown: e => {
        if (e.key === 'Enter' && e.target.value.trim()) save([...dirs, e.target.value]);
      },
    }));
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
  const workdir = workdirSelect(rule.workdir, "Task directory, else the project's, else app default");
  const doneLane = doneLaneSelect(lanes, rule.done_lane_id);
  dlg.replaceChildren(h('h2', {}, rule.id ? 'Edit rule' : 'New rule'),
    h('label', {}, 'Name'), name,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'From'), from), h('div', { class: 'grow' }, h('label', {}, 'To'), to)),
    h('label', {}, 'Prompt template — use a skill like /p-dev:code, and {{.Context}} (the task plus results of tasks it builds on), {{.Task.Title}}, {{.Task.Description}}, {{.Fields.pull_request}}'),
    prompt,
    h('label', {}, 'Agent mode'), mode,
    h('label', {}, 'Working directory'), workdir,
    h('label', {}, 'After the agent finishes (skipped if the task was moved meanwhile)'), doneLane,
    h('div', { class: 'dialog-actions' },
      cancelButton(dlg),
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
    await api('POST', '/api/tasks', { title: title.value, kind: 'reminder', due_at: fromLocalInput(when.value), project_id: state.projectId });
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
    h('div', { class: 'dialog-actions' }, cancelButton(dlg), h('button', { onclick: create }, 'Set reminder', hint('↵'))));
  dlg.showModal();
  title.focus();
}

// ---------- new task dialog ----------

// opts.buildsOn: the task the new one follows up on; it goes in the same project.
async function newTaskDialog(opts = {}) {
  if (!state.board) await loadBoard();
  const from = opts.buildsOn;
  const dlg = document.getElementById('dialog');
  const title = h('input', { placeholder: 'Title' });
  const desc = h('textarea', { rows: 4, placeholder: 'Description' });
  const kind = select(state.meta.kinds.map(k => [k, k]), 'task');
  const project = select(state.board.projects.map(p => [p.id, p.name]), from ? from.project_id : state.projectId);
  const lane = select(state.board.lanes.map(l => [l.id, l.name]), state.board.lanes[0]?.id);
  const due = h('input', { type: 'datetime-local' });
  const create = async () => {
    if (!title.value.trim()) return;
    if (due.value) askNotificationPermission();
    const t = await api('POST', '/api/tasks', {
      title: title.value, description: desc.value, kind: kind.value, lane_id: Number(lane.value), due_at: fromLocalInput(due.value),
      project_id: Number(project.value), context_from: from ? [from.id] : [],
    });
    dlg.close();
    // Back to the board, on the new task's project, where the task now shows.
    state.projectId = t.project_id;
    remember('project', t.project_id);
    go('#/board');
  };
  title.addEventListener('keydown', e => { if (e.key === 'Enter' && !e.metaKey && !e.ctrlKey) create(); });
  submitOnCmdEnter([title, desc, kind, project, lane, due], create);
  // replaceChildren would draw a null as the text "null", unlike h().
  dlg.replaceChildren(...[h('h2', {}, from ? 'New follow-up task' : 'New task'),
    from ? h('p', { class: 'muted small' }, `Builds on #${from.id} ${from.title}: its agents' results go into this task's agent context.`) : null,
    h('label', {}, 'Title'), title, h('label', {}, 'Description'), desc,
    h('label', {}, 'Project'), project,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'Kind'), kind), h('div', { class: 'grow' }, h('label', {}, 'Lane'), lane)),
    h('label', {}, 'Due (optional) — you get a notification at this time'), due,
    h('div', { class: 'dialog-actions' }, cancelButton(dlg), h('button', { onclick: create }, 'Create', hint('⌘↵')))].filter(Boolean));
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
    openNotification(n);
  };
}

// Opens what the notification is about: its session if it has one, else its task.
function openNotification(n) {
  if (n.session_id) location.hash = sessionHref({ task_id: n.task_id, id: n.session_id });
  else if (n.task_id) location.hash = `#/task/${n.task_id}`;
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
      openNotification(n);
    },
  }, h('div', {}, n.message), h('div', { class: 'muted small' }, (n.task_title ? n.task_title + ' · ' : '') + ago(n.created_at))))
    : [h('div', { class: 'notif muted' }, 'Nothing here.')]));
  panel.classList.remove('hidden');
}

// ---------- keyboard shortcuts ----------

// ⌥ (Option/Alt) + key works everywhere, text fields included: it's matched by the physical key
// (e.code), since on a Mac ⌥+letter would type a special character. ⌘ and Ctrl are left to the
// browser and to the text fields' cursor keys; letters that start an accent (⌥E, ⌥I, ⌥N, ⌥U) are
// avoided. "/" and "?" work alone outside a text field.
const shortcuts = [
  { keys: '⌥T', code: 'KeyT', label: 'New task', run: newTaskDialog },
  { keys: '⌥R', code: 'KeyR', label: 'Quick reminder', run: reminderDialog },
  { keys: '⌥P', code: 'KeyP', label: 'Agent session without a task', run: sessionDialog },
  { keys: '⌥M', code: 'KeyM', label: 'Notifications', run: toggleNotifications },
  { keys: '⌥K', code: 'KeyK', label: 'Pick a project', run: projectPicker },
  { keys: '⌥[', code: 'BracketLeft', label: 'Previous project', run: () => stepProject(-1) },
  { keys: '⌥]', code: 'BracketRight', label: 'Next project', run: () => stepProject(1) },
  { keys: '⌥B', code: 'KeyB', label: 'Go to Board', run: () => go('#/board') },
  { keys: '⌥L', code: 'KeyL', label: 'Go to List', run: () => go('#/list') },
  { keys: '⌥S', code: 'KeyS', label: 'Go to Sessions', run: () => go('#/sessions') },
  { keys: '⌥O', code: 'KeyO', label: 'Go to Memory', run: () => go('#/memory') },
  { keys: '⌥A', code: 'KeyA', label: 'Go to Archive', run: () => go('#/archive') },
  { keys: '⌥,', code: 'Comma', label: 'Go to Settings', run: () => go('#/settings') },
  { keys: '/', label: 'Filter the list or archive', run: focusFilter },
  { keys: '?', label: 'Show these shortcuts', run: shortcutsDialog },
];

// The shortcut a keydown triggers, if any; inField is whether it was typed into a text field.
function shortcutFor(e, inField) {
  if (e.metaKey || e.ctrlKey || e.repeat) return null;
  if (e.altKey) return e.shiftKey ? null : shortcuts.find(s => s.code === e.code) || null;
  if (inField) return null;
  return shortcuts.find(s => !s.code && s.keys === e.key) || null;
}

// Where a project picked with ⌥K opens: the list when on the list, else the board.
function pickedProjectHash(hash) {
  return /^#\/list\b/.test(hash) ? '#/list' : '#/board';
}

// Makes ⌘↵ (Ctrl↵ elsewhere) in any of the fields call submit, e.g. from a dialog's textarea.
function submitOnCmdEnter(fields, submit) {
  for (const f of fields) {
    f.addEventListener('keydown', e => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !e.repeat) { e.preventDefault(); submit(); }
    });
  }
}

function onShortcut(e) {
  if (document.getElementById('dialog').open) return;
  const inField = !!e.target.closest('input, textarea, select, [contenteditable]');
  if (e.key === 'Escape' && !inField) { closeNotifications(); return; }
  const sc = shortcutFor(e, inField);
  if (sc) { e.preventDefault(); sc.run(); }
}

// Jumps to a page; on that page already, it redraws it (closing the task dialog).
function go(hash) {
  if (location.hash === hash) route();
  else location.hash = hash;
}

function focusFilter() {
  const f = view.querySelector('.toolbar input');
  if (f) f.focus();
  else location.hash = '#/list'; // the list focuses its filter on open
}

function closeNotifications() {
  document.getElementById('notif-panel').classList.add('hidden');
}

function stepProject(by) {
  const ps = state.board?.projects;
  if (!ps || ps.length < 2) return;
  const i = ps.findIndex(p => p.id === state.projectId);
  const next = ps[(i + by + ps.length) % ps.length];
  switchProject(next.id);
  toast(next.name);
}

// ⌥K: a list of the projects; ↑/↓ move, Enter (or a click) opens the chosen one.
async function projectPicker() {
  if (!state.board) await loadBoard();
  const ps = state.board.projects;
  if (!ps.length) return;
  const hash = pickedProjectHash(location.hash);
  const dlg = document.getElementById('dialog');
  let i = Math.max(0, ps.findIndex(p => p.id === state.projectId));
  const pick = p => {
    dlg.close();
    state.projectId = p.id;
    remember('project', p.id);
    go(hash);
  };
  const items = ps.map((p, j) => h('li', { onclick: () => pick(p), onmousemove: () => { i = j; draw(); } }, p.name));
  function draw() {
    items.forEach((el, j) => el.classList.toggle('selected', j === i));
    items[i].scrollIntoView({ block: 'nearest' });
  }
  const list = h('ul', {
    class: 'picker', tabindex: 0, onkeydown: e => {
      const by = { ArrowDown: 1, ArrowUp: -1 }[e.key];
      if (by) { e.preventDefault(); i = (i + by + ps.length) % ps.length; draw(); }
      else if (e.key === 'Enter') { e.preventDefault(); pick(ps[i]); }
    },
  }, items);
  dlg.replaceChildren(h('h2', {}, 'Open project'), list,
    h('p', { class: 'muted small' }, `↑/↓ to choose, Enter opens its ${hash === '#/list' ? 'list' : 'board'}, Esc closes.`));
  dlg.showModal();
  list.focus();
  draw();
}

function shortcutsDialog() {
  const dlg = document.getElementById('dialog');
  dlg.replaceChildren(h('h2', {}, 'Keyboard shortcuts'),
    h('table', { class: 'shortcuts' }, shortcuts.map(s => h('tr', {},
      h('td', {}, s.keys.split(' ').map(k => h('kbd', {}, k))), h('td', {}, s.label)))),
    h('p', { class: 'muted small' }, '⌘/Ctrl+Enter sends a prompt or reply. Esc closes a dialog or the notifications. ',
      'Greyed keys next to buttons and links show their shortcut.'),
    h('div', { class: 'dialog-actions' }, h('button', { onclick: () => dlg.close() }, 'Close', hint('Esc'))));
  dlg.showModal();
}

function cancelButton(dlg) {
  return h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel', hint('Esc'));
}

// Header controls carry data-shortcut; their hint is drawn from it so markup and table stay in step.
function drawHeaderHints() {
  document.querySelectorAll('[data-shortcut]').forEach(el => {
    const keys = hint(el.dataset.shortcut);
    if (el.classList.contains('kbd-hint')) el.replaceChildren(...keys.childNodes);
    else el.append(keys);
  });
}

// ---------- boot ----------

(async function boot() {
  [state.meta, { workdirs: state.workdirs }, state.skills] = await Promise.all([api('GET', '/api/meta'), api('GET', '/api/settings'),
    api('GET', '/api/skills')]);
  document.getElementById('new-task-btn').addEventListener('click', newTaskDialog);
  document.getElementById('reminder-btn').addEventListener('click', reminderDialog);
  document.getElementById('new-session-btn').addEventListener('click', sessionDialog);
  document.getElementById('shortcuts-btn').addEventListener('click', shortcutsDialog);
  document.addEventListener('keydown', onShortcut);
  drawHeaderHints();
  document.getElementById('notif-btn').addEventListener('click', toggleNotifications);
  document.getElementById('project-select').addEventListener('change', e => switchProject(Number(e.target.value)));
  window.addEventListener('hashchange', route);
  route();
  refreshGlobal();
  setInterval(refreshGlobal, 3000);
})();

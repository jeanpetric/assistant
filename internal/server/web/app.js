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
    toast(msg);
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

// Which CLI runs the session. Switching it swaps the model suggestions and
// clears a model picked for the other CLI.
const agentLabels = { claude: 'Claude Code', gemini: 'Gemini CLI' };
function agentSelect(value, model, list) {
  const agents = state.meta.agents || [{ name: 'claude', models: state.meta.models }];
  const sel = select(agents.map(a => [a.name, agentLabels[a.name] || a.name]), value || state.meta.default_agent || 'claude');
  const fill = () => list.replaceChildren(...(agents.find(a => a.name === sel.value)?.models || []).map(m => h('option', { value: m })));
  sel.addEventListener('change', () => { model.value = ''; fill(); });
  fill();
  return sel;
}

// Working directory picked from the list in Settings; '' falls back to defaultLabel's directory.
// A value no longer in the list stays selectable so it isn't lost silently.
function workdirSelect(value, defaultLabel, attrs) {
  const dirs = state.workdirs.includes(value) || !value ? state.workdirs : [...state.workdirs, value];
  return select([['', defaultLabel], ...dirs.map(d => [d, d])], value || '', attrs);
}

// workdirSelect for a project, task or rule x: its workdir here, or '' for
// the shared default it was created with (on any computer), if it has one.
function localWorkdirSelect(x, defaultLabel, attrs) {
  const shared = x.workdir_default || '';
  const label = shared ? `Shared default (${shared})` : defaultLabel;
  return workdirSelect(x.workdir === shared ? '' : x.workdir, label, attrs);
}

function remember(key, value) {
  try { localStorage.setItem(key, value); } catch (_) { /* storage unavailable */ }
}

function recall(key) {
  try { return localStorage.getItem(key) || ''; } catch (_) { return ''; }
}

// A flag kept across redraws and reloads; an unset flag leaves no key behind.
function flag(key) {
  return recall(key) === '1';
}

function setFlag(key, on) {
  try {
    if (on) localStorage.setItem(key, '1');
    else localStorage.removeItem(key);
  } catch (_) { /* storage unavailable */ }
}

// A session collapsed to its head stays collapsed across redraws and reloads.
const sessionCollapsed = id => flag(`session.collapsed.${id}`);
const setSessionCollapsed = (id, on) => setFlag(`session.collapsed.${id}`, on);

// So does a turn's prompt or reply folded to one line, by the id of the prompt; part is 'prompt' or 'reply'.
const foldCollapsed = (part, id) => flag(`${part}.collapsed.${id}`);
const setFoldCollapsed = (part, id, on) => setFlag(`${part}.collapsed.${id}`, on);

function trunc(s, n) {
  s = (s || '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// The one line a folded reply shows: its tool calls and, once the turn finished, when.
function replyLine(tools, end) {
  return ['Reply', tools ? (tools === 1 ? '1 tool call' : `${tools} tool calls`) : '', end ? '✓ ' + end : '']
    .filter(Boolean).join(' · ');
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
    task: () => renderTaskLink(Number(parts[1]), Number(parts[3]) || 0),
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
  // Pages showing one session or memory stay on it; the others redraw for the project.
  if (!/^#\/(session|sessions|memory|settings)\b/.test(location.hash)) route();
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
  const l = state.board && state.board.all_lanes.find(l => l.id === id);
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

function watchedBadge(t) {
  return t.watched ? h('span', { class: 'badge watched', title: 'Watched — listed in ⌥J, counted in the project picker (⌥K)' }, '👁') : null;
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

// How a task's latest agent turn ended, shown until the user dismisses the
// session, moves the task or a new turn starts.
const outcomes = {
  finished: ['◐ turn finished', 'The agent finished its turn: look at what it did'],
  done: ['✓ done', 'The agent finished and the task reached its done lane'],
  failed: ['✕ failed', 'The agent\'s turn failed'],
  cancelled: ['■ cancelled', 'The agent\'s turn was stopped'],
};

function outcomeBadge(taskId, st) {
  const o = st && outcomes[st.status];
  if (!o) return null;
  return h('a', {
    class: 'badge outcome-' + st.status, href: sessionHref({ task_id: taskId, id: st.session_id }),
    title: (st.reason ? `${o[1]}: ${st.reason}` : o[1]) + ' — open the session',
    onclick: e => { e.preventDefault(); e.stopPropagation(); openTaskDialog(taskId, st.session_id); },
  }, o[0]);
}

// A task's whole agent status, for its dialog.
function agentStatusEl(taskId, st) {
  if (!st) return null;
  if (st.status === 'running') return runningBadge(1);
  if (st.status === 'needs_you') return needsInputBadge(1, 'The agent ' + st.reason);
  return outcomeBadge(taskId, st);
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

// How full an agent's context is: tokens in it and, once its model's window is known, the share used.
function contextEl(tokens, window) {
  if (!tokens) return null;
  const k = n => n >= 1e6 ? (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M' : Math.round(n / 1000) + 'k';
  if (!window) return h('span', { class: 'context small muted', title: 'Tokens in the agent\'s context' }, `ctx ${k(tokens)}`);
  const pct = Math.min(100, Math.round(tokens / window * 100));
  const cls = pct >= 90 ? 'danger' : pct >= 70 ? 'warn' : null;
  return h('span', { class: 'context small muted', title: `Context: ${tokens.toLocaleString()} of ${window.toLocaleString()} tokens` },
    'ctx ', h('span', { class: 'meter' }, h('span', { class: cls, style: `width:${pct}%` })), h('span', { class: cls }, `${pct}%`),
    h('span', {}, ` ${k(tokens)}/${k(window)}`));
}

// ---------- board ----------

let dragId = null;

async function renderBoard() {
  const b = await loadBoard();
  const board = h('div', { class: 'board' });
  for (const lane of b.lanes) {
    // The server orders each lane: by due time, then the rest as dragged.
    const tasks = b.tasks.filter(t => t.lane_id === lane.id);
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

// Under a card whose agent runs: how full its context is and the usage left.
function runningMeta(ctx, lim) {
  const parts = [ctx && contextEl(ctx.tokens, ctx.window), lim && limitsEl(lim)].filter(Boolean);
  return parts.length ? h('div', { class: 'card-meta' }, ...parts) : null;
}

function card(t) {
  // A click opens the task in a dialog over the board.
  const el = h('article', { class: 'card', draggable: 'true', onclick: () => openTaskDialog(t.id) },
    h('div', { class: 'card-title' }, t.title),
    h('div', { class: 'card-meta' }, watchedBadge(t), kindBadge(t.kind), dueBadge(t), buildsOnBadge(t), needsInputBadge(state.board.needs_input[t.id]), runningBadge(state.board.running[t.id]), outcomeBadge(t.id, state.board.agent_status[t.id]), fieldLinks(t)),
    runningMeta(state.board.running_context[t.id], state.board.running_limits[t.id]));
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
      .sort((a, c) => laneOrder[a.lane_id] - laneOrder[c.lane_id]); // stable: keeps the server's order within a lane
    tbody.replaceChildren(...rows.map(t => h('tr', { class: 'clickable', onclick: () => openTaskDialog(t.id) },
      h('td', {}, h('div', {}, t.title), t.description ? h('div', { class: 'muted small' }, trunc(t.description, 120)) : null),
      h('td', {}, laneName(t.lane_id)),
      h('td', {}, kindBadge(t.kind) || 'task'),
      h('td', { class: 'small', title: fmtTime(t.due_at) }, dueBadge(t)),
      h('td', {}, needsInputBadge(b.needs_input[t.id]), runningBadge(b.running[t.id]), outcomeBadge(t.id, b.agent_status[t.id]), fieldLinks(t)),
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
    tbody.replaceChildren(...rows.map(t => h('tr', { class: 'clickable', onclick: () => openTaskDialog(t.id) },
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

// ---------- task link ----------

// A task's link (#/task/7, #/task/7/s/3) opens the board with the task in its dialog, the session
// (if any) scrolled to; the address goes back to the board so closing the dialog leaves it there.
async function renderTaskLink(id, focusSession) {
  history.replaceState(null, '', '#/board');
  document.querySelectorAll('[data-nav]').forEach(a => a.classList.toggle('active', a.dataset.nav === 'board'));
  await renderBoard();
  await openTaskDialog(id, focusSession);
}

// The task description, read as rendered markdown; Edit (or a double click) swaps in the textarea, which
// saves on change and goes back to the rendered view on Done. Expand reads it in a wide dialog. An empty
// description starts in the textarea.
function descriptionField(text, save) {
  const area = h('textarea', { rows: 10, class: 'mono', placeholder: 'Markdown', onchange: () => save(area.value) }, text);
  const view = h('div', { class: 'md desc-view', title: 'Double-click to edit', ondblclick: () => show(true, true) });
  const toggle = h('button', { class: 'ghost small', onclick: () => show(area.classList.contains('hidden'), true) });
  const expand = h('button', { class: 'ghost small', title: 'Read it in a wide dialog', onclick: () => {
    const dlg = document.getElementById('dialog');
    dlg.replaceChildren(h('div', { class: 'md desc-dialog' }, markdown(area.value)),
      h('div', { class: 'dialog-actions' }, h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Close', hint('Esc'))));
    dlg.classList.add('wide');
    dlg.addEventListener('close', () => dlg.classList.remove('wide'), { once: true });
    dlg.showModal();
  } }, 'Expand');
  const show = (editing, focus) => {
    if (!editing) view.replaceChildren(...markdown(area.value));
    area.classList.toggle('hidden', !editing);
    view.classList.toggle('hidden', editing);
    expand.classList.toggle('hidden', !area.value.trim());
    toggle.textContent = editing ? 'Done' : 'Edit';
    if (editing && focus) area.focus();
  };
  show(!String(text || '').trim(), false);
  return [h('div', { class: 'row desc-bar' }, h('label', { class: 'grow' }, 'Description'), toggle, expand), view, area];
}

// ---------- task dialog: every page opens a task here, over itself ----------

// A click on a task link opens the task's dialog over the page on screen; ⌘/Ctrl-click still opens a new tab.
function openTaskLink(e) {
  const a = e.target.closest && e.target.closest('a[href^="#/task/"]');
  const to = a && taskHash(a.getAttribute('href'));
  if (!to || e.metaKey || e.ctrlKey || e.shiftKey || e.button) return;
  e.preventDefault();
  openTaskDialog(to.id, to.session);
}

const taskDialog = { el: null, timers: [], id: 0 };

async function openTaskDialog(id, focusSession) {
  closeTaskDialog();
  const dlg = taskDialog.el || (taskDialog.el = document.getElementById('task-dialog'));
  if (!dlg.dataset.wired) {
    dlg.dataset.wired = '1';
    dlg.addEventListener('close', () => {
      taskDialog.timers.forEach(clearInterval);
      taskDialog.timers = [];
      taskDialog.id = 0;
      dlg.replaceChildren();
      // The page under it shows what changed in the task.
      if (location.hash === '' || location.hash.startsWith('#/board')) renderBoardInPlace();
      else if (/^#\/(list|archive)\b/.test(location.hash)) route();
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
  taskDialog.id = id;
  dlg.replaceChildren(
    h('div', { class: 'task-dialog-bar' },
      h('span', { class: 'task-id', title: 'The task\'s id, as text refers to it' }, '#' + id),
      page.title,
      page.done,
      page.watch,
      h('button', { class: 'ghost small', title: 'Close (Esc)', onclick: () => dlg.close() }, '✕')),
    page.el);
  if (!dlg.open) dlg.showModal();
  page.focus(focusSession);
}

function closeTaskDialog() {
  if (taskDialog.el && taskDialog.el.open) taskDialog.el.close();
}

// The task's editor and its sessions, for the task dialog.
// `every` polls for as long as the host shows it, `leave` runs once the task is
// archived or deleted, and `reload` redraws it after a restore.
async function taskView(id, { every, leave, reload }) {
  await loadBoard();
  const data = await api('GET', `/api/tasks/${id}`);
  const t = data.task;
  // A task opened from another project (a notification, ⌥J) brings its project into focus.
  if (t.project_id !== state.projectId) {
    state.projectId = t.project_id;
    remember('project', t.project_id);
    await loadBoard();
  }
  const save = patch => api('PATCH', `/api/tasks/${id}`, patch).then(nt => Object.assign(t, nt));

  // --- side panel ---
  const side = h('div', { class: 'task-side' });
  const sessionPanel = newSessionPanel(t, s => addSession(s));
  const buildsOn = buildsOnEditor(t, data.builds_on, data.used_by, async ids => {
    const refs = await save({ context_from: ids }).then(() => api('GET', `/api/tasks/${id}`)).then(d => d.builds_on);
    sessionPanel.drawContextLabel();
    return refs;
  });
  // A change of the memories shows in the context label and the sessions' Update memory.
  const memoriesChanged = d => {
    t.memories = d.task.memories;
    sessionPanel.drawContextLabel();
    components.forEach(c => c.drawHead());
    return d.memories;
  };
  const memories = memoryEditor(t, data.memories, {
    setIds: ids => save({ memories: ids }).then(() => api('GET', `/api/tasks/${id}`)).then(memoriesChanged),
    reload: () => api('GET', `/api/tasks/${id}`).then(memoriesChanged),
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

  // The title heads the dialog's top bar, left of the watch eye.
  const title = h('input', { class: 'title-input', value: t.title, onchange: e => save({ title: e.target.value }) });

  // Watching a task counts it in the ⌥K picker until it reaches Done; ⌥W clicks this icon. The dialog puts
  // it in its top bar, beside close.
  const watch = h('button', {
    class: 'ghost small watch-toggle', onclick: async () => { await save({ watched: !t.watched }); drawWatch(); },
  }, '👁');
  const drawWatch = () => {
    watch.title = t.watched ? 'Watched — click to stop watching (⌥W)' : 'Watch: list this task in ⌥J and count it in the project picker (⌥K) until it reaches Done (⌥W)';
    watch.setAttribute('aria-pressed', String(!!t.watched));
    watch.classList.toggle('on', !!t.watched);
  };
  drawWatch();

  // The lane select in the side panel; Done in the top bar (⌥D) moves the task to the lane named Done.
  const moveTo = async laneId => {
    const res = await api('POST', `/api/tasks/${id}/move`, { lane_id: laneId, index: -1 });
    if (res.started.length) toast(`Started ${res.started.map(s => s.title).join(', ')}`);
    await reloadStatus();
  };
  const lane = select(state.board.all_lanes.map(l => [l.id, l.name]), t.lane_id, { onchange: e => moveTo(Number(e.target.value)) });
  const doneLane = state.board.all_lanes.find(l => l.name.toLowerCase() === 'done');
  const done = h('button', {
    class: 'ghost small done-toggle', title: 'Move the task to Done (⌥D)',
    onclick: async () => { await moveTo(doneLane.id); toast('Moved to ' + doneLane.name); },
  }, '✓ Done', hint('⌥D'));
  const drawDone = () => done.classList.toggle('hidden', !doneLane || t.lane_id === doneLane.id || !!t.archived_at);
  drawDone();

  const archived = t.archived_at
    ? h('div', { class: 'row archived-note' }, h('span', { class: 'grow' }, 'Archived ' + ago(t.archived_at)),
      h('button', { class: 'small', onclick: async () => { await api('POST', `/api/tasks/${id}/unarchive`); reload(); } }, 'Restore'))
    : null;
  // The agent status, redrawn when a session's state or the lane changes.
  const agentStatus = h('div', { class: 'card-meta agent-status' });
  const drawStatus = st => agentStatus.replaceChildren(...[agentStatusEl(id, st)].filter(Boolean));
  // Also catches a move by an agent turn's done lane while the dialog is open.
  const reloadStatus = () => api('GET', `/api/tasks/${id}`).then(d => {
    Object.assign(t, { lane_id: d.task.lane_id, watched: d.task.watched });
    if (document.activeElement !== lane) lane.value = t.lane_id;
    drawStatus(d.agent_status);
    drawWatch();
    drawDone();
  });
  drawStatus(data.agent_status);
  side.append(h('div', { class: 'panel' },
    archived,
    agentStatus,
    h('label', {}, 'Project'),
    select(state.board.projects.map(p => [p.id, p.name]), t.project_id, {
      onchange: async e => { await save({ project_id: Number(e.target.value) }); toast('Moved to ' + projectName(t.project_id)); },
    }),
    h('label', {}, 'Lane'),
    lane,
    h('label', {}, 'Kind'),
    select(state.meta.kinds.map(k => [k, k]), t.kind, { onchange: e => save({ kind: e.target.value }) }),
    h('label', {}, 'Due — you get a notification at this time'),
    h('div', { class: 'field-row' }, due, clearDue),
    descriptionField(t.description, text => save({ description: text })),
    h('label', {}, 'Working directory for agents'),
    localWorkdirSelect(t, `Project default (${taskWorkdir({ ...t, workdir: '' })})`, { onchange: e => save({ workdir: e.target.value }) }),
    fields,
    memories.el,
    buildsOn.el,
    h('div', { class: 'row', style: 'margin-top:14px' },
      h('span', { class: 'muted small grow' }, 'Created ' + fmtTime(t.created_at)),
      h('button', { class: 'ghost', title: 'Copy a link to this task, to paste wherever you refer to it', onclick: () => copyTaskLink(id) },
        'Copy link', hint('⌥C')),
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
    const c = sessionComponent(s, s.messages || [], t);
    components.set(s.id, c);
    sessionsEl.prepend(c.el); // newest first
  };
  data.sessions.forEach(addSession);

  const artifacts = artifactsPanel(id, data.artifacts);
  const sessionsTab = h('div', {}, sessionPanel.el, artifacts.el, sessionsEl,
    data.sessions.length ? null : h('p', { class: 'muted' }, 'No agent sessions yet.'));

  // --- tabs: the sessions in full, or the activity thread their agents and the user keep ---
  const activity = activityPanel(id, data.activity, { openSession: sid => focus(sid), onChange: () => drawTabs() });
  const tabSessions = h('button', { class: 'tab', onclick: () => showTab('sessions') }, 'Sessions');
  const tabActivity = h('button', { class: 'tab', onclick: () => showTab('activity') });
  const drawTabs = () => {
    const open = activity.openCount();
    tabActivity.replaceChildren('Activity', activity.count() ? h('span', { class: 'pill' }, activity.count()) : null,
      open ? h('span', { class: 'badge needs-input' }, open > 1 ? `✋ ${open} questions` : '✋ question') : null);
  };
  const showTab = name => {
    remember('task.tab', name);
    sessionsTab.classList.toggle('hidden', name !== 'sessions');
    activity.el.classList.toggle('hidden', name !== 'activity');
    tabSessions.classList.toggle('active', name === 'sessions');
    tabActivity.classList.toggle('active', name === 'activity');
  };
  drawTabs();
  // An open question shows first; otherwise the tab last looked at.
  showTab(activity.openCount() || recall('task.tab') === 'activity' ? 'activity' : 'sessions');
  const main = h('div', {}, h('div', { class: 'task-tabs' }, tabSessions, tabActivity), sessionsTab, activity.el);
  let sessionsState = JSON.stringify(data.sessions.map(s => [s.id, s.status, s.needs_input, s.seen]));
  every(2000, async () => {
    components.forEach(c => c.drawLimits());
    activity.reload();
    const list = await api('GET', `/api/sessions?task_id=${id}`);
    const now = JSON.stringify(list.map(s => [s.id, s.status, s.needs_input, s.seen]).reverse());
    if (now !== sessionsState) { sessionsState = now; reloadStatus(); }
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

  // Scrolls to the end of the linked session, or else of the newest open one, where its latest turn and
  // the reply box are: a long transcript is read from its bottom up. 'activity' opens the thread at its
  // newest message and the box to answer in.
  function focus(sessionId) {
    if (sessionId === 'activity' || (!sessionId && activity.isShown())) {
      showTab('activity');
      activity.focus();
      return;
    }
    if (sessionId) showTab('sessions');
    const c = sessionId ? components.get(sessionId) : [...components.values()].reverse().find(c => !c.el.classList.contains('collapsed'));
    if (!c) return;
    if (c.el.classList.contains('collapsed')) c.setCollapsed(false); // a link to a session opens it
    c.el.scrollIntoView({ block: 'end' });
  }

  return {
    el: h('div', { class: 'task-page' }, side, main),
    title,
    done,
    watch,
    focus,
  };
}

// The task's activity thread: short messages from its agent sessions (each linking to its session) and
// the user, oldest first. Open questions stand out with an Answer button. The user's message answers the
// question picked, or else the latest open one, and goes to the session that asked; with none open it
// only joins the thread, where the task's next agent reads it.
function activityPanel(taskId, messages, { openSession, onChange }) {
  let xs = messages || [];
  let shown = JSON.stringify(xs);
  let answerTo = 0; // the question picked with Answer; 0: the latest open one
  const list = h('div', { class: 'activity-list' });
  const target = h('div', { class: 'muted small activity-target' });
  const input = h('textarea', { rows: 2, placeholder: 'Message the task\'s agents… (⌘↵ to send)' });
  const send = h('button', {}, 'Send', hint('⌘↵'));
  const openQuestions = () => xs.filter(m => m.question && !m.answered_at);
  const author = m => m.session_id
    ? h('a', {
      href: '#', class: 'activity-author', title: 'Open the session',
      onclick: e => { e.preventDefault(); openSession(m.session_id); },
    }, `${m.session_title || 'Session'} · #${m.session_id}`)
    : h('strong', { class: 'activity-author' }, 'You');
  const drawTarget = () => {
    const open = openQuestions();
    if (!open.some(m => m.id === answerTo)) answerTo = 0;
    const q = open.find(m => m.id === answerTo) || open[open.length - 1];
    target.replaceChildren(...(q
      ? ['Answers ', h('em', {}, trunc(q.body, 80)), ` — goes to ${q.session_title || 'the session that asked'}, which continues`,
        answerTo ? h('button', { class: 'ghost small', title: 'Answer the latest open question instead', onclick: () => { answerTo = 0; drawTarget(); } }, '✕') : null]
      : ['No open question: the message stays on the thread and starts no session.']));
  };
  const draw = () => {
    list.replaceChildren(...(xs.length ? xs.map(m => {
      const open = m.question && !m.answered_at;
      return h('div', { class: 'activity-msg' + (m.session_id ? '' : ' mine') + (open ? ' open' : '') },
        h('div', { class: 'activity-head' }, author(m),
          h('span', { class: 'muted small', title: fmtTime(m.created_at) }, ago(m.created_at)),
          m.question ? h('span', { class: 'badge' + (open ? ' needs-input' : '') }, open ? '✋ question' : 'answered') : null,
          // Only the computer a session runs on can pass it the answer.
          open ? h('button', {
            class: 'ghost small', disabled: !!m.session_node, title: m.session_node ? `Started on ${m.session_node} — read-only here` : null,
            onclick: () => { answerTo = m.id; drawTarget(); input.focus(); },
          }, 'Answer') : null),
        h('div', { class: 'md' }, markdown(m.body)));
    }) : [h('p', { class: 'muted' }, 'No activity yet: the agents working on this task post short notes and questions here.')]));
    drawTarget();
  };
  const reload = async () => {
    const now = await api('GET', `/api/tasks/${taskId}/messages`);
    const j = JSON.stringify(now);
    if (j === shown) return;
    shown = j;
    xs = now;
    draw();
    onChange();
  };
  const submit = async () => {
    const body = input.value.trim();
    if (!body || send.disabled) return;
    send.disabled = true;
    try {
      const res = await api('POST', `/api/tasks/${taskId}/messages`, answerTo ? { body, answer_to: answerTo } : { body });
      input.value = '';
      answerTo = 0;
      if (res.answered_id) toast(res.queued ? 'Answer queued: the session is still working' : 'Answer sent: the session continues');
      await reload();
    } finally {
      send.disabled = false;
    }
  };
  send.addEventListener('click', submit);
  submitOnCmdEnter([input], submit);
  draw();
  const el = h('div', { class: 'activity' }, list, h('div', { class: 'activity-composer' }, target, h('div', { class: 'row' }, input, send)));
  return {
    el,
    reload,
    count: () => xs.length,
    openCount: () => openQuestions().length,
    isShown: () => !el.classList.contains('hidden'),
    focus: () => { el.scrollIntoView({ block: 'end' }); input.focus(); },
  };
}

// The tasks this one builds on, editable, and the tasks that build on it.
// setIds saves a new list and resolves to the updated refs.
function buildsOnEditor(task, refs, usedBy, setIds) {
  const el = h('div');
  const taskLink = r => h('a', { href: `#/task/${r.id}` }, `#${r.id} ${r.title}`, r.archived ? ' (archived)' : '');
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
// reload fetches it again; both resolve to the updated refs.
function memoryEditor(task, refs, { setIds, reload }) {
  const el = h('div');
  const add = memorySearch({
    exclude: x => refs.some(r => r.id === x.id),
    onPick: async x => { refs = await setIds([...refs.map(r => r.id), x.id]); draw(); add.input.focus(); },
  });
  const draw = () => {
    el.replaceChildren(...[
      h('label', {}, 'Memory — agents read it and update it after each turn'),
      refs.length ? null : h('div', { class: 'muted small' }, 'None: what agents find out stays in their sessions.'),
      refs.map(r => h('div', { class: 'field-row' },
        h('span', { class: 'link-ref' }, h('a', { href: `#/memory/${r.id}`, onclick: closeTaskDialog }, r.name)),
        h('button', {
          class: 'ghost small', title: 'Unlink this memory',
          onclick: async () => { refs = await setIds(refs.filter(x => x.id !== r.id).map(x => x.id)); draw(); },
        }, '✕'))),
      add.el,
      h('button', {
        class: 'ghost small', title: 'A new memory on this task\'s topic, linked to it',
        onclick: () => memoryDialog({ task, done: async () => { refs = await reload(); draw(); } }),
      }, '+ New memory'),
    ].flat().filter(c => c != null));
  };
  draw();
  return { el };
}

// A search box with a list of matches: ↑/↓ choose, Enter (or a click) picks
// one, Esc closes the list. find(query) resolves to the matches (focusing the
// empty box lists find('')); label(x) draws one, none says there are none.
function searchBox({ placeholder, find, label, none, onPick }) {
  const input = h('input', { type: 'search', placeholder });
  const list = h('ul', { class: 'picker task-search', hidden: true });
  let found = [], i = 0, timer = 0, seq = 0;
  const draw = () => {
    list.replaceChildren(...found.map((x, j) => h('li', {
      class: j === i ? 'selected' : '',
      onmousedown: e => { e.preventDefault(); pick(x); },
      onmousemove: () => { if (i !== j) { i = j; draw(); } },
    }, ...label(x))));
    if (!found.length) list.append(h('li', { class: 'muted small' }, none));
    list.hidden = false;
    list.children[i]?.scrollIntoView({ block: 'nearest' });
  };
  const close = () => { clearTimeout(timer); seq++; list.hidden = true; };
  const search = async () => {
    const n = ++seq;
    const xs = await find(input.value.trim());
    if (n !== seq) return;
    found = xs.slice(0, 20);
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

// A search box over every task (archived ones included).
function taskSearch({ placeholder, exclude, note, onPick }) {
  return searchBox({
    placeholder, onPick, none: 'No matching tasks',
    find: async q => (await api('GET', '/api/tasks?limit=50&q=' + encodeURIComponent(q))).filter(x => !exclude(x)),
    label: x => [`#${x.id} ${x.title}`, ...note(x).filter(Boolean).map(n => h('span', { class: 'small task-search-note' }, ' · ' + n))],
  });
}

// A search box over the memories by the words of their name, description
// and body; the empty box lists the most recently used.
function memorySearch({ exclude, onPick }) {
  return searchBox({
    placeholder: 'Link a memory — search by words…', onPick, none: 'No matching memories',
    find: async q => (await api('GET', q ? '/api/memories?q=' + encodeURIComponent(q) : '/api/memories')).filter(x => !exclude(x)),
    label: x => [x.name, x.description ? h('span', { class: 'small task-search-note' }, ' · ' + trunc(x.description, 80)) : null].filter(Boolean),
  });
}

function newSessionPanel(task, onStarted) {
  const prompt = h('textarea', { rows: 4, placeholder: 'What should the agent do? Skills work too, e.g. /p-dev:review 123' });
  const mode = select(modeOptions(false), state.meta.default_mode);
  const [model, modelList] = modelInput('');
  const agentSel = agentSelect('', model, modelList);
  const ctx = h('input', { type: 'checkbox', checked: true });
  const ctxLabel = h('span');
  const drawContextLabel = () => {
    const n = task.context_from.length, m = task.memories.length;
    ctxLabel.textContent = 'Include task context' + (m ? `, ${m} memor${m > 1 ? 'ies' : 'y'}` : '') +
      (n ? ` and session summaries of ${n} earlier task${n > 1 ? 's' : ''}` : '');
  };
  drawContextLabel();
  const workdir = workdirSelect('', `Default (${taskWorkdir(task)})`);
  const doneLane = doneLaneSelect(state.board.all_lanes, '');
  const start = async () => {
    if (!prompt.value.trim()) return;
    const s = await api('POST', `/api/tasks/${task.id}/sessions`, {
      prompt: prompt.value, mode: mode.value, agent: agentSel.value, model: model.value, workdir: workdir.value, include_context: ctx.checked,
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
      h('div', { class: 'grow', style: 'min-width:140px' }, agentSel),
      h('div', { class: 'grow', style: 'min-width:200px' }, mode),
      h('div', { class: 'grow', style: 'min-width:140px' }, model, modelList),
      h('div', { class: 'grow', style: 'min-width:200px' }, workdir),
      h('div', { class: 'grow', style: 'min-width:200px' }, doneLane),
      h('label', { class: 'row', style: 'margin:0' }, ctx, ctxLabel),
      h('button', { onclick: start }, 'Start', hint('⌘↵'))));
  return { el, drawContextLabel };
}

// 7s, 2m 05s, 1h 02m
function fmtElapsed(secs) {
  const pad = n => String(n).padStart(2, '0');
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ${pad(secs % 60)}s`;
  return `${Math.floor(secs / 3600)}h ${pad(Math.floor(secs / 60) % 60)}m`;
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

// A turn is a prompt and everything up to the next prompt, its reply. The prompt and the reply
// each fold to one line on their own, from their chevron, and open again from that line.
function turnEl(m) {
  const turn = h('div', { class: 'turn', 'data-id': m.id });
  turn.dataset.prompt = m.content;
  turn.append(foldEl('prompt', m.id, messageEl(m)), foldEl('reply', m.id, h('div', { class: 'turn-body' })));
  drawTurnLine(turn);
  return turn;
}

function foldEl(part, id, content) {
  const fold = h('div', { class: 'fold ' + part, 'data-part': part });
  fold.append(
    h('div', {
      class: 'turn-line', role: 'button', tabindex: 0, title: `Expand the ${part}`, 'aria-expanded': 'false',
      onclick: () => flipFold(fold, false),
      onkeydown: e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); flipFold(fold, false); } },
    }),
    h('button', {
      class: 'turn-toggle', title: `Fold the ${part} to one line`, 'aria-label': `Fold the ${part}`, 'aria-expanded': 'true',
      onclick: () => flipFold(fold, true) }, h('span', { class: 'turn-caret' })),
    content);
  fold.classList.toggle('collapsed', foldCollapsed(part, id));
  return fold;
}

function setFold(fold, on) {
  fold.classList.toggle('collapsed', on);
  setFoldCollapsed(fold.dataset.part, fold.closest('.turn').dataset.id, on);
}

// Folds the prompt and the reply alike.
function setTurn(turn, on) {
  turn.querySelectorAll(':scope > .fold').forEach(f => setFold(f, on));
}

// Folding from a part's own control hands focus to the other one, so the keyboard isn't left on a hidden element.
function flipFold(fold, on) {
  setFold(fold, on);
  fold.querySelector(on ? '.turn-line' : '.turn-toggle').focus({ preventScroll: true });
}

function drawTurnLine(turn) {
  const [prompt, reply] = turn.querySelectorAll(':scope > .fold > .turn-line');
  prompt.replaceChildren(h('span', { class: 'turn-caret' }), trunc(turn.dataset.prompt, 120));
  reply.replaceChildren(h('span', { class: 'turn-caret' }), replyLine(turn.querySelectorAll('.tool-use').length, turn.dataset.end));
}

// Tool calls, their output and CLI-injected context between two messages fold into one collapsed group,
// so a session reads as the conversation; the group keeps growing while the agent works.
// Everything after a prompt lands in its turn, folded or not.
function appendMessage(body, m) {
  if (m.role === 'user' && m.kind === 'text') { body.append(turnEl(m)); return; }
  const turn = body.lastElementChild?.classList.contains('turn') ? body.lastElementChild : null;
  const into = turn ? turn.querySelector('.turn-body') : body;
  if (!isToolMsg(m)) {
    into.append(messageEl(m));
  } else {
    let group = into.lastElementChild;
    if (!group || !group.classList.contains('tools')) {
      group = h('details', { class: 'tools' }, h('summary', {}), h('div', { class: 'tools-body' }));
      into.append(group);
    }
    group.lastElementChild.append(messageEl(m));
    const n = group.querySelectorAll('.tools-body > .tool-use').length;
    group.firstElementChild.textContent = n === 1 ? '1 tool call' : `${n} tool calls`;
  }
  if (!turn) return;
  if (m.kind === 'result') turn.dataset.end = fmtTime(m.created_at);
  drawTurnLine(turn);
}

// sessionNode names the computer a session runs on.
const sessionNode = s => s.node_name || `node ${s.node_id}`;

// task: the session's task, if it is shown on the task's page.
function sessionComponent(session, messages, task) {
  let last = 0;
  const body = h('div', { class: 'session-body' });
  const head = h('div', { class: 'session-head' });
  const limits = h('div', { class: 'session-limits' });
  // Below the transcript while a turn runs, from the moment it starts, so a
  // turn shows it is under way before the agent's first message or tool call.
  const working = h('div', { class: 'working' });
  // Messages sent while the agent works, under "Working…"; each starts the next turn once the one before ends.
  const queued = h('div', { class: 'queued' });
  let turnStart = 0; // when the prompt of the latest turn was sent
  const input = h('textarea', { rows: 2, placeholder: 'Reply to continue this session… (⌘↵ to send)' });
  const send = h('button', {}, 'Send', hint('⌘↵'));
  // Task sessions can change where the task goes when the agent finishes, even mid-run.
  const doneLane = session.task_id && state.board ? doneLaneSelect(state.board.all_lanes, session.done_lane_id, {
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
      await refresh();
    },
  }) : null;
  // A session another node runs goes on only there: here it is read-only.
  const readOnly = h('div', { class: 'read-only-bar muted' });
  const foot = h('div', { class: 'session-foot' }, readOnly, input,
    h('div', { class: 'row', style: 'margin-top:6px; justify-content:flex-end' }, runSkill, doneLane, send));
  // What tasks building on this one get instead of the transcript; collapsed
  // until opened, and the open state survives redraws.
  const summary = h('details', { class: 'session-summary' });
  const el = h('div', { class: 'session' }, head, summary, body, working, queued, limits, foot);
  const c = { el, session, lastId: () => last };
  // Collapsed, only the head shows; the title toggles it too.
  c.setCollapsed = on => {
    el.classList.toggle('collapsed', on);
    setSessionCollapsed(c.session.id, on);
    drawHead();
  };
  el.classList.toggle('collapsed', sessionCollapsed(session.id));

  // Shows a turn that just started (status and prompt) without waiting for the next poll.
  const refresh = async () => {
    const upd = await api('GET', `/api/sessions/${c.session.id}?after=${last}`);
    c.update(upd.session, upd.messages);
  };

  const doResume = async () => {
    await api('POST', `/api/sessions/${c.session.id}/resume`);
    await refresh();
  };

  const doSend = async () => {
    if (!input.value.trim()) return;
    await api('POST', `/api/sessions/${c.session.id}/messages`, { text: input.value });
    input.value = '';
    await refresh();
  };
  send.addEventListener('click', doSend);
  input.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) doSend(); });

  function drawHead() {
    const s = c.session;
    const owned = s.owned !== false;
    const running = s.status === 'running';
    const resumable = owned && ['interrupted', 'cancelled', 'error'].includes(s.status);
    const resumeCmd = owned && s.claude_session_id ? `cd ${JSON.stringify(s.workdir)} && ${s.agent || 'claude'} --resume ${s.claude_session_id}` : '';
    head.replaceChildren(...[
      h('span', { class: 'title', onclick: () => c.setCollapsed(!el.classList.contains('collapsed')) }, s.title || `Session ${s.id}`),
      h('span', { class: 'badge status-' + s.status }, running ? h('span', { class: 'pulse' }) : null, s.status),
      needsInputBadge(s.needs_input ? 1 : 0, 'The agent ' + s.needs_input),
      s.needs_input || (!running && !s.seen && s.task_id) ? h('button', {
        class: 'ghost small', title: s.needs_input ? 'No answer needed: clear the tag' : 'Seen how the turn ended: clear the task\'s status',
        onclick: async () => {
          await api('DELETE', `/api/sessions/${s.id}/needs-input`);
          Object.assign(c.session, { needs_input: '', seen: true });
          drawHead();
        },
      }, 'Dismiss') : null,
      s.agent && s.agent !== 'claude' ? h('span', { class: 'badge' }, agentLabels[s.agent] || s.agent) : null,
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
      running && owned ? h('button', { class: 'danger small', onclick: () => api('POST', `/api/sessions/${s.id}/cancel`) }, 'Stop') : null,
      resumable ? h('button', { class: 'small', title: 'Continue this session where it stopped', onclick: doResume }, 'Resume') : null,
      s.memorizing ? h('span', { class: 'badge' }, h('span', { class: 'pulse' }), 'updating memory') : null,
      !running && !s.memorizing && task?.memories.length && s.claude_session_id ? h('button', {
        class: 'ghost small', title: 'Update the task\'s memories with what this session found out (it also happens after every turn)',
        onclick: async () => {
          await api('POST', `/api/sessions/${s.id}/memory`);
          c.session.memorizing = true;
          drawHead();
          toast('Updating the memories…');
        },
      }, 'Update memory') : null,
      owned && !running && s.claude_session_id ? h('button', {
        class: 'ghost small', title: 'Have the agent write a brief and continue in a new session with a fresh context',
        onclick: () => handoffDialog(s, refresh),
      }, 'Hand off') : null,
      !el.classList.contains('collapsed') && body.querySelectorAll(':scope > .turn').length > 1 ? h('button', {
        class: 'ghost small', title: 'Fold the prompt and the reply of every turn but the last',
        onclick: () => [...body.querySelectorAll(':scope > .turn')].slice(0, -1).forEach(t => setTurn(t, true)),
      }, 'Fold turns') : null,
      el.classList.contains('collapsed')
        ? h('button', { class: 'ghost small', title: 'Expand the session', onclick: () => c.setCollapsed(false) }, '▸')
        : h('button', { class: 'ghost small', title: 'Collapse the session to its head', onclick: () => c.setCollapsed(true) }, '▾'),
    ].filter(Boolean));
    // While the agent works a reply is queued; the first turn may not have its agent session yet.
    send.disabled = !running && !s.claude_session_id;
    input.placeholder = running ? 'The agent is working: a reply waits in the queue until it finishes… (⌘↵ to queue)'
      : 'Reply to continue this session… (⌘↵ to send)';
    if (runSkill) runSkill.disabled = running || !s.claude_session_id;
    readOnly.textContent = owned ? '' : `Started on ${sessionNode(s)} — read-only here`;
    readOnly.hidden = owned;
    for (const x of [input, send, runSkill]) if (x) x.hidden = !owned;
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
    limits.replaceChildren(...[contextEl(s.context_tokens, s.context_window),
      s.status === 'running' || known ? limitsEl(s.limits) : null].filter(Boolean));
  }

  function drawWorking() {
    if (c.session.status !== 'running') { working.replaceChildren(); return; }
    const secs = Math.max(0, Math.round((Date.now() - (turnStart || Date.now())) / 1000));
    working.replaceChildren(h('span', { class: 'pulse' }), 'Working…', h('span', { class: 'muted small' }, fmtElapsed(secs)));
  }

  function drawQueued() {
    queued.replaceChildren(...(c.session.queued || []).map(q => h('div', { class: 'queued-msg' },
      h('span', { class: 'label' }, 'Queued'),
      h('span', { class: 'text' }, q.content),
      h('button', {
        class: 'ghost small', title: 'Don\'t send this message', hidden: c.session.owned === false,
        onclick: async () => {
          await api('DELETE', `/api/queued-messages/${q.id}`);
          c.session.queued = c.session.queued.filter(x => x.id !== q.id);
          drawQueued();
        },
      }, '✕'))));
  }

  // The countdowns and the turn's elapsed time tick even when nothing else changes.
  c.drawLimits = () => { drawLimits(); drawWorking(); };
  c.drawHead = drawHead;

  c.update = (s, msgs) => {
    c.session = s;
    if (doneLane && document.activeElement !== doneLane) doneLane.value = s.done_lane_id ?? '';
    for (const m of msgs) {
      if (m.id <= last) continue; // a refresh and the poll may both fetch it
      appendMessage(body, m);
      last = m.id;
      if (m.role === 'user') turnStart = Date.parse(m.created_at);
    }
    drawHead();
    drawWorking();
    drawQueued();
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
  const doneLane = doneLaneSelect(state.board.all_lanes, '');
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
  if (!state.board) await loadBoard();
  const statusSel = select([['', 'All statuses'], ['running', 'Running'], ['idle', 'Idle'], ['error', 'Error'],
    ['cancelled', 'Cancelled'], ['interrupted', 'Interrupted']], '', { onchange: draw });
  const search = h('input', { type: 'search', placeholder: 'Search by words of the task title or description…', value: recall('sessions-search') });
  const tbody = h('tbody');
  const empty = h('p', { class: 'muted' });
  let seq = 0, timer = 0;
  async function draw() {
    const q = search.value.trim(), n = ++seq;
    remember('sessions-search', q);
    const params = new URLSearchParams();
    if (statusSel.value) params.set('status', statusSel.value);
    if (q) params.set('q', q);
    const xs = await api('GET', '/api/sessions' + (params.size ? '?' + params : ''));
    if (n !== seq) return;
    empty.textContent = xs.length ? '' : q ? 'No session\'s task has these words.' : 'No agent sessions yet.';
    tbody.replaceChildren(...xs.map(s => h('tr', { class: 'clickable', onclick: () => openFromSessions(s) },
      h('td', {}, h('span', { class: 'badge status-' + s.status }, s.status === 'running' ? h('span', { class: 'pulse' }) : null, s.status),
        needsInputBadge(s.needs_input ? 1 : 0, 'The agent ' + s.needs_input),
        s.context_tokens ? h('div', {}, contextEl(s.context_tokens, s.context_window)) : null),
      h('td', { class: 'small', title: s.workdir }, s.project_id ? projectName(s.project_id) : h('span', { class: 'muted' }, '—')),
      h('td', {}, s.task_id ? h('div', {}, s.task_title) : h('div', { class: 'muted' }, 'No task'), h('div', { class: 'muted small' }, s.title)),
      h('td', { class: 'small' }, trunc(s.last_message, 220)),
      h('td', { class: 'muted small' }, s.trigger, h('br'), s.mode),
      h('td', { class: 'muted small' }, ago(s.updated_at)))));
  }
  view.replaceChildren(h('div', { class: 'panel' },
    h('div', { class: 'toolbar' }, h('h2', { class: 'grow', style: 'flex:1' }, 'Agent sessions'), statusSel,
      h('button', { onclick: sessionDialog }, '▶ New session')),
    search,
    h('table', {}, h('thead', {}, h('tr', {}, ['Status', 'Project', 'Task / session', 'Latest', 'Trigger', 'Updated'].map(x => h('th', {}, x)))), tbody),
    empty));
  search.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(draw, 150); });
  await draw();
  every(3000, draw);
}

// A task's session opens in the task's dialog, which brings the task's project into focus.
function openFromSessions(s) {
  if (s.task_id) openTaskDialog(s.task_id, s.id);
  else location.hash = sessionHref(s);
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

// opts.buildsOn: the task on screen; the new task builds on it (unless
// unlinked) and the session runs in its project.
async function sessionDialog(opts = {}) {
  if (!state.board) await loadBoard();
  const from = opts.buildsOn;
  const dlg = document.getElementById('dialog');
  const prompt = h('textarea', { rows: 6, placeholder: 'What should the agent do? Skills work too, e.g. /p-dev:review 123' });
  // Runs in the current project's directory, like its tasks' agents.
  const projectId = from ? from.project_id : state.projectId;
  const buildsOn = buildsOnPicker(from ? [from] : [], projectId);
  const where = h('p', { class: 'muted small' }, `Project ${projectName(projectId)} · ${taskWorkdir({ project_id: projectId })}`);
  const [model, modelList] = modelInput(recall('session.model'));
  const agentSel = agentSelect(recall('session.agent'), model, modelList);
  const mode = select(modeOptions(false), recall('session.mode') || state.meta.default_mode);
  // watch: the new task starts out watched (⌘⇧↵).
  const start = async (watch = false) => {
    if (!prompt.value.trim()) return;
    const s = await api('POST', '/api/sessions', { prompt: prompt.value, project_id: projectId, agent: agentSel.value, model: model.value, mode: mode.value, context_from: buildsOn.ids(), watched: watch });
    remember('session.agent', agentSel.value);
    remember('session.model', model.value);
    remember('session.mode', mode.value);
    dlg.close();
    const next = afterSessionStart(location.hash, s.id);
    if (next) go(next);
    else toast(`Task #${s.task_id} created, session started`);
  };
  submitOnCmdEnter([prompt], start);
  dlg.replaceChildren(h('h2', {}, 'New agent session'),
    h('label', {}, 'Prompt'), prompt,
    h('p', { class: 'muted small' }, 'A new task holds the session: its title is the first line of the prompt, its description the whole prompt.'),
    where,
    buildsOn.el,
    h('div', { class: 'row' },
      h('div', { class: 'grow' }, h('label', {}, 'Agent'), agentSel),
      h('div', { class: 'grow' }, h('label', {}, 'Model'), model, modelList),
      h('div', { class: 'grow' }, h('label', {}, 'Mode'), mode)),
    h('div', { class: 'dialog-actions' }, cancelButton(dlg),
      h('button', { class: 'ghost', onclick: () => start(true) }, 'Start & watch', hint('⌘⇧↵')),
      h('button', { onclick: () => start() }, 'Start', hint('⌘↵'))));
  dlg.showModal();
  prompt.focus();
}

// A new session starts in the background: the Sessions list redraws with it, a
// standalone session's page opens it, and any other page stays as it is (null).
function afterSessionStart(hash, id) {
  if (/^#\/sessions\b/.test(hash)) return '#/sessions';
  if (/^#\/session\//.test(hash)) return `#/session/${id}`;
  return null;
}

// ---------- memory ----------

const sourceLabel = { user: 'edited by hand', agent: 'updated by an agent', note: 'note added' };

async function renderMemories() {
  if (!state.board) await loadBoard();
  const all = await api('GET', '/api/memories'); // the most recently used first
  const filter = h('input', { type: 'search', placeholder: 'Filter by words of the name, description or body…', value: recall('memory-filter') || '' });
  const tbody = h('tbody');
  const empty = h('p', { class: 'muted' });
  let seq = 0, timer = 0;
  const draw = async () => {
    const q = filter.value.trim(), n = ++seq;
    remember('memory-filter', q);
    const xs = q ? await api('GET', '/api/memories?q=' + encodeURIComponent(q)) : all;
    if (n !== seq) return;
    tbody.replaceChildren(...xs.map(m => h('tr', { class: 'clickable', onclick: () => { location.hash = `#/memory/${m.id}`; } },
      h('td', {}, m.name, m.description ? h('div', { class: 'muted small' }, m.description) : null),
      h('td', { class: 'muted small' }, m.tasks),
      h('td', { class: 'muted small', title: 'Its latest revision, or the latest activity in a session of a task linked to it' }, ago(m.last_used)))));
    empty.textContent = xs.length ? '' : all.length ? 'No memory has these words.' : 'No memories yet.';
  };
  filter.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(draw, 150); });
  view.replaceChildren(h('div', { class: 'panel', style: 'max-width:900px' },
    h('div', { class: 'toolbar' }, h('h2', { class: 'grow', style: 'flex:1' }, 'Memory'),
      h('button', { onclick: () => memoryDialog() }, '+ New memory')),
    h('p', { class: 'muted small' }, 'Lasting knowledge on a topic, kept across tasks. Link a memory to tasks: their agents read it with the task context, ' +
      'and after each turn a copy of the agent rewrites it with what the session found out. Every change is kept as a revision. ' +
      'A new task is suggested the memories on its topic.'),
    filter,
    h('table', {}, h('thead', {}, h('tr', {}, ['Memory', 'Tasks', 'Last used'].map(x => h('th', {}, x)))), tbody),
    empty));
  await draw();
  filter.focus();
}

// opts.task: link the new memory to this task, optionally writing it from
// the task's sessions so far; then done() instead of opening the memory.
function memoryDialog(opts = {}) {
  const { task, done } = opts;
  const dlg = document.getElementById('dialog');
  const name = h('input', { placeholder: 'e.g. Productive API', value: task ? task.title : '' });
  const description = h('input', { placeholder: 'What belongs in it — agents go by this' });
  const fill = h('input', { type: 'checkbox', checked: true });
  const create = async () => {
    if (!name.value.trim()) return;
    const m = await api('POST', '/api/memories', {
      name: name.value, description: description.value, ...(task ? { task_id: task.id, fill: fill.checked } : {}),
    });
    dlg.close();
    if (!task) { location.hash = `#/memory/${m.id}`; return; }
    toast(fill.checked ? `Memory ${m.name} linked; writing it from the task's sessions…` : `Memory ${m.name} linked`);
    done?.(m);
  };
  submitOnCmdEnter([name, description], create);
  dlg.replaceChildren(...[h('h2', {}, task ? `New memory for #${task.id}` : 'New memory'),
    h('label', {}, 'Name — the topic'), name, h('label', {}, 'About'), description,
    task ? h('label', { class: 'row' }, fill, h('span', {}, 'Write it from this task\'s agent sessions so far')) : null,
    h('div', { class: 'dialog-actions' }, cancelButton(dlg), h('button', { onclick: create }, 'Create', hint('⌘↵')))].filter(Boolean));
  dlg.showModal();
  name.focus();
  name.select();
}

async function renderMemory(id) {
  if (!state.board) await loadBoard();
  let data = await api('GET', `/api/memories/${id}`);
  let m = data.memory;
  let base = m.revision, loaded = m.body; // the revision and text the editor started from
  const save = patch => api('PATCH', `/api/memories/${id}`, patch).then(nm => { m = nm; return nm; });

  const name = h('input', { class: 'title-input', value: m.name, onchange: e => save({ name: e.target.value }) });
  const description = h('input', { value: m.description, placeholder: 'What belongs in it — agents go by this', onchange: e => save({ description: e.target.value }) });
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
  const filling = h('div', { class: 'badge hidden' }, h('span', { class: 'pulse' }), 'being written from a task\'s sessions');
  const drawSide = () => {
    revisions.replaceChildren(...(data.revisions.length ? data.revisions.map((r, i) => h('div', { class: 'revision' },
      h('a', { href: '#', onclick: e => { e.preventDefault(); revisionDialog(r.id, i === 0, reload); } }, fmtTime(r.created_at)),
      h('div', { class: 'muted small' }, sourceLabel[r.source] || r.source,
        r.task_id ? [' · ', h('a', { href: r.session_id ? `#/task/${r.task_id}/s/${r.session_id}` : `#/task/${r.task_id}` }, `#${r.task_id}`)] : null)))
      : [h('p', { class: 'muted small' }, 'No revisions yet.')]));
    tasks.replaceChildren(...(data.tasks.length ? data.tasks.map(t => h('div', { class: 'link-ref' },
      h('a', { href: `#/task/${t.id}` }, `#${t.id} ${t.title}`), t.archived ? ' (archived)' : ''))
      : [h('p', { class: 'muted small' }, 'No task is linked to it yet; link it from a task.')]));
    filling.classList.toggle('hidden', !data.filling);
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
        h('div', { class: 'row', style: 'margin-top:14px' },
          h('button', { title: 'A new task linked to this memory: its agents read it and add to it', onclick: () => newTaskDialog({ memory: m }) },
            '+ Task on this topic')),
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
      h('div', { class: 'panel' }, h('h3', {}, 'Revisions'), filling, revisions)),
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
  const [rules, settings, skills, trello] = await Promise.all([api('GET', '/api/rules'), api('GET', '/api/settings'), api('GET', '/api/skills'),
    api('GET', '/api/trello')]);
  state.skills = skills;
  const rerender = () => renderSettings();

  const lanes = h('div', { class: 'panel' }, h('h2', {}, 'Lanes'),
    h('p', { class: 'muted small' }, 'Board columns, left to right.'),
    b.all_lanes.map((l, i) => {
      const name = h('input', { value: l.name });
      const color = h('input', { type: 'color', value: l.color || '#888888', style: 'width:42px;padding:2px' });
      const saveLane = () => api('PATCH', `/api/lanes/${l.id}`, { name: name.value, color: color.value });
      name.addEventListener('change', saveLane);
      color.addEventListener('change', saveLane);
      const move = async d => {
        const ids = b.all_lanes.map(x => x.id);
        [ids[i], ids[i + d]] = [ids[i + d], ids[i]];
        await api('POST', '/api/lanes/reorder', { ids });
        rerender();
      };
      return h('div', { class: 'row', style: 'margin-bottom:6px' }, color, h('div', { class: 'grow' }, name),
        h('button', { class: 'ghost', disabled: i === 0, onclick: () => move(-1) }, '↑'),
        h('button', { class: 'ghost', disabled: i === b.all_lanes.length - 1, onclick: () => move(1) }, '↓'),
        h('button', { class: 'danger', onclick: async () => { await api('DELETE', `/api/lanes/${l.id}`); rerender(); } }, 'Delete'));
    }),
    h('input', {
      placeholder: '+ New lane (Enter)', onkeydown: async e => {
        if (e.key === 'Enter' && e.target.value.trim()) { await api('POST', '/api/lanes', { name: e.target.value }); rerender(); }
      },
    }));

  const projects = h('div', { class: 'panel' }, h('h2', {}, 'Projects'),
    h('p', { class: 'muted small' }, 'Each project is a board of its own on the same lanes; pick one at the top, and untick the lanes it does not use (a hidden lane still shows while the project has tasks in it). Rules and auto-archive apply to all of them. ' +
      "Agents on a project's tasks run in its directory unless the task or session picks another."),
    b.projects.map((p, i) => {
      const name = h('input', { value: p.name });
      const color = h('input', { type: 'color', value: p.color || '#888888', style: 'width:42px;padding:2px' });
      const workdir = localWorkdirSelect(p, `App default (${state.meta.workdir})`, { title: 'Default working directory for agents' });
      const shows = b.all_lanes.map(l => h('input', { type: 'checkbox', checked: !p.hidden_lanes.includes(l.id) }));
      const saveProject = async () => {
        const hidden = b.all_lanes.filter((l, j) => !shows[j].checked).map(l => l.id);
        try {
          await api('PATCH', `/api/projects/${p.id}`, { name: name.value, color: color.value, workdir: workdir.value, hidden_lanes: hidden });
        } catch {
          rerender(); // e.g. the last shown lane: back to what is saved
          return;
        }
        loadBoard();
      };
      name.addEventListener('change', saveProject);
      color.addEventListener('change', saveProject);
      workdir.addEventListener('change', saveProject);
      shows.forEach(c => c.addEventListener('change', saveProject));
      const move = async d => {
        const ids = b.projects.map(x => x.id);
        [ids[i], ids[i + d]] = [ids[i + d], ids[i]];
        await api('POST', '/api/projects/reorder', { ids });
        rerender();
      };
      return h('div', { style: 'margin-bottom:10px' },
        h('div', { class: 'row' }, color, h('div', { class: 'grow' }, name), h('div', { class: 'grow' }, workdir),
          h('button', { class: 'ghost', disabled: i === 0, onclick: () => move(-1) }, '↑'),
          h('button', { class: 'ghost', disabled: i === b.projects.length - 1, onclick: () => move(1) }, '↓'),
          h('button', {
            class: 'danger', title: 'Only a project without tasks (archived ones included) can be deleted',
            onclick: async () => { await api('DELETE', `/api/projects/${p.id}`); rerender(); },
          }, 'Delete')),
        h('div', { class: 'row small muted', style: 'flex-wrap:wrap;gap:4px 12px;margin-top:4px' }, 'Lanes shown:',
          b.all_lanes.map((l, j) => h('label', { class: 'row', style: 'margin:0;gap:4px', title: 'Shown on this project\'s board' }, shows[j], l.name))));
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
    h('p', { class: 'muted small' }, 'Extra attributes every task can have: links to Productive, PRs, Slack threads… The url fields productive, pull_request, slack and trello fill themselves from a link of their kind in the task\'s description, and pull_request from the PR an agent opens.'),
    h('table', {}, h('tbody', {}, b.fields.map(f => h('tr', {},
      h('td', {}, h('code', {}, f.key)), h('td', {}, f.label), h('td', { class: 'muted' }, f.type),
      h('td', {}, h('button', {
        class: 'danger small', onclick: async () => {
          if (confirm(`Delete field ${f.key} and its values on all tasks?`)) { await api('DELETE', `/api/fields/${f.key}`); rerender(); }
        },
      }, 'Delete')))))),
    h('div', { class: 'row', style: 'margin-top:10px' }, h('div', { class: 'grow' }, fKey), h('div', { class: 'grow' }, fLabel), fType,
      h('button', { onclick: async () => { await api('POST', '/api/fields', { key: fKey.value, label: fLabel.value, type: fType.value }); rerender(); } }, 'Add')));

  const laneLabel = id => id == null ? 'Any lane' : (b.all_lanes.find(l => l.id === id) || {}).name || '?';
  const rulesPanel = h('div', { class: 'panel' },
    h('div', { class: 'row' }, h('h2', { class: 'grow' }, 'Transition rules'), h('button', { onclick: () => ruleDialog(null, b, skills, rerender) }, '+ Add rule')),
    h('p', { class: 'muted small' }, 'When a task moves between lanes, start an agent session automatically, run a skill in the task\'s latest session (a new one if it has none or it is working), ' +
      'or move the task on once an agent turn on it finishes. ' +
      'A project\'s rule replaces any-project rules for the same transition. A rule for a kind fires only for tasks of that kind.'),
    rules.length ? h('table', {}, h('thead', {}, h('tr', {}, ['', 'Rule', 'Project', 'Kind', 'Transition', 'Action', 'Mode', 'When done', ''].map(x => h('th', {}, x)))),
      h('tbody', {}, rules.map(r => h('tr', {},
        h('td', {}, h('input', { type: 'checkbox', checked: r.enabled, onchange: async e => { await api('PUT', `/api/rules/${r.id}`, { ...r, enabled: e.target.checked }); } })),
        h('td', {}, r.name),
        h('td', { class: 'small' }, r.project_id ? projectName(r.project_id) : 'Any'),
        h('td', { class: 'small' }, r.kind || 'Any'),
        h('td', {}, `${laneLabel(r.from_lane_id)} → ${laneLabel(r.to_lane_id)}`),
        h('td', { class: 'small' }, isMoveRule(r) ? `Move to ${laneLabel(r.done_lane_id)} after an agent turn` : r.skill_id ? 'Skill: ' + ((skills.find(sk => sk.id === r.skill_id) || {}).name || '?') : h('code', {}, trunc(r.prompt, 80))),
        h('td', { class: 'muted small' }, r.skill_id || isMoveRule(r) ? '—' : r.mode || 'app default'),
        h('td', { class: 'muted small' }, isMoveRule(r) ? '—' : r.done_lane_id ? '→ ' + laneLabel(r.done_lane_id) : 'stay'),
        h('td', {}, h('button', { class: 'ghost small', onclick: () => ruleDialog(r, b, skills, rerender) }, 'Edit'), ' ',
          h('button', { class: 'danger small', onclick: async () => { await api('DELETE', `/api/rules/${r.id}`); rerender(); } }, 'Delete'))))))
      : h('p', { class: 'muted' }, 'No rules yet.'));

  view.replaceChildren(h('div', { style: 'max-width:900px' }, projects, lanes, workdirsPanel(settings.workdirs, rerender), rulesPanel,
    skillsPanel(skills, b, rerender), sessionPromptPanel(settings), sessionLanePanel(b.all_lanes, settings), archivePanel(b.all_lanes, settings), trelloPanel(b, trello, rerender), fields));
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
              if (confirm(`Delete skill ${sk.name} and the transition rules that run it? Artifacts it made stay.`)) { await api('DELETE', `/api/skills/${sk.id}`); rerender(); }
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
    const lane = select([['', 'In any lane'], ...b.all_lanes.map(l => [l.id, 'In ' + l.name])], a.lane_id ?? '');
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
    h('p', { class: 'muted small' }, `Where agents can run, offered as a dropdown on tasks, task sessions, projects and rules. Unset means the app default (${state.meta.workdir}).`),
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

// The prompt added to every agent turn, telling the agent which app and project it runs for.
function sessionPromptPanel(settings) {
  const text = h('textarea', { class: 'mono', rows: 8 });
  text.value = settings.session_prompt;
  const save = async () => {
    const cur = await api('GET', '/api/settings');
    await api('PUT', '/api/settings', { ...cur, session_prompt: text.value });
    toast('Saved');
  };
  return h('div', { class: 'panel' }, h('h2', {}, 'Session prompt'),
    h('p', { class: 'muted small' }, 'Added to the system prompt of every agent turn, so a session in another repo still knows it runs for this app. ' +
      'Template fields: {{.Project.Name}}, {{.Project.ID}}, {{.Task.ID}}, {{.Task.Title}}, {{.Session.ID}}, {{.Session.Workdir}}. Empty sends nothing.'),
    text, h('div', { class: 'row', style: 'margin-top:6px' }, h('button', { onclick: save }, 'Save')));
}

// The lane a task made for a new agent session (⌥P) starts in.
function sessionLanePanel(lanes, settings) {
  const lane = select([['', `First lane (${lanes[0]?.name ?? ''})`], ...lanes.map(l => [l.id, l.name])], settings.session_lane_id ?? '', {
    onchange: async () => {
      await api('PUT', '/api/settings', { ...await api('GET', '/api/settings'), session_lane_id: lane.value ? Number(lane.value) : null });
      toast('Saved');
    },
  });
  return h('div', { class: 'panel' }, h('h2', {}, 'New sessions'),
    h('p', { class: 'muted small' }, 'A new agent session (⌥P) gets a task of its own; the task starts in this lane.'),
    h('label', {}, 'Lane'), lane);
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

// Trello sync: boards whose open cards become tasks, each with its project, lane and the lane that archives its cards.
function trelloPanel(b, trello, rerender) {
  const id = el => (el.value ? Number(el.value) : null);
  const selects = x => [
    select([['', `First project (${b.projects[0]?.name ?? ''})`], ...b.projects.map(p => [p.id, p.name])], x.project_id ?? ''),
    select([['', `First lane (${b.all_lanes[0]?.name ?? ''})`], ...b.all_lanes.map(l => [l.id, l.name])], x.lane_id ?? ''),
    select([['', 'Done lane: none'], ...b.all_lanes.map(l => [l.id, 'Done lane: ' + l.name])], x.done_lane_id ?? ''),
  ];
  const body = (board, [project, lane, done]) => ({ board: board.value, project_id: id(project), lane_id: id(lane), done_lane_id: id(done) });
  const status = x => !trello.configured ? null
    : x.error ? h('span', { style: 'color:var(--danger)' }, `Last sync failed: ${x.error}`)
      : h('span', { class: 'muted' }, x.last_sync_at ? `Last synced ${fmtTime(x.last_sync_at)}` : 'Not synced yet');
  const row = (board, sels, ...end) => h('div', { class: 'row' }, h('div', { class: 'grow' }, board), sels.map(el => h('div', {}, el)), ...end);

  const boards = trello.boards.map(x => {
    const board = h('input', { value: x.board, title: 'Board URL, short link or id' });
    const sels = selects(x);
    const save = async () => { await api('PUT', `/api/trello/boards/${x.id}`, body(board, sels)); toast('Saved'); };
    [board, ...sels].forEach(el => el.addEventListener('change', save));
    return h('div', { style: 'margin-bottom:10px' },
      row(board, sels, h('button', {
        class: 'danger', title: 'Stop syncing this board; its tasks stay',
        onclick: async () => {
          if (confirm(`Stop syncing ${x.board}? Its tasks stay here.`)) { await api('DELETE', `/api/trello/boards/${x.id}`); rerender(); }
        },
      }, 'Remove')),
      h('div', { class: 'small' }, status(x)));
  });

  const board = h('input', { placeholder: 'https://trello.com/b/…' });
  const sels = selects({});
  const add = async () => {
    if (!board.value.trim()) { toast('Enter a board URL'); return; }
    await api('POST', '/api/trello/boards', body(board, sels));
    rerender();
  };
  board.addEventListener('keydown', e => { if (e.key === 'Enter') add(); });

  return h('div', { class: 'panel' }, h('h2', {}, 'Trello'),
    h('p', { class: 'muted small' }, "Every minute each board's open cards become tasks in its project, with their description and due date, and a card's new due date updates its task. A task moved into its board's done lane gets its card archived on Trello; moved out again, the card comes back."),
    !trello.configured ? h('p', { class: 'small muted' }, 'Off: add trello_key and trello_token to ~/.assistant/config.json and restart.') : null,
    boards.length ? boards : h('p', { class: 'muted' }, 'No boards yet.'),
    row(board, sels, h('button', { onclick: add }, 'Add')),
    h('div', { class: 'row', style: 'margin-top:10px' }, h('div', { class: 'grow' }), h('button', {
      class: 'ghost', disabled: !trello.configured || !trello.boards.length,
      onclick: async () => { await api('POST', '/api/trello/sync'); rerender(); },
    }, 'Sync now')));
}

// A rule with neither prompt nor skill moves the task to its done lane once an agent turn on it finishes.
const isMoveRule = r => !r.skill_id && !r.prompt.trim();

// The rule to save from the dialog's values, or why it can't be saved. Only the chosen action's fields go
// with it: a move rule has no prompt, mode or directory, a skill rule no prompt.
function ruleBody(rule, v) {
  const move = v.action === 'move';
  const skill = v.action && !move ? Number(v.action) : null;
  const session = !move && !skill;
  if (!v.name.trim()) return [null, 'Give the rule a name'];
  if (move && v.moveTo === v.to) return [null, 'Move the task to a lane other than To, the lane the rule watches'];
  return [{
    ...rule, name: v.name, prompt: session ? v.prompt : '', skill_id: skill, mode: session ? v.mode : '',
    workdir: session ? v.workdir : '', project_id: v.project ? Number(v.project) : null, kind: v.kind || '',
    from_lane_id: v.from ? Number(v.from) : null, to_lane_id: Number(v.to),
    done_lane_id: move ? Number(v.moveTo) : v.doneLane ? Number(v.doneLane) : null,
  }, null];
}

// The lanes a move rule can move its task to: any but To, the lane it watches. keep stays chosen if it can,
// else a Review lane, else the first.
function moveToLanes(lanes, to, keep) {
  const opts = lanes.filter(l => String(l.id) !== String(to));
  const pick = opts.find(l => String(l.id) === String(keep)) || opts.find(l => /review/i.test(l.name)) || opts[0];
  return [opts.map(l => [l.id, l.name]), pick ? pick.id : ''];
}

function ruleDialog(rule, b, skills, done) {
  const lanes = b.all_lanes;
  // Automated sessions usually end in review, so new rules default to a Review lane if there is one.
  const review = lanes.find(l => /review/i.test(l.name));
  rule = rule || { name: '', project_id: null, kind: '', from_lane_id: null, to_lane_id: lanes[0].id, prompt: '{{.Context}}\n\n', skill_id: null,
    mode: '', workdir: '', done_lane_id: review ? review.id : null, enabled: true };
  const dlg = document.getElementById('dialog');
  const name = h('input', { value: rule.name, placeholder: 'e.g. Start coding' });
  const project = select([['', 'Any project'], ...b.projects.map(p => [p.id, p.name])], rule.project_id ?? '');
  const kind = select([['', 'Any kind'], ...state.meta.kinds.map(k => [k, k])], rule.kind || '');
  const from = select([['', 'Any lane'], ...lanes.map(l => [l.id, l.name])], rule.from_lane_id ?? '');
  const to = select(lanes.map(l => [l.id, l.name]), rule.to_lane_id);
  const action = select([['', 'Start a new agent session with a prompt'],
    ...skills.map(sk => [sk.id, `Run skill "${sk.name}" in the task's latest session (or a new one)`]),
    ['move', 'Move the task once an agent turn on it finishes']], rule.id && isMoveRule(rule) ? 'move' : rule.skill_id ?? '');
  const moveTo = h('select', {});
  const fillMoveTo = () => {
    const [opts, pick] = moveToLanes(lanes, to.value, moveTo.value || rule.done_lane_id);
    moveTo.replaceChildren(...select(opts, pick).children);
    moveTo.value = pick;
  };
  to.onchange = fillMoveTo;
  fillMoveTo();
  const prompt = h('textarea', { rows: 6 }, rule.prompt);
  const mode = select(modeOptions(true), rule.mode);
  const workdir = localWorkdirSelect(rule, "Task directory, else the project's, else app default");
  const doneLane = doneLaneSelect(lanes, rule.done_lane_id);
  // A skill continues an existing session: prompt, mode and directory are the session's.
  const sessionOpts = h('div', {},
    h('label', {}, 'Prompt template — use a skill like /p-dev:code, and {{.Context}} (the task plus results of tasks it builds on), {{.Task.Title}}, {{.Task.Description}}, {{.Fields.pull_request}}'),
    prompt,
    h('label', {}, 'Agent mode'), mode,
    h('label', {}, 'Working directory'), workdir);
  const moveOpts = h('div', {}, h('label', {}, 'Once an agent turn on it finishes, move it from To to (skipped if the task was moved meanwhile)'), moveTo);
  const doneOpts = h('div', {}, h('label', {}, 'After the agent finishes (skipped if the task was moved meanwhile)'), doneLane);
  const showOpts = () => {
    sessionOpts.classList.toggle('hidden', !!action.value);
    moveOpts.classList.toggle('hidden', action.value !== 'move');
    doneOpts.classList.toggle('hidden', action.value === 'move');
  };
  action.onchange = showOpts;
  showOpts();
  dlg.replaceChildren(h('h2', {}, rule.id ? 'Edit rule' : 'New rule'),
    h('label', {}, 'Name'), name,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'Tasks in project'), project),
      h('div', { class: 'grow' }, h('label', {}, 'Of kind'), kind)),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'From'), from), h('div', { class: 'grow' }, h('label', {}, 'To'), to)),
    h('label', {}, 'Then'), action,
    sessionOpts, moveOpts, doneOpts,
    h('div', { class: 'dialog-actions' },
      cancelButton(dlg),
      h('button', {
        onclick: async () => {
          const [body, err] = ruleBody(rule, {
            name: name.value, project: project.value, kind: kind.value, from: from.value, to: to.value, action: action.value,
            prompt: prompt.value, mode: mode.value, workdir: workdir.value, moveTo: moveTo.value, doneLane: doneLane.value,
          });
          if (err) return toast(err);
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

// opts.buildsOn: the task the new one follows up on; it goes in the same
// project and gets its memories. opts.memory: a memory to link it to.
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
  const memories = memoryPicker(title, desc);
  const buildsOn = buildsOnPicker(from ? [from] : [], from ? from.project_id : state.projectId);
  if (opts.memory) memories.add(opts.memory);
  if (from) (await api('GET', `/api/tasks/${from.id}`)).memories.forEach(memories.add);
  // watch: the task starts out watched (⌘⇧↵).
  const create = async (watch = false) => {
    if (!title.value.trim()) return;
    if (due.value) askNotificationPermission();
    const t = await api('POST', '/api/tasks', {
      title: title.value, description: desc.value, kind: kind.value, lane_id: Number(lane.value), due_at: fromLocalInput(due.value),
      project_id: Number(project.value), context_from: buildsOn.ids(), memories: memories.ids(), watched: watch,
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
  dlg.replaceChildren(...[h('h2', {}, from ? 'New follow-up task' : opts.memory ? `New task on ${opts.memory.name}` : 'New task'),
    h('label', {}, 'Title'), title, h('label', {}, 'Description'), desc,
    buildsOn.el,
    memories.el,
    h('label', {}, 'Project'), project,
    h('div', { class: 'row' }, h('div', { class: 'grow' }, h('label', {}, 'Kind'), kind), h('div', { class: 'grow' }, h('label', {}, 'Lane'), lane)),
    h('label', {}, 'Due (optional) — you get a notification at this time'), due,
    h('div', { class: 'dialog-actions' }, cancelButton(dlg),
      h('button', { class: 'ghost', onclick: () => create(true) }, 'Create & watch', hint('⌘⇧↵')),
      h('button', { onclick: () => create() }, 'Create', hint('⌘↵')))].filter(Boolean));
  dlg.showModal();
  title.focus();
}

// The tasks a new task builds on: initial ones (the task on screen), removable,
// and more picked by search. projectId is the new task's, to name other projects.
function buildsOnPicker(initial, projectId) {
  let picked = initial.map(t => ({ id: t.id, title: t.title }));
  const el = h('div');
  const search = taskSearch({
    placeholder: 'Build on an earlier task — search any project by words or #id…',
    exclude: x => picked.some(p => p.id === x.id),
    note: x => [x.project_id !== projectId ? projectName(x.project_id) : null, x.archived_at ? 'archived' : null],
    onPick: x => { picked.push({ id: x.id, title: x.title }); draw(); search.input.focus(); },
  });
  const draw = () => {
    el.replaceChildren(...[
      h('label', {}, 'Builds on — their agents\' results go into this task\'s agent context'),
      picked.map(t => h('div', { class: 'field-row' }, h('span', { class: 'link-ref' }, `#${t.id} ${t.title}`),
        h('button', { class: 'ghost small', title: 'Don\'t build on this task', onclick: () => { picked = picked.filter(x => x.id !== t.id); draw(); } }, '✕'))),
      search.el,
    ].flat());
  };
  draw();
  return { el, ids: () => picked.map(t => t.id) };
}

// The memories of a new task: picked by search, or suggested from the words
// of its title and description as they are typed, linked with one click.
function memoryPicker(title, desc) {
  let picked = []; // {id, name}
  let suggested = [], seq = 0, timer = 0;
  const el = h('div');
  const add = m => { if (!picked.some(x => x.id === m.id)) picked.push({ id: m.id, name: m.name }); draw(); };
  const search = memorySearch({ exclude: x => picked.some(p => p.id === x.id), onPick: m => { add(m); search.input.focus(); } });
  const draw = () => {
    const free = suggested.filter(m => !picked.some(p => p.id === m.id)).slice(0, 5);
    el.replaceChildren(...[
      h('label', {}, 'Memory — its agents read it and add to it'),
      picked.map(m => h('div', { class: 'field-row' }, h('span', { class: 'link-ref' }, m.name),
        h('button', { class: 'ghost small', title: 'Unlink this memory', onclick: () => { picked = picked.filter(x => x.id !== m.id); draw(); } }, '✕'))),
      free.length ? h('div', { class: 'memory-suggestions' }, h('span', { class: 'muted small' }, 'On this topic:'),
        free.map(m => h('button', {
          class: 'ghost small', title: (m.description ? m.description + ' — ' : '') + 'click to link',
          onclick: () => add(m),
        }, '+ ' + m.name))) : null,
      search.el,
    ].flat().filter(c => c != null));
  };
  const suggest = async () => {
    const q = `${title.value} ${desc.value}`.trim(), n = ++seq;
    const xs = q ? await api('GET', '/api/memories?q=' + encodeURIComponent(q)) : [];
    if (n !== seq) return;
    suggested = xs;
    draw();
  };
  for (const f of [title, desc]) f.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(suggest, 300); });
  draw();
  return { el, add, ids: () => picked.map(m => m.id) };
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

// Opens what the notification is about in the task's dialog, scrolled to its session if it has one.
// A question an agent asked on the task's thread opens the thread instead.
function openNotification(n) {
  if (n.task_id && n.message.startsWith('Question from ')) openTaskDialog(n.task_id, 'activity');
  else if (n.task_id) openTaskDialog(n.task_id, n.session_id || 0);
  else if (n.session_id) location.hash = `#/session/${n.session_id}`;
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
  api('GET', '/api/sync/status').then(st => drawSyncStatus(document.getElementById('sync-status'), st)).catch(() => {});
}

// The header's sync indicator, shown only on a computer syncing with a hub:
// online or offline, changes not pushed yet and the last sync.
function drawSyncStatus(el, st) {
  const client = st.role === 'client';
  el.classList.toggle('hidden', !client);
  if (!client) return;
  el.classList.toggle('offline', !st.online);
  const parts = [st.online ? '● Synced' + (st.synced_at ? ' ' + ago(st.synced_at) : '') : '○ Offline'];
  if (st.pending) parts.push(`${st.pending} pending`);
  el.textContent = parts.join(' · ');
  el.title = [`Sync with ${st.server}`, st.synced_at ? `last sync ${fmtTime(st.synced_at)}` : 'never synced',
    st.pending ? `${st.pending} changes waiting to be pushed` : 'nothing waiting to be pushed', st.error].filter(Boolean).join('\n');
}

async function toggleNotifications() {
  const panel = document.getElementById('notif-panel');
  if (!panel.classList.contains('hidden')) { panel.classList.add('hidden'); return; }
  drawNotifications(panel, await api('GET', '/api/notifications'));
  panel.classList.remove('hidden');
}

function drawNotifications(panel, notes) {
  const head = h('div', { class: 'notif-head' }, h('strong', {}, 'Notifications'),
    notes.length ? h('button', {
      class: 'small ghost', title: 'Take them all off this list',
      onclick: async () => {
        await api('POST', '/api/notifications/archive');
        closeNotifications();
        refreshGlobal();
        toast('Notifications archived');
      },
    }, 'Archive all') : null);
  panel.replaceChildren(head, ...(notes.length ? notes.map(n => h('div', {
    class: 'notif' + (n.read_at ? '' : ' unread'), style: 'cursor:pointer',
    onclick: async () => {
      if (!n.read_at) await api('POST', `/api/notifications/${n.id}/read`);
      panel.classList.add('hidden');
      refreshGlobal();
      openNotification(n);
    },
  }, h('div', { class: 'notif-body' },
    h('div', {}, n.message), h('div', { class: 'muted small' }, (n.task_title ? n.task_title + ' · ' : '') + ago(n.created_at))),
  // Archives just this one; the panel stays open.
  h('button', {
    class: 'ghost small notif-archive', title: 'Archive',
    onclick: async e => {
      e.stopPropagation();
      await api('POST', `/api/notifications/${n.id}/archive`);
      drawNotifications(panel, notes.filter(x => x.id !== n.id));
      refreshGlobal();
    },
  }, '✕')))
    : [h('div', { class: 'notif muted' }, 'Nothing here.')]));
}

// ---------- keyboard shortcuts ----------

// ⌥ (Option/Alt) + key works everywhere, text fields included: it's matched by the physical key
// (e.code), since on a Mac ⌥+letter would type a special character. ⌘ and Ctrl are left to the
// browser and to the text fields' cursor keys; letters that start an accent (⌥E, ⌥I, ⌥N, ⌥U) are
// avoided. "/" and "?" work alone outside a text field.
const shortcuts = [
  { keys: '⌥T', code: 'KeyT', label: 'New task, building on the open one', run: async () => newTaskDialog({ buildsOn: await shownTask() }) },
  { keys: '⌥R', code: 'KeyR', label: 'Quick reminder', run: reminderDialog },
  { keys: '⌥P', code: 'KeyP', label: 'Agent session in a new task, building on the open one', run: async () => sessionDialog({ buildsOn: await shownTask() }) },
  { keys: '⌥C', code: 'KeyC', label: 'Copy the open task\'s link', run: () => copyTaskLink(taskDialog.id) },
  { keys: '⌥W', code: 'KeyW', label: 'Watch or unwatch the open task', run: toggleWatch },
  { keys: '⌥D', code: 'KeyD', label: 'Move the open task to Done', run: moveShownToDone },
  { keys: '⌥M', code: 'KeyM', label: 'Notifications', run: toggleNotifications },
  { keys: '⌥K', code: 'KeyK', label: 'Pick a project', run: projectPicker },
  { keys: '⌥J', code: 'KeyJ', label: 'Watched tasks', run: watchedPicker },
  { keys: '⌥S', code: 'KeyS', label: 'Search every task, archived ones too', run: searchDialog },
  { keys: '⌥[', code: 'BracketLeft', label: 'Previous project', run: () => stepProject(-1) },
  { keys: '⌥]', code: 'BracketRight', label: 'Next project', run: () => stepProject(1) },
  { keys: '⌥B', code: 'KeyB', label: 'Go to Board', run: () => go('#/board') },
  { keys: '⌥L', code: 'KeyL', label: 'Go to List', run: () => go('#/list') },
  { keys: '⌥G', code: 'KeyG', label: 'Go to Sessions', run: () => go('#/sessions') },
  { keys: '⌥O', code: 'KeyO', label: 'Go to Memory', run: () => go('#/memory') },
  { keys: '⌥A', code: 'KeyA', label: 'Go to Archive', run: () => go('#/archive') },
  { keys: '⌥,', code: 'Comma', label: 'Go to Settings', run: () => go('#/settings') },
  { keys: '⌥H', code: 'KeyH', label: 'Show or hide the key hints next to buttons and links', run: toggleHints },
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

// The task open in the dialog, or undefined.
async function shownTask() {
  const id = taskDialog.id;
  return id ? (await api('GET', `/api/tasks/${id}`)).task : undefined;
}

// The link that opens a task, e.g. http://localhost:6767/#/task/7.
function taskLink(origin, id) {
  return `${origin}/#/task/${id}`;
}

// The task and session a task link points at: '#/task/7/s/3' → { id: 7, session: 3 }; null for other links.
function taskHash(hash) {
  const m = /^#\/task\/(\d+)(?:\/s\/(\d+))?$/.exec(hash || '');
  return m ? { id: Number(m[1]), session: Number(m[2]) || 0 } : null;
}

function copyTaskLink(id) {
  if (!id) { toast('Open a task to copy its link'); return; }
  navigator.clipboard.writeText(taskLink(location.origin, id)).then(() => toast('Task link copied'));
}

// ⌥W: the open task's Watch button.
function toggleWatch() {
  const btn = document.querySelector('.watch-toggle');
  if (btn) btn.click(); else toast('Open a task to watch it');
}

// ⌥D: the open task's Done button.
function moveShownToDone() {
  const btn = document.querySelector('.done-toggle');
  if (!btn) toast('Open a task to move it to Done');
  else if (btn.classList.contains('hidden')) toast('The task is already done');
  else btn.click();
}

// Where a project picked with ⌥K opens: the list when on the list, else the board.
function pickedProjectHash(hash) {
  return /^#\/list\b/.test(hash) ? '#/list' : '#/board';
}

// Makes ⌘↵ (Ctrl↵ elsewhere) in any of the fields call submit, e.g. from a dialog's textarea;
// submit gets whether ⇧ was held too (⌘⇧↵), which the new task and session dialogs take as "and watch it".
function submitOnCmdEnter(fields, submit) {
  for (const f of fields) {
    f.addEventListener('keydown', e => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !e.repeat) { e.preventDefault(); submit(e.shiftKey); }
    });
  }
}

function onShortcut(e) {
  if (document.getElementById('dialog').open) return;
  const inField = !!e.target.closest('input, textarea, select, [contenteditable]');
  if (e.key === 'Escape' && closeNotifications()) { e.preventDefault(); return; }
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

// Hides the notifications panel; true if it was open.
function closeNotifications() {
  const panel = document.getElementById('notif-panel');
  if (panel.classList.contains('hidden')) return false;
  panel.classList.add('hidden');
  return true;
}

function stepProject(by) {
  const ps = state.board?.projects;
  if (!ps || ps.length < 2) return;
  const i = ps.findIndex(p => p.id === state.projectId);
  const next = ps[(i + by + ps.length) % ps.length];
  switchProject(next.id);
  toast(next.name);
}

// How the ⌥K picker names a project: its watched tasks in brackets.
function pickerLabel(p) {
  return `${p.name} (${p.watched})`;
}

// ⌥K: a list of the projects; ↑/↓ move, Enter (or a click) opens the chosen one.
async function projectPicker() {
  await loadBoard(); // fresh watched counts
  const ps = state.board.projects;
  if (!ps.length) return;
  const hash = pickedProjectHash(location.hash);
  pickerDialog({
    title: 'Open project', items: ps, start: ps.findIndex(p => p.id === state.projectId), label: p => [pickerLabel(p)],
    help: `↑/↓ to choose, Enter opens its ${hash === '#/list' ? 'list' : 'board'}, Esc closes.`,
    onPick: p => { state.projectId = p.id; remember('project', p.id); go(hash); },
  });
}

// How the ⌥J picker names a watched task: its id and title, then its project and lane.
function watchedLabel(t, projects, lanes) {
  const where = [projects.find(p => p.id === t.project_id)?.name, lanes.find(l => l.id === t.lane_id)?.name].filter(Boolean);
  return [`#${t.id} ${t.title}`, where.length ? ' · ' + where.join(' · ') : ''];
}

// The dot before a watched task in the ⌥J picker: green while an agent works on it, orange while one
// waits for the user, grey otherwise.
function agentDot(st) {
  const [cls, title] = st?.status === 'running' ? ['running', 'An agent works on it']
    : st?.status === 'needs_you' ? ['needs-you', 'The agent ' + st.reason] : ['idle', 'No agent at work'];
  return h('span', { class: 'agent-dot ' + cls, title });
}

// ⌥J: the watched tasks of every project; ↑/↓ move, Enter (or a click) opens the chosen one's dialog.
async function watchedPicker() {
  const [tasks] = await Promise.all([api('GET', '/api/watched'), state.board ? null : loadBoard()]);
  if (!tasks.length) { toast('No watched tasks — ⌥W watches the open task'); return; }
  pickerDialog({
    title: 'Watched tasks', items: tasks, start: tasks.findIndex(t => t.id === taskDialog.id),
    label: t => {
      const [name, where] = watchedLabel(t, state.board.projects, state.board.all_lanes);
      return [agentDot(t.agent_status), name, where ? h('span', { class: 'small task-search-note' }, where) : null];
    },
    help: '↑/↓ to choose, Enter opens the task, Esc closes.',
    onPick: t => openTaskDialog(t.id),
  });
}

// How the ⌥S search notes a found task: its project, then its lane, or "archived".
function searchNote(t, projects, lanes) {
  return [projects.find(p => p.id === t.project_id)?.name,
    t.archived_at ? 'archived' : lanes.find(l => l.id === t.lane_id)?.name];
}

// ⌥S: a search over every task of every project, archived ones included, by words or #id; the
// empty box lists the latest. Enter (or a click) opens the chosen one's dialog.
async function searchDialog() {
  if (!state.board) await loadBoard();
  const dlg = document.getElementById('dialog');
  const search = taskSearch({
    placeholder: 'Search tasks by words or #id…', exclude: () => false,
    note: t => searchNote(t, state.board.projects, state.board.all_lanes),
    onPick: t => { dlg.close(); openTaskDialog(t.id); },
  });
  dlg.replaceChildren(h('h2', {}, 'Search tasks'), h('div', { class: 'search-dialog' }, search.el),
    h('p', { class: 'muted small' }, 'Every project, archived tasks too. ↑/↓ to choose, Enter opens the task, Esc closes.'));
  dlg.showModal();
  search.input.focus();
}

// A dialog listing items (label(x) draws one) with the one at start chosen; ↑/↓ move, Enter (or a
// click) closes it and calls onPick with the chosen item.
function pickerDialog({ title, items: xs, start, label, help, onPick }) {
  const dlg = document.getElementById('dialog');
  let i = Math.max(0, start);
  const pick = x => { dlg.close(); onPick(x); };
  const items = xs.map((x, j) => h('li', { onclick: () => pick(x), onmousemove: () => { i = j; draw(); } }, ...label(x)));
  function draw() {
    items.forEach((el, j) => el.classList.toggle('selected', j === i));
    items[i].scrollIntoView({ block: 'nearest' });
  }
  const list = h('ul', {
    class: 'picker', tabindex: 0, onkeydown: e => {
      const by = { ArrowDown: 1, ArrowUp: -1 }[e.key];
      if (by) { e.preventDefault(); i = (i + by + xs.length) % xs.length; draw(); }
      else if (e.key === 'Enter') { e.preventDefault(); pick(xs[i]); }
    },
  }, items);
  dlg.replaceChildren(h('h2', {}, title), list, h('p', { class: 'muted small' }, help));
  dlg.showModal();
  list.focus();
  draw();
}

function shortcutsDialog() {
  const dlg = document.getElementById('dialog');
  dlg.replaceChildren(h('h2', {}, 'Keyboard shortcuts'),
    h('table', { class: 'shortcuts' }, shortcuts.map(s => h('tr', {},
      h('td', {}, s.keys.split(' ').map(k => h('kbd', {}, k))), h('td', {}, s.label)))),
    h('p', { class: 'muted small' }, '⌘/Ctrl+Enter sends a prompt or reply; with ⇧ too, a new task or agent session starts out watched. Esc closes a dialog or the notifications. ',
      'Greyed keys next to buttons and links show their shortcut; ⌥H shows or hides them.'),
    h('div', { class: 'dialog-actions' }, h('button', { onclick: () => dlg.close() }, 'Close', hint('Esc'))));
  dlg.showModal();
}

function cancelButton(dlg) {
  return h('button', { class: 'ghost', onclick: () => dlg.close() }, 'Cancel', hint('Esc'));
}

// Key hints are hidden until ⌥H shows them; the choice survives reloads.
function showHints(on) {
  document.body.classList.toggle('show-hints', on);
}

function toggleHints() {
  const on = !flag('ui.hints');
  setFlag('ui.hints', on);
  showHints(on);
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
  showHints(flag('ui.hints'));
  document.getElementById('notif-btn').addEventListener('click', toggleNotifications);
  document.getElementById('project-select').addEventListener('change', e => switchProject(Number(e.target.value)));
  document.addEventListener('click', openTaskLink);
  window.addEventListener('hashchange', route);
  route();
  refreshGlobal();
  setInterval(refreshGlobal, 3000);
})();

'use strict';

// Markdown → DOM for agent messages: the subset agents write — headings, paragraphs, lists (task lists
// too), quotes, fenced code, tables, rules; inline code, bold, italic, strikethrough, links. Everything is
// built with h(), never innerHTML, so message text can't inject markup; links go only to http(s), mailto
// and in-app hashes. Unfinished input (a streaming message) renders as far as it goes.

const mdFence = /^ {0,3}(`{3,}|~{3,})[ \t]*([^`\s]*)/;
const mdHeading = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/;
const mdRule = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const mdQuote = /^ {0,3}> ?/;
const mdItem = /^( *)([-*+]|\d{1,9}[.)])(?:[ \t]+|$)/;
const mdTableSep = /^ {0,3}\|?[ \t]*:?-+:?[ \t]*(\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/;

function markdown(text) {
  return mdBlocks(String(text || '').replace(/\r\n?/g, '\n').split('\n'));
}

const mdIndent = line => line.match(/^ */)[0].length;
const mdBlank = line => !line.trim();
const mdIsTable = (lines, i) => lines[i].includes('|') && i + 1 < lines.length && lines[i + 1].includes('|') && mdTableSep.test(lines[i + 1]);

// A line that starts a block of its own, so it ends a paragraph.
function mdStartsBlock(lines, i) {
  const l = lines[i];
  return mdFence.test(l) || mdHeading.test(l) || mdRule.test(l) || mdQuote.test(l) || mdItem.test(l) || mdIsTable(lines, i);
}

function mdBlocks(lines) {
  const out = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    let m;
    if (mdBlank(line)) {
      i++;
    } else if ((m = line.match(mdFence))) {
      const close = new RegExp('^ {0,3}' + m[1][0] + '{' + m[1].length + ',}[ \\t]*$');
      const body = [];
      for (i++; i < lines.length && !close.test(lines[i]); i++) body.push(lines[i]);
      i++;
      out.push(h('pre', {}, h('code', { class: m[2] ? 'lang-' + m[2] : null }, body.join('\n'))));
    } else if ((m = line.match(mdHeading))) {
      out.push(h('h' + m[1].length, {}, mdInline(m[2] || '')));
      i++;
    } else if (mdRule.test(line)) {
      out.push(h('hr', {}));
      i++;
    } else if (mdQuote.test(line)) {
      const body = [];
      for (; i < lines.length && !mdBlank(lines[i]); i++) {
        if (mdQuote.test(lines[i])) body.push(lines[i].replace(mdQuote, ''));
        else if (mdStartsBlock(lines, i)) break;
        else body.push(lines[i]); // lazy continuation
      }
      out.push(h('blockquote', {}, mdBlocks(body)));
    } else if (mdItem.test(line)) {
      i = mdList(lines, i, out);
    } else if (mdIsTable(lines, i)) {
      i = mdTable(lines, i, out);
    } else {
      const para = [line.trim()];
      for (i++; i < lines.length && !mdBlank(lines[i]) && !mdStartsBlock(lines, i); i++) para.push(lines[i].trim());
      out.push(h('p', {}, mdInline(para.join('\n'))));
    }
  }
  return out;
}

// A list of items with the same kind of marker, starting at lines[i]; returns the index after it.
// A line indented past the item's marker belongs to the item, so nesting works with 2 or 4 spaces.
function mdList(lines, i, out) {
  const first = lines[i].match(mdItem);
  const ordered = /\d/.test(first[2]);
  const delim = first[2].slice(-1);
  const sameKind = m => m && (ordered ? /\d/.test(m[2]) && m[2].endsWith(delim) : m[2] === first[2]) && m[1].length <= first[1].length + 1;
  const items = [];
  while (i < lines.length) {
    const m = lines[i].match(mdItem);
    if (!sameKind(m)) break;
    const markerIndent = m[1].length;
    const contentIndent = m[0].length;
    const body = [lines[i].slice(contentIndent)];
    for (i++; i < lines.length; i++) {
      const l = lines[i];
      if (mdBlank(l)) {
        // A blank line stays in the item only if indented content follows it.
        let j = i;
        while (j < lines.length && mdBlank(lines[j])) j++;
        if (j < lines.length && mdIndent(lines[j]) > markerIndent && !sameKind(lines[j].match(mdItem))) { body.push(''); continue; }
        break;
      }
      if (mdIndent(l) > markerIndent && !sameKind(l.match(mdItem))) body.push(l.slice(Math.min(mdIndent(l), contentIndent)));
      else if (!mdBlank(lines[i - 1]) && !mdStartsBlock(lines, i)) body.push(l); // lazy continuation
      else break;
    }
    items.push(mdListItem(body));
    while (i < lines.length && mdBlank(lines[i]) && sameKind((lines.slice(i).find(l => !mdBlank(l)) || '').match(mdItem))) i++;
  }
  const start = ordered ? parseInt(first[2], 10) : 1;
  out.push(h(ordered ? 'ol' : 'ul', { start: start !== 1 ? start : null }, items));
  return i;
}

function mdListItem(body) {
  const task = body[0].match(/^\[([ xX])\][ \t]+/);
  if (task) body[0] = body[0].slice(task[0].length);
  const blocks = mdBlocks(body);
  if (task) {
    const box = h('input', { type: 'checkbox', disabled: true, checked: task[1] !== ' ' });
    if (blocks[0] && blocks[0].tagName === 'P') blocks[0].prepend(box);
    else blocks.unshift(box);
  }
  return h('li', { class: task ? 'task' : null }, blocks);
}

function mdTable(lines, i, out) {
  const align = mdCells(lines[i + 1]).map(c => {
    const l = c.startsWith(':'), r = c.endsWith(':');
    return l && r ? 'center' : r ? 'right' : l ? 'left' : null;
  });
  const row = (line, tag) => h('tr', {}, align.map((a, k) =>
    h(tag, { style: a ? 'text-align:' + a : null }, mdInline(mdCells(line)[k] || ''))));
  const head = row(lines[i], 'th');
  const rows = [];
  for (i += 2; i < lines.length && !mdBlank(lines[i]) && lines[i].includes('|'); i++) rows.push(row(lines[i], 'td'));
  out.push(h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, head), rows.length ? h('tbody', {}, rows) : null)));
  return i;
}

// The cells of a table row: split on unescaped pipes, outer pipes optional.
function mdCells(line) {
  const cells = line.trim().replace(/^\|/, '').replace(/(^|[^\\])\|$/, '$1').split(/(?<!\\)\|/);
  return cells.map(c => c.trim().replace(/\\\|/g, '|'));
}

const mdPunct = /[!-/:-@[-`{-~]/;
const mdWord = /[\p{L}\p{N}]/u;

function mdInline(s) {
  const out = [];
  let buf = '';
  const push = node => { if (buf) out.push(buf); buf = ''; out.push(node); };
  let i = 0;
  while (i < s.length) {
    const c = s[i];
    if (c === '\\' && mdPunct.test(s[i + 1] || '')) {
      buf += s[i + 1];
      i += 2;
      continue;
    }
    if (c === '`') {
      const n = s.slice(i).match(/^`+/)[0].length;
      const end = mdCodeEnd(s, i + n, n);
      if (end < 0) { buf += '`'.repeat(n); i += n; continue; }
      let code = s.slice(i + n, end).replace(/\n/g, ' ');
      if (/^ .*[^ ].* $/.test(code)) code = code.slice(1, -1);
      push(h('code', {}, code));
      i = end + n;
      continue;
    }
    const link = (c === '[' || (c === '!' && s[i + 1] === '[')) && mdLink(s, c === '!' ? i + 1 : i);
    if (link) {
      push(mdAnchor(link.url, c === '!' && !link.text ? link.url : mdInline(link.text)));
      i = link.end;
      continue;
    }
    if (c === '<') {
      const m = s.slice(i).match(/^<((?:https?:\/\/|mailto:)[^\s<>]+)>/i);
      if (m) { push(mdAnchor(m[1], m[1])); i += m[0].length; continue; }
    }
    if ((c === 'h' || c === 'H') && !mdWord.test(s[i - 1] || '')) {
      const m = s.slice(i).match(/^https?:\/\/[^\s<]+/i);
      if (m) {
        let url = m[0].replace(/[.,;:!?'"*_~]+$/, '');
        while (url.endsWith(')') && url.split('(').length < url.split(')').length) url = url.slice(0, -1);
        push(mdAnchor(url, url));
        i += url.length;
        continue;
      }
    }
    const em = (c === '*' || c === '_' || (c === '~' && s[i + 1] === '~')) && mdEmphasis(s, i);
    if (em) {
      push(h(em.tag, {}, mdInline(s.slice(em.from, em.to))));
      i = em.end;
      continue;
    }
    buf += c;
    i++;
  }
  if (buf) out.push(buf);
  return out;
}

// Where the code span opened by n backticks closes: the next run of exactly n backticks.
function mdCodeEnd(s, from, n) {
  const re = /`+/g;
  re.lastIndex = from;
  for (let m; (m = re.exec(s));) if (m[0].length === n) return m.index;
  return -1;
}

// **strong**, __strong__, *em*, _em_ or ~~strike~~ opening at s[i], or null.
// Underscores inside words (snake_case) stay text.
function mdEmphasis(s, i) {
  const c = s[i];
  const double = s[i + 1] === c;
  if (c === '~' && !double) return null;
  const d = double ? c + c : c;
  if (/\s/.test(s[i + d.length] || ' ')) return null;
  if (c === '_' && mdWord.test(s[i - 1] || '')) return null;
  for (let j = i + d.length + 1; j < s.length; j++) {
    if (s[j] === '`') { // emphasis never closes inside a code span
      const n = s.slice(j).match(/^`+/)[0].length;
      const end = mdCodeEnd(s, j + n, n);
      if (end > 0) j = end + n - 1;
      continue;
    }
    if (s[j] !== c) continue;
    let run = 1;
    while (s[j + run] === c) run++;
    if (/\s/.test(s[j - 1]) || run < d.length || (!double && run === 2)) { j += run - 1; continue; }
    if (c === '_' && mdWord.test(s[j + run] || '')) { j += run - 1; continue; }
    const at = j + run - d.length; // the run's last delimiters close it: ***both*** is <em><strong>
    return { tag: c === '~' ? 'del' : double ? 'strong' : 'em', from: i + d.length, to: at, end: at + d.length };
  }
  return null;
}

// [text](url "title") with s[i] === '['; the url may sit in <>; returns { text, url, end } or null.
function mdLink(s, i) {
  let depth = 0, j = i;
  for (; j < s.length; j++) {
    if (s[j] === '\\') { j++; continue; }
    if (s[j] === '[') depth++;
    else if (s[j] === ']' && --depth === 0) break;
  }
  if (j >= s.length || s[j + 1] !== '(') return null;
  const m = s.slice(j + 2).match(/^[ \t]*(?:<([^<>\n]*)>|((?:[^\s()]|\([^\s()]*\))*))(?:[ \t]+("[^"]*"|'[^']*'))?[ \t]*\)/);
  if (!m) return null;
  return { text: s.slice(i + 1, j), url: m[1] !== undefined ? m[1] : m[2], end: j + 2 + m[0].length };
}

function mdAnchor(url, label) {
  if (/^(https?:|mailto:)/i.test(url)) return h('a', { href: url, target: '_blank', rel: 'noopener noreferrer' }, label);
  if (url.startsWith('#')) return h('a', { href: url }, label);
  return h('span', { title: url }, label); // a file path or an unsafe scheme: show the text only
}

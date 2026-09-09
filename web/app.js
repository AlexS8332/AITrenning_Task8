'use strict';

// Состояние страницы. Диалоги живут на сервере и на диске; здесь их список,
// открытый диалог целиком (сообщения и ходы с журналами) и ход, чей журнал
// показан справа. Пока ход идёт, его журнал приходит потоком событий, после
// завершения читается из диалога.
const state = {
  agents: [],
  serverStarted: null,
  conversations: [],
  current: null,      // диалог целиком (ответ GET /api/conversations/{id})
  draftAgent: null,   // тип агента нового диалога, пока не отправлено первое сообщение
  source: null,       // EventSource идущего хода
  live: null,         // { view, events } идущего хода
  selectedTurn: null, // идентификатор хода, чей журнал показан
  filter: { tools: true, llm: true }
};

const el = {};
for (const id of [
  'history-dir', 'server-started', 'new-button', 'conversations', 'conversations-empty',
  'thread-title', 'thread-meta', 'file-button', 'delete-button', 'thread', 'thread-empty',
  'composer', 'text', 'send-button', 'error', 'start-button',
  'growth-box', 'growth-meta', 'growth-body',
  'new-dialog', 'new-close', 'new-create', 'agent-choice',
  'log-note', 'turn-status', 'show-tools', 'show-llm', 'log', 'totals',
  'prompt-box', 'prompt-agent', 'prompt-meta', 'prompt-body',
  'file-dialog', 'file-close', 'file-path', 'file-body'
]) {
  el[id.replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = document.getElementById(id);
}

const KIND_LABEL = {
  'agent.start': 'агент',
  'agent.done': 'готово',
  'agent.error': 'ошибка',
  'llm.request': 'модель ←',
  'llm.response': 'модель →',
  'tool.call': 'инструмент ←',
  'tool.result': 'инструмент →',
  'tool.error': 'инструмент ✕',
  'note': 'заметка',
  'prompt': 'промпт'
};

async function init() {
  if (window.marked) {
    marked.use({
      gfm: true,
      breaks: true,
      renderer: {
        // Сырой HTML из ответа модели показываем как текст.
        html(token) {
          const raw = typeof token === 'string' ? token : (token.raw ?? token.text ?? '');
          return escapeHTML(raw);
        }
      }
    });
  }

  try {
    const data = await getJSON('/api/agents');
    state.agents = data.agents || [];
    state.serverStarted = data.serverStarted ? new Date(data.serverStarted) : null;
    el.historyDir.textContent = data.historyDir || '—';
    el.serverStarted.textContent = state.serverStarted ? formatTime(state.serverStarted) : '—';
  } catch (err) {
    showError('Не удалось получить список агентов: ' + err.message);
  }

  renderAgentChoice();
  el.newButton.addEventListener('click', openPicker);
  el.startButton.addEventListener('click', openPicker);
  el.newClose.addEventListener('click', () => el.newDialog.close());
  el.newCreate.addEventListener('click', () => {
    const chosen = el.agentChoice.querySelector('input:checked');
    el.newDialog.close();
    startDraft(chosen ? chosen.value : (state.agents[0] && state.agents[0].key));
  });
  el.composer.addEventListener('submit', (e) => { e.preventDefault(); send(); });
  el.text.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.ctrlKey) { e.preventDefault(); send(); }
  });
  el.showTools.addEventListener('change', () => { state.filter.tools = el.showTools.checked; applyFilter(); });
  el.showLlm.addEventListener('change', () => { state.filter.llm = el.showLlm.checked; applyFilter(); });
  el.fileButton.addEventListener('click', showFile);
  el.fileClose.addEventListener('click', () => el.fileDialog.close());
  el.deleteButton.addEventListener('click', () => {
    if (state.current) deleteConversation(state.current.id, state.current.title);
  });

  await refreshList();

  // Диалог переживает и перезагрузку страницы, и перезапуск сервера:
  // идентификатор в адресе, а сам диалог на диске.
  const id = new URLSearchParams(location.search).get('c');
  if (id) {
    await open(id);
  } else {
    openNew();
  }
}

/* ---------- Создание диалога ---------- */

// renderAgentChoice заполняет окно создания диалога типами агентов.
function renderAgentChoice() {
  el.agentChoice.innerHTML = '';
  state.agents.forEach((a, i) => {
    const label = document.createElement('label');
    const input = document.createElement('input');
    input.type = 'radio';
    input.name = 'new-agent';
    input.value = a.key;
    input.checked = i === 0;
    label.appendChild(input);
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = a.title;
    label.appendChild(name);
    const desc = document.createElement('span');
    desc.className = 'desc';
    desc.textContent = a.description;
    label.appendChild(desc);
    el.agentChoice.appendChild(label);
  });
}

function openPicker() {
  if (!state.agents.length) { showError('Список агентов не загружен.'); return; }
  el.newDialog.showModal();
}

// startDraft открывает пустой диалог с выбранным агентом. Сам диалог
// появится на сервере и на диске с первым сообщением.
function startDraft(agentKey) {
  history.replaceState(null, '', location.pathname);
  openNew();
  const info = state.agents.find((a) => a.key === agentKey);
  if (!info) return;
  state.draftAgent = agentKey;
  el.threadTitle.textContent = 'Новый диалог';
  el.threadMeta.textContent = info.title + ' · ' + info.description;
  el.thread.innerHTML = '';
  const p = document.createElement('p');
  p.className = 'empty';
  p.textContent = 'Напишите первое сообщение. Например: представьтесь и спросите о животном, потом задайте уточняющий вопрос, перезапустите сервер и продолжите.';
  el.thread.appendChild(p);
  el.composer.hidden = false;
  el.text.focus();
}

/* ---------- Список диалогов ---------- */

async function refreshList() {
  try {
    const data = await getJSON('/api/conversations');
    state.conversations = data.conversations || [];
  } catch (err) {
    showError('Не удалось получить список диалогов: ' + err.message);
    return;
  }
  renderList();
}

function renderList() {
  el.conversations.innerHTML = '';
  el.conversationsEmpty.hidden = state.conversations.length > 0;
  for (const c of state.conversations) {
    const li = document.createElement('li');
    if (state.current && state.current.id === c.id) li.classList.add('active');

    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'item';
    b.addEventListener('click', () => {
      history.replaceState(null, '', '?c=' + encodeURIComponent(c.id));
      open(c.id);
    });

    const title = document.createElement('span');
    title.className = 'title';
    title.textContent = c.title || 'без названия';
    b.appendChild(title);

    const meta = document.createElement('span');
    meta.className = 'meta';
    const agent = document.createElement('span');
    agent.className = 'agent';
    agent.dataset.agent = c.agentKey;
    agent.textContent = c.agentKey;
    meta.appendChild(agent);
    if (c.running) {
      const dot = document.createElement('span');
      dot.className = 'dot running';
      meta.appendChild(dot);
    }
    meta.appendChild(document.createTextNode(
      plural(c.turns, 'ход', 'хода', 'ходов') + ' · ' + plural(c.messages, 'сообщение', 'сообщения', 'сообщений') +
      ' · ' + formatTime(new Date(c.updated))));
    b.appendChild(meta);

    li.appendChild(b);
    li.appendChild(removeButton(c));
    el.conversations.appendChild(li);
  }
}

// removeButton — крестик в строке списка: удаляет диалог, не открывая его.
// Пока в диалоге идёт ход, удалять нельзя — сервер ответит 409, поэтому
// кнопка выключена до конца хода.
function removeButton(c) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'remove';
  b.textContent = '×';
  b.disabled = Boolean(c.running);
  b.title = c.running ? 'Идёт ход: дождитесь ответа' : 'Удалить диалог';
  b.setAttribute('aria-label', 'Удалить диалог «' + (c.title || c.id) + '»');
  b.addEventListener('click', () => deleteConversation(c.id, c.title));
  return b;
}

/* ---------- Открытие диалога ---------- */

// openNew — состояние без выбранного диалога: подсказка и кнопка создания.
function openNew() {
  detach();
  state.current = null;
  state.draftAgent = null;
  state.live = null;
  state.selectedTurn = null;
  el.threadTitle.textContent = 'Диалог не выбран';
  el.threadMeta.textContent = '';
  el.fileButton.hidden = true;
  el.deleteButton.hidden = true;
  el.growthBox.hidden = true;
  el.composer.hidden = true;
  el.thread.innerHTML = '';
  el.thread.appendChild(el.threadEmpty);
  el.threadEmpty.hidden = false;
  el.sendButton.disabled = false;
  renderList();
  renderJournal(null);
}

async function open(id) {
  let d;
  try {
    d = await getJSON('/api/conversations/' + encodeURIComponent(id));
  } catch (err) {
    showError('Диалог не открылся: ' + err.message);
    openNew();
    return;
  }
  showError('');
  state.current = d;
  state.draftAgent = null;
  el.composer.hidden = false;
  el.fileButton.hidden = false;
  el.deleteButton.hidden = false;
  renderList();
  renderThread();

  if (d.active) {
    // Ход идёт: подключаемся к потоку, если ещё не подключены к нему.
    if (!state.live || state.live.view.id !== d.active.id) attach(d.active.id);
    state.selectedTurn = d.active.id;
    renderJournal(currentTurn());
  } else {
    detach();
    state.live = null;
    if (!state.selectedTurn || !d.turns.some((t) => t.id === state.selectedTurn)) {
      state.selectedTurn = d.turns.length ? d.turns[d.turns.length - 1].id : null;
    }
    renderJournal(currentTurn());
    el.sendButton.disabled = false;
  }
}

function currentTurn() {
  if (state.live && state.live.view.id === state.selectedTurn) {
    return { live: true, id: state.live.view.id, status: state.live.view.status, agentKey: state.live.view.agentKey,
      started: state.live.view.started, totals: state.live.view.totals, events: state.live.events,
      history: state.live.view.history, saveError: state.live.view.saveError, context: state.live.view.context };
  }
  if (!state.current) return null;
  const t = state.current.turns.find((x) => x.id === state.selectedTurn);
  if (!t) return null;
  return { live: false, id: t.id, status: t.status, agentKey: state.current.agentKey, started: t.started,
    totals: t.totals, events: t.events || [], saveError: '', context: t.context };
}

/* ---------- Отправка ---------- */

async function send() {
  const text = el.text.value.trim();
  if (!text) { showError('Введите сообщение.'); return; }
  showError('');
  el.sendButton.disabled = true;

  try {
    let view;
    if (state.current) {
      view = await postJSON('/api/conversations/' + encodeURIComponent(state.current.id) + '/turns', { text });
    } else {
      if (!state.draftAgent) { el.sendButton.disabled = false; openPicker(); return; }
      view = await postJSON('/api/conversations', { agent: state.draftAgent, text });
      history.replaceState(null, '', '?c=' + encodeURIComponent(view.conversationId));
    }
    el.text.value = '';
    state.selectedTurn = view.id;
    await open(view.conversationId);
    await refreshList();
  } catch (err) {
    showError('Не удалось отправить: ' + err.message);
    el.sendButton.disabled = false;
  }
}

/* ---------- Поток событий идущего хода ---------- */

function attach(turnId) {
  detach();
  state.live = { view: { id: turnId, status: 'running', events: 0, totals: {} }, events: [] };

  const source = new EventSource('/api/turns/' + encodeURIComponent(turnId) + '/events');
  state.source = source;

  source.addEventListener('snapshot', (e) => {
    const snap = JSON.parse(e.data);
    state.live = { view: snap.view, events: snap.events || [] };
    if (state.selectedTurn === turnId) renderJournal(currentTurn());
    renderThread();
  });
  source.addEventListener('state', (e) => {
    if (!state.live) return;
    state.live.view = JSON.parse(e.data);
    if (state.selectedTurn === turnId) renderJournal(currentTurn(), true);
    updateRunningBubble();
  });
  source.addEventListener('log', (e) => {
    if (!state.live) return;
    const ev = JSON.parse(e.data);
    state.live.events.push(ev);
    if (state.selectedTurn === turnId) appendEvent(ev);
    updateRunningBubble();
  });
  source.addEventListener('done', async () => {
    detach();
    const id = state.current ? state.current.id : (state.live && state.live.view.conversationId);
    state.live = null;
    // Ход записан в диалог: перечитываем его с сервера, там уже новые
    // сообщения и журнал хода.
    if (id) await open(id);
    await refreshList();
  });
  source.onerror = () => {
    if (source.readyState === EventSource.CLOSED) {
      el.sendButton.disabled = false;
      showError('Поток событий закрылся. Возможно, сервер перезапускали: обновите страницу, диалог на диске.');
    }
  };
}

function detach() {
  if (state.source) {
    state.source.close();
    state.source = null;
  }
}

/* ---------- Лента сообщений ---------- */

function renderThread() {
  const d = state.current;
  const box = el.thread;
  box.innerHTML = '';
  if (!d) return;

  const info = state.agents.find((a) => a.key === d.agentKey);
  el.threadTitle.textContent = d.title || 'Новый диалог';
  el.threadMeta.textContent = (info ? info.title : d.agentKey) + ' · ' + d.model + ' · ' +
    plural(d.messages.length, 'сообщение', 'сообщения', 'сообщений') + ' в истории, ≈' + kilo(d.runes) + ' символов';

  renderGrowth(d);

  let restartShown = false;
  for (const t of d.turns) {
    // Ход раньше старта сервера сделан в прошлом запуске: между ним и
    // первым ходом этого запуска был перезапуск.
    if (state.serverStarted && !restartShown && new Date(t.started) > state.serverStarted && t !== d.turns[0]) {
      box.appendChild(restartMarker());
      restartShown = true;
    }
    box.appendChild(userMessage(t.user, t.started));
    box.appendChild(agentMessage(t, d));
  }
  if (d.active) {
    if (state.serverStarted && !restartShown && d.turns.length && new Date(d.turns[d.turns.length - 1].started) < state.serverStarted) {
      box.appendChild(restartMarker());
    }
    box.appendChild(userMessage(d.active.user, d.active.started));
    const m = document.createElement('div');
    m.className = 'msg agent';
    m.id = 'running-bubble';
    const bubble = document.createElement('div');
    bubble.className = 'bubble running';
    bubble.textContent = 'агент работает…';
    m.appendChild(bubble);
    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.appendChild(who(d.agentKey));
    m.appendChild(meta);
    box.appendChild(m);
    updateRunningBubble();
  } else if (state.serverStarted && d.turns.length && new Date(d.turns[d.turns.length - 1].started) < state.serverStarted) {
    // Все ходы сделаны до старта этого сервера: диалог поднят с диска и
    // ждёт продолжения.
    box.appendChild(restartMarker('сервер перезапущен после этого хода, история поднята с диска'));
  }
  box.scrollTop = box.scrollHeight;
}

function restartMarker(text) {
  const div = document.createElement('div');
  div.className = 'restart';
  div.textContent = text || 'сервер перезапущен, история поднята с диска';
  return div;
}

function userMessage(text, started) {
  const m = document.createElement('div');
  m.className = 'msg user';
  const bubble = document.createElement('div');
  bubble.className = 'bubble';
  bubble.textContent = text;
  m.appendChild(bubble);
  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.textContent = formatTime(new Date(started));
  m.appendChild(meta);
  return m;
}

function agentMessage(t, d) {
  const m = document.createElement('div');
  m.className = 'msg agent';
  const bubble = document.createElement('div');
  if (t.status === 'failed') {
    bubble.className = 'bubble failed';
    bubble.textContent = 'Ход завершился ошибкой: ' + (t.error || 'без объяснения') + '. Сообщение в историю не попало.';
  } else {
    bubble.className = 'bubble markdown';
    bubble.innerHTML = renderMarkdown(t.reply || '');
  }
  m.appendChild(bubble);

  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.appendChild(who(d.agentKey));
  const tot = t.totals || {};
  const u = tot.usage || {};
  const parts = [];
  if (tot.toolCalls) parts.push(plural(tot.toolCalls, 'вызов', 'вызова', 'вызовов') + ' инструментов');
  parts.push((u.prompt || 0) + '→' + (u.completion || 0) + ' ток.');
  if (u.cacheHit) parts.push('кэш ' + Math.round(100 * u.cacheHit / Math.max(1, u.prompt)) + '%');
  if (tot.cost && tot.cost.known) parts.push(formatUSD(tot.cost.usd));
  parts.push((tot.seconds || 0).toFixed(1) + ' с');
  for (const p of parts) {
    const span = document.createElement('span');
    span.textContent = p;
    meta.appendChild(span);
  }
  const link = document.createElement('button');
  link.type = 'button';
  link.className = 'link' + (state.selectedTurn === t.id ? ' active' : '');
  link.textContent = 'журнал';
  link.addEventListener('click', () => {
    state.selectedTurn = t.id;
    renderJournal(currentTurn());
    for (const b of el.thread.querySelectorAll('button.link')) b.classList.toggle('active', b === link);
  });
  meta.appendChild(link);
  m.appendChild(meta);
  return m;
}

function who(agentKey) {
  const span = document.createElement('span');
  span.className = 'who';
  const info = state.agents.find((a) => a.key === agentKey);
  span.textContent = info ? info.title.toLowerCase() : agentKey;
  return span;
}

// updateRunningBubble показывает в пузыре идущего хода последнее событие
// журнала: видно, что агент делает, не глядя в журнал.
function updateRunningBubble() {
  const bubble = document.querySelector('#running-bubble .bubble');
  if (!bubble || !state.live) return;
  const evs = state.live.events.filter((e) => e.kind !== 'prompt');
  const last = evs.length ? evs[evs.length - 1] : null;
  bubble.textContent = last ? 'агент работает: ' + last.title + '…' : 'агент работает…';
}

/* ---------- Журнал хода ---------- */

function renderJournal(turn, statusOnly) {
  if (!turn) {
    el.logNote.textContent = state.current ? 'выберите ход в диалоге' : 'журнал появится после первого сообщения';
    el.turnStatus.innerHTML = '';
    el.log.innerHTML = '';
    el.totals.innerHTML = '';
    el.promptBox.hidden = true;
    return;
  }

  el.turnStatus.innerHTML = '';
  const dot = document.createElement('span');
  dot.className = 'dot ' + turn.status;
  el.turnStatus.appendChild(dot);
  el.turnStatus.appendChild(document.createTextNode(
    turn.status === 'running' ? 'агент работает…' : turn.status === 'done' ? 'готово' : 'ошибка'));
  renderTotals(turn);
  if (statusOnly) return;

  const idx = state.current ? state.current.turns.findIndex((t) => t.id === turn.id) : -1;
  el.logNote.textContent = idx >= 0 ? 'ход ' + (idx + 1) + ' · ' + formatTime(new Date(turn.started))
    : 'текущий ход · ' + formatTime(new Date(turn.started));

  el.log.innerHTML = '';
  el.promptBox.hidden = true;
  for (const ev of turn.events) appendEvent(ev);
  if (!turn.events.length) {
    const p = document.createElement('li');
    p.className = 'empty';
    p.textContent = 'Журнал пуст.';
    el.log.appendChild(p);
  }
}

function appendEvent(ev) {
  if (ev.kind === 'prompt') {
    renderPrompt(ev);
    return;
  }
  const li = document.createElement('li');
  li.className = 'entry ' + ev.kind;
  li.dataset.kind = ev.kind;

  const line = document.createElement('div');
  line.className = 'entry-line';

  const t = document.createElement('span');
  t.className = 't';
  t.textContent = '+' + offsetSeconds(ev.time).toFixed(1) + 'с';
  line.appendChild(t);

  const kind = document.createElement('span');
  kind.className = 'kind';
  kind.textContent = KIND_LABEL[ev.kind] || ev.kind;
  line.appendChild(kind);

  const title = document.createElement('span');
  title.className = 'title';
  title.textContent = ev.title;
  line.appendChild(title);

  if (ev.usage) {
    const u = document.createElement('span');
    u.className = 'usage';
    let text = ev.usage.prompt + '→' + ev.usage.completion + ' ток.';
    // Оценка до отправки рядом с фактом: видно, промахнулись или нет.
    if (ev.tokens && ev.tokens.estimated) {
      text = '≈' + ev.tokens.estimated + ' / ' + text;
      if (ev.tokens.errorPct) text += ' (' + signedPct(ev.tokens.errorPct) + ')';
    }
    if (ev.usage.cacheHit) text += ' · кэш ' + ev.usage.cacheHit;
    if (ev.seconds) text += ' · ' + ev.seconds.toFixed(1) + 'с';
    u.textContent = text;
    line.appendChild(u);
  } else if (ev.tokens && ev.tokens.estimated) {
    const u = document.createElement('span');
    u.className = 'usage';
    u.textContent = '≈' + ev.tokens.estimated + ' ток. в запросе';
    line.appendChild(u);
  } else if (ev.seconds && ev.kind !== 'agent.start') {
    const u = document.createElement('span');
    u.className = 'usage';
    u.textContent = ev.seconds.toFixed(1) + 'с';
    line.appendChild(u);
  }
  li.appendChild(line);

  if (ev.detail) {
    const details = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = 'подробности';
    const pre = document.createElement('pre');
    pre.textContent = ev.detail;
    details.appendChild(summary);
    details.appendChild(pre);
    li.appendChild(details);
  }

  li.hidden = !passesFilter(ev);
  const stick = el.log.scrollHeight - el.log.scrollTop - el.log.clientHeight < 40;
  el.log.appendChild(li);
  if (stick) el.log.scrollTop = el.log.scrollHeight;
}

// renderPrompt показывает, что получила модель на старте хода: системный
// промпт, новое сообщение, размер истории и инструменты.
function renderPrompt(ev) {
  let p = null;
  try { p = JSON.parse(ev.detail); } catch (_) { p = null; }

  el.promptBox.hidden = false;
  el.promptBox.dataset.agent = ev.agent;
  el.promptAgent.textContent = ev.agent;
  el.promptAgent.dataset.agent = ev.agent;
  const tools = p && p.tools ? p.tools : [];
  const est = p && p.estimate ? p.estimate : null;
  el.promptMeta.textContent = 'промпт хода · истории: ' + (p ? p.history : '?') + ' сообщ., ≈' + kilo(p ? p.historyRunes : 0) +
    ' символов · ' + (tools.length ? 'инструментов: ' + tools.length : 'без инструментов') +
    (est ? ' · ≈' + kilo(est.total) + ' токенов' : '');

  const body = el.promptBody;
  body.innerHTML = '';
  if (!p) {
    const pre = document.createElement('pre');
    pre.textContent = ev.detail || '';
    body.appendChild(pre);
    return;
  }
  if (est) body.appendChild(tokenSplit(est, p.limit));
  promptBlock(body, 'Сообщение system', p.system);
  promptBlock(body, 'История прошлых ходов', p.history + ' сообщ. в порядке диалога: user, assistant, tool… (ответы инструментов прошлых ходов могут быть сокращены)');
  promptBlock(body, 'Новое сообщение user', p.user);
  if (tools.length) {
    const h = document.createElement('h4');
    h.textContent = 'Инструменты, как они описаны модели';
    body.appendChild(h);
    const ul = document.createElement('ul');
    for (const t of tools) {
      const li = document.createElement('li');
      const code = document.createElement('code');
      code.textContent = t.name;
      li.appendChild(code);
      li.appendChild(document.createTextNode(' — ' + (t.description || '')));
      ul.appendChild(li);
    }
    body.appendChild(ul);
  }
}

// tokenSplit — из чего сложился контекст запроса. Полоска показывает
// доли: обычно почти всё место занимает история, и это и есть ответ на
// вопрос, почему длинный диалог дороже короткого при том же вопросе.
function tokenSplit(est, limit) {
  const box = document.createElement('div');
  box.className = 'token-split';

  const h = document.createElement('h4');
  h.textContent = 'Оценка запроса: ≈' + est.total + ' токенов' +
    (limit ? ' при лимите ' + limit + ' (' + Math.round(100 * est.total / limit) + '%)' : '');
  box.appendChild(h);

  const parts = [
    ['history', 'история', est.history],
    ['tools', 'инструменты', est.tools],
    ['system', 'системный промпт', est.system],
    ['user', 'новое сообщение', est.user]
  ].filter((p) => p[2] > 0);
  const sum = parts.reduce((a, p) => a + p[2], 0) || 1;

  const bar = document.createElement('div');
  bar.className = 'split-bar';
  for (const [cls, label, value] of parts) {
    const seg = document.createElement('span');
    seg.className = 'seg ' + cls;
    seg.style.width = (100 * value / sum) + '%';
    seg.title = label + ': ≈' + value + ' токенов';
    bar.appendChild(seg);
  }
  box.appendChild(bar);

  const legend = document.createElement('ul');
  legend.className = 'split-legend';
  for (const [cls, label, value] of parts) {
    const li = document.createElement('li');
    const dot = document.createElement('span');
    dot.className = 'seg ' + cls;
    li.appendChild(dot);
    li.appendChild(document.createTextNode(label + ' — ≈' + value + ' (' + Math.round(100 * value / sum) + '%)'));
    legend.appendChild(li);
  }
  box.appendChild(legend);
  return box;
}

function promptBlock(box, title, text) {
  const h = document.createElement('h4');
  h.textContent = title;
  box.appendChild(h);
  const pre = document.createElement('pre');
  pre.textContent = text || '';
  box.appendChild(pre);
}

function offsetSeconds(time) {
  const turn = currentTurn();
  if (!turn) return 0;
  return Math.max(0, (new Date(time) - new Date(turn.started)) / 1000);
}

function passesFilter(ev) {
  const f = state.filter;
  if (!f.tools && ev.kind.startsWith('tool.')) return false;
  if (!f.llm && ev.kind.startsWith('llm.')) return false;
  return true;
}

function applyFilter() {
  for (const li of el.log.children) {
    if (li.dataset.kind) li.hidden = !passesFilter({ kind: li.dataset.kind });
  }
}

function renderTotals(turn) {
  const t = turn.totals || {};
  const u = t.usage || {};
  const parts = [
    ['время', (t.seconds || 0).toFixed(1) + ' с'],
    ['к модели', String(t.llmCalls || 0)],
    ['инструментов', String(t.toolCalls || 0)],
    ['токены', (u.prompt || 0) + '→' + (u.completion || 0)]
  ];
  if (u.cacheHit) parts.push(['из кэша', u.cacheHit + ' (' + Math.round(100 * u.cacheHit / Math.max(1, u.prompt)) + '%)']);
  const c = turn.context;
  if (c && c.estimate && c.estimate.total) {
    let text = '≈' + c.estimate.total;
    // Факт первого запроса — то единственное, с чем оценка сравнима:
    // дальше в ходе к контексту добавляются ответы инструментов.
    if (c.firstPrompt) text += ' против ' + c.firstPrompt + ' (' + signedPct((c.estimate.total - c.firstPrompt) / c.firstPrompt * 100) + ')';
    parts.push(['оценка контекста', text]);
  }
  if (c && c.peak && c.estimate && c.peak > c.estimate.total) parts.push(['пик внутри хода', '≈' + c.peak]);
  if (c && c.trimmed) parts.push(['выброшено из истории', plural(c.trimmed, 'сообщение', 'сообщения', 'сообщений')]);
  if (t.cost && t.cost.known) parts.push(['стоимость', formatUSD(t.cost.usd) + ' (' + t.cost.tariff + ')']);
  el.totals.innerHTML = '';
  for (const [k, val] of parts) {
    const span = document.createElement('span');
    span.textContent = k + ' ';
    const strong = document.createElement('strong');
    strong.textContent = val;
    span.appendChild(strong);
    el.totals.appendChild(span);
  }
  if (turn.saveError) {
    const span = document.createElement('span');
    span.style.color = 'var(--error)';
    span.textContent = 'история не записана: ' + turn.saveError;
    el.totals.appendChild(span);
  }
}

/* ---------- Рост токенов и стоимости ---------- */

const SVG = 'http://www.w3.org/2000/svg';

// renderGrowth показывает, как растут контекст и деньги по мере диалога.
// Главное здесь то, что видно не сразу: короткие вопросы не значат
// дешёвые ходы. Историю модель получает заново на каждом ходе, поэтому
// столбик контекста растёт, даже когда сам вопрос — три слова.
function renderGrowth(d) {
  const turns = (d && d.turns ? d.turns : []).filter((t) => t.totals && t.totals.usage);
  el.growthBox.hidden = turns.length === 0;
  if (!turns.length) return;

  let totalIn = 0, totalOut = 0, totalCost = 0, costKnown = true;
  const rows = turns.map((t, i) => {
    const u = t.totals.usage || {};
    const c = t.context || {};
    totalIn += u.prompt || 0;
    totalOut += u.completion || 0;
    if (t.totals.cost && t.totals.cost.known) totalCost += t.totals.cost.usd;
    else costKnown = false;
    return {
      n: i + 1,
      estimate: (c.estimate && c.estimate.total) || 0,
      actual: c.firstPrompt || 0,
      prompt: u.prompt || 0,
      completion: u.completion || 0,
      cacheHit: u.cacheHit || 0,
      cost: t.totals.cost && t.totals.cost.known ? t.totals.cost.usd : null,
      cumulative: costKnown ? totalCost : null,
      trimmed: c.trimmed || 0
    };
  });

  el.growthMeta.textContent = 'рост токенов и стоимости · ходов: ' + rows.length +
    ' · всего ' + kilo(totalIn) + ' токенов на вход, ' + kilo(totalOut) + ' на выход' +
    (costKnown ? ' · ' + formatUSD(totalCost) : '');

  const body = el.growthBody;
  body.innerHTML = '';
  body.appendChild(growthChart(rows));
  body.appendChild(growthTable(rows));

  const hint = document.createElement('p');
  hint.className = 'hint';
  hint.textContent = 'Столбик — токены запроса за ход (вся история заново), линия — накопленная стоимость диалога. ' +
    'Оценка считается до отправки, факт приходит в usage ответа.';
  body.appendChild(hint);
}

// growthChart — столбики токенов и линия накопленной стоимости. Рисуется
// вручную в SVG: одна картинка не стоит того, чтобы тащить библиотеку.
function growthChart(rows) {
  const W = 640, H = 200, padL = 46, padR = 46, padT = 12, padB = 26;
  const maxTok = Math.max(...rows.map((r) => r.prompt), 1);
  const maxCost = Math.max(...rows.map((r) => r.cumulative || 0), 1e-9);
  const plotW = W - padL - padR, plotH = H - padT - padB;
  const step = plotW / rows.length;
  const barW = Math.max(2, Math.min(38, step * 0.6));

  const svg = document.createElementNS(SVG, 'svg');
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('class', 'chart');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', 'токены и стоимость по ходам диалога');

  // Ось токенов слева: ноль, середина, максимум.
  for (const part of [0, 0.5, 1]) {
    const y = padT + plotH - plotH * part;
    const line = document.createElementNS(SVG, 'line');
    line.setAttribute('x1', padL); line.setAttribute('x2', W - padR);
    line.setAttribute('y1', y); line.setAttribute('y2', y);
    line.setAttribute('class', part === 0 ? 'axis' : 'grid');
    svg.appendChild(line);

    svg.appendChild(chartText(padL - 6, y + 4, kilo(Math.round(maxTok * part)), 'end', 'tick'));
    if (part > 0) {
      svg.appendChild(chartText(W - padR + 6, y + 4, formatUSD(maxCost * part), 'start', 'tick cost'));
    }
  }

  rows.forEach((r, i) => {
    const x = padL + step * i + (step - barW) / 2;
    const h = plotH * (r.prompt / maxTok);
    const bar = document.createElementNS(SVG, 'rect');
    bar.setAttribute('x', x); bar.setAttribute('y', padT + plotH - h);
    bar.setAttribute('width', barW); bar.setAttribute('height', Math.max(1, h));
    bar.setAttribute('class', 'bar');
    const title = document.createElementNS(SVG, 'title');
    title.textContent = `ход ${r.n}: ${r.prompt} токенов на вход, ${r.completion} на выход`;
    bar.appendChild(title);
    svg.appendChild(bar);

    // Доля из кэша — она почти ничего не стоит, и это видно по линии денег.
    if (r.cacheHit) {
      const ch = plotH * (r.cacheHit / maxTok);
      const hit = document.createElementNS(SVG, 'rect');
      hit.setAttribute('x', x); hit.setAttribute('y', padT + plotH - ch);
      hit.setAttribute('width', barW); hit.setAttribute('height', Math.max(1, ch));
      hit.setAttribute('class', 'bar cached');
      const t2 = document.createElementNS(SVG, 'title');
      t2.textContent = `ход ${r.n}: из кэша ${r.cacheHit} токенов`;
      hit.appendChild(t2);
      svg.appendChild(hit);
    }

    if (rows.length <= 12 || (i + 1) % 5 === 0) {
      svg.appendChild(chartText(padL + step * i + step / 2, H - 8, String(r.n), 'middle', 'tick'));
    }
  });

  const points = rows
    .map((r, i) => (r.cumulative === null ? null : [padL + step * i + step / 2, padT + plotH - plotH * (r.cumulative / maxCost)]))
    .filter(Boolean);
  if (points.length > 1) {
    const path = document.createElementNS(SVG, 'polyline');
    path.setAttribute('points', points.map((p) => p.join(',')).join(' '));
    path.setAttribute('class', 'costline');
    svg.appendChild(path);
  }
  for (const p of points) {
    const dot = document.createElementNS(SVG, 'circle');
    dot.setAttribute('cx', p[0]); dot.setAttribute('cy', p[1]); dot.setAttribute('r', 2.5);
    dot.setAttribute('class', 'costdot');
    svg.appendChild(dot);
  }
  return svg;
}

function chartText(x, y, text, anchor, cls) {
  const t = document.createElementNS(SVG, 'text');
  t.setAttribute('x', x); t.setAttribute('y', y);
  t.setAttribute('text-anchor', anchor);
  t.setAttribute('class', cls);
  t.textContent = text;
  return t;
}

// growthTable — те же числа, но точные: картинка показывает форму роста,
// таблица отвечает на вопрос «сколько именно».
function growthTable(rows) {
  const table = document.createElement('table');
  table.className = 'growth-table';
  const head = document.createElement('tr');
  for (const h of ['ход', 'оценка', 'факт', 'вход', 'из кэша', 'выход', 'ход стоил', 'всего']) {
    const th = document.createElement('th');
    th.textContent = h;
    head.appendChild(th);
  }
  table.appendChild(head);

  for (const r of rows) {
    const tr = document.createElement('tr');
    const err = r.actual ? Math.round((r.estimate - r.actual) / r.actual * 100) : null;
    const cells = [
      String(r.n) + (r.trimmed ? ' ✂' : ''),
      r.estimate ? '≈' + r.estimate : '—',
      r.actual ? String(r.actual) + (err === null ? '' : ' (' + (err > 0 ? '+' : '') + err + '%)') : '—',
      String(r.prompt),
      r.cacheHit ? String(r.cacheHit) : '—',
      String(r.completion),
      r.cost === null ? '—' : formatUSD(r.cost),
      r.cumulative === null ? '—' : formatUSD(r.cumulative)
    ];
    for (const c of cells) {
      const td = document.createElement('td');
      td.textContent = c;
      tr.appendChild(td);
    }
    table.appendChild(tr);
  }
  return table;
}

/* ---------- Файл и удаление ---------- */

async function showFile() {
  if (!state.current) return;
  try {
    const data = await getJSON('/api/conversations/' + encodeURIComponent(state.current.id) + '/file');
    el.filePath.textContent = data.path;
    el.fileBody.textContent = data.json;
    el.fileDialog.showModal();
  } catch (err) {
    showError('Не удалось прочитать файл: ' + err.message);
  }
}

// deleteConversation удаляет диалог вместе с файлом на диске. Вызывается и
// из шапки открытого диалога, и из списка слева, поэтому диалог задаётся
// явно: в списке можно удалить не тот, что открыт.
async function deleteConversation(id, title) {
  if (!id) return;
  if (!confirm('Удалить диалог «' + (title || id) + '» вместе с файлом на диске?')) return;
  try {
    const res = await fetch('/api/conversations/' + encodeURIComponent(id), { method: 'DELETE' });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || res.statusText);
    showError('');
    if (state.current && state.current.id === id) {
      history.replaceState(null, '', location.pathname);
      openNew();
    }
    await refreshList();
  } catch (err) {
    showError('Не удалось удалить: ' + err.message);
  }
}

/* ---------- Утилиты ---------- */

async function getJSON(url) {
  const res = await fetch(url);
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

async function postJSON(url, body) {
  const res = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function renderMarkdown(text) {
  if (!window.marked) return '<pre>' + escapeHTML(text) + '</pre>';
  return marked.parse(text);
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function formatUSD(usd) {
  return usd >= 0.01 ? '$' + usd.toFixed(4) : '$' + usd.toFixed(6);
}

function formatTime(d) {
  if (isNaN(d)) return '—';
  const pad = (n) => String(n).padStart(2, '0');
  return pad(d.getDate()) + '.' + pad(d.getMonth() + 1) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
}

function kilo(n) {
  n = n || 0;
  return n < 1000 ? String(n) : (n / 1000).toFixed(1) + ' тыс.';
}

// signedPct — расхождение оценки с фактом со знаком: плюс значит, что
// оценка завысила.
function signedPct(pct) {
  const rounded = Math.round(pct);
  return (rounded > 0 ? '+' : '') + rounded + '%';
}

function plural(n, one, few, many) {
  const m10 = n % 10, m100 = n % 100;
  const word = m10 === 1 && m100 !== 11 ? one : (m10 >= 2 && m10 <= 4 && (m100 < 10 || m100 >= 20) ? few : many);
  return n + ' ' + word;
}

function showError(text) {
  el.error.textContent = text;
  el.error.hidden = !text;
}

init();

// llmgw admin UI — a small dependency-free SPA on top of /admin/api.
'use strict';

// ---------------------------------------------------------------- utilities

const state = { csrf: '', user: '', mode: '' };

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (k === 'html') el.innerHTML = v; // only used with static strings
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

class APIError extends Error {
  constructor(status, message, code) { super(message); this.status = status; this.code = code; }
}

async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  if (method !== 'GET' && state.csrf) opts.headers['X-CSRF-Token'] = state.csrf;
  const res = await fetch('/admin/api' + path, opts);
  if (res.status === 204) return null;
  let data = null;
  const text = await res.text();
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: { message: text } }; }
  if (!res.ok) {
    const msg = (data && data.error && data.error.message) || res.statusText;
    if (res.status === 401 && !path.startsWith('/login')) { showLogin(); }
    throw new APIError(res.status, msg, data && data.error && data.error.code);
  }
  return data;
}

function toast(msg, isErr) {
  const t = h('div', { class: 'toast' + (isErr ? ' err' : '') }, msg);
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), isErr ? 7000 : 3500);
}

async function guard(fn) {
  try { return await fn(); } catch (e) { toast(e.message || String(e), true); }
}

const fmtTime = (s) => s ? new Date(s).toLocaleString() : '—';
const fmtMs = (ms) => ms === null || ms === undefined ? '—' : (ms >= 1000 ? (ms / 1000).toFixed(ms % 1000 ? 1 : 0) + 's' : ms + 'ms');
const fmtDur = (ms) => ms === 0 ? '無制限' : fmtMs(ms);
const srcLabel = { global: '全体', provider: 'プロバイダ', alias: 'エイリアス' };

function badge(text, kind) { return h('span', { class: 'badge' + (kind ? ' ' + kind : '') }, text); }
function statusBadge(code) {
  return badge(code, code >= 500 ? 'err' : code >= 400 ? 'warn' : code === 499 ? 'warn' : 'ok');
}
function pretty(v) {
  if (v === null || v === undefined) return '(なし)';
  return typeof v === 'string' ? v : JSON.stringify(v, null, 2);
}
async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast('コピーしました'); }
  catch { toast('コピーできませんでした', true); }
}
function field(label, input, hint) {
  return h('label', { class: 'field' }, h('span', {}, label), input, hint ? h('span', { class: 'muted small' }, hint) : null);
}
function intOrNull(v) { v = String(v ?? '').trim(); return v === '' ? null : parseInt(v, 10); }
function secToMs(v) { v = String(v ?? '').trim(); return v === '' ? null : Math.round(parseFloat(v) * 1000); }
function msToSec(ms) { return ms === null || ms === undefined ? '' : String(ms / 1000); }
function localInput(iso) {
  if (!iso) return '';
  const d = new Date(iso); const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}
function toISO(local) { return local ? new Date(local).toISOString().replace(/\.\d{3}Z$/, 'Z') : null; }

// modal returns a promise resolved with the value passed to close().
function modal(title, body, buttons, opts = {}) {
  return new Promise((resolve) => {
    const root = document.getElementById('modal-root');
    const close = (v) => { backdrop.remove(); document.removeEventListener('keydown', onKey); resolve(v); };
    const onKey = (e) => { if (e.key === 'Escape') close(undefined); };
    const footer = h('footer', {}, (buttons || [{ label: '閉じる', value: undefined }]).map((b) =>
      h('button', { class: b.class || '', onclick: async () => {
        if (b.action) { const r = await b.action(); if (r === false || r === undefined) return; close(r); }
        else close(b.value);
      } }, b.label)));
    const box = h('div', { class: 'modal' + (opts.wide ? ' wide' : '') },
      h('header', {}, h('span', {}, title), h('button', { class: 'link', onclick: () => close(undefined) }, '✕')),
      h('div', { class: 'body' }, body), footer);
    // Close only when both mousedown and mouseup happen on the backdrop itself.
    // Otherwise a text-selection drag that starts inside the dialog and ends
    // outside it would fire a click on the backdrop and discard the input.
    let downOnBackdrop = false;
    const backdrop = h('div', {
      class: 'modal-backdrop',
      onmousedown: (e) => { downOnBackdrop = e.target === backdrop; },
      onclick: (e) => { if (downOnBackdrop && e.target === backdrop) close(undefined); downOnBackdrop = false; },
    }, box);
    document.addEventListener('keydown', onKey);
    root.append(backdrop);
    const first = box.querySelector('input,select,textarea'); if (first) first.focus();
  });
}
async function confirmDialog(msg, okLabel = '実行', danger = true) {
  return modal('確認', h('p', {}, msg), [
    { label: 'キャンセル', value: false },
    { label: okLabel, value: true, class: danger ? 'danger' : 'primary' }]);
}

// --------------------------------------------------------------- routing

const routes = {};
function route(name, fn) { routes[name] = fn; }

async function render() {
  const [name, ...rest] = (location.hash.replace(/^#\/?/, '') || 'dashboard').split('/');
  const main = document.getElementById('main');
  document.querySelectorAll('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.route === name));
  const fn = routes[name] || routes.dashboard;
  main.replaceChildren(h('p', { class: 'muted' }, '読み込み中…'));
  try {
    const view = await fn(...rest);
    main.replaceChildren(view);
  } catch (e) {
    if (e instanceof APIError && e.status === 401) return;
    main.replaceChildren(h('div', { class: 'panel err-text' }, 'エラー: ' + e.message));
  }
}

// ------------------------------------------------------------------ login

function showLogin() {
  document.getElementById('nav').hidden = true;
  const user = h('input', { value: 'admin', autocomplete: 'username' });
  const pass = h('input', { type: 'password', autocomplete: 'current-password' });
  const err = h('p', { class: 'err-text small' });
  const form = h('form', { class: 'panel login', onsubmit: async (e) => {
    e.preventDefault(); err.textContent = '';
    try {
      const s = await api('POST', '/login', { username: user.value, password: pass.value });
      setSession(s); render();
    } catch (ex) { err.textContent = ex.message; }
  } }, h('h1', {}, 'llmgw'), field('ユーザー名', user), field('パスワード', pass), err,
  h('button', { class: 'primary', type: 'submit' }, 'ログイン'));
  document.getElementById('main').replaceChildren(form);
  pass.focus();
}

function setSession(s) {
  state.csrf = s.csrf_token || ''; state.user = s.username; state.mode = s.mode;
  document.getElementById('nav').hidden = false;
  document.getElementById('whoami').textContent = s.username;
  document.getElementById('logout').hidden = s.mode !== 'session';
}

// -------------------------------------------------------------- dashboard

route('dashboard', async () => {
  const [prov, errs] = await Promise.all([api('GET', '/providers'), api('GET', '/logs?status=error&limit=15')]);
  const cards = prov.data.map((p) => h('div', { class: 'card' },
    h('div', { class: 'title' }, h('span', {}, p.name),
      badge(p.enabled ? (p.health.status === 'ok' ? '稼働中' : p.health.status === 'error' ? 'エラー' : '未確認') : '無効',
        !p.enabled ? '' : p.health.status === 'ok' ? 'ok' : p.health.status === 'error' ? 'err' : '')),
    h('div', { class: 'muted small mono' }, p.base_url),
    h('dl', { class: 'kv' },
      h('dt', {}, '種別'), h('dd', {}, p.type),
      h('dt', {}, 'モデル数'), h('dd', {}, p.models.length),
      h('dt', {}, '最終確認'), h('dd', {}, fmtTime(p.health.checked_at)),
      p.health.error ? [h('dt', {}, 'エラー'), h('dd', { class: 'err-text small' }, p.health.error)] : null)));
  return h('div', {},
    h('h1', {}, 'ダッシュボード'),
    h('h2', {}, 'プロバイダ'),
    cards.length ? h('div', { class: 'cards' }, cards) : h('p', { class: 'muted' }, 'プロバイダが未登録です。', h('a', { href: '#/providers' }, '追加する')),
    h('h2', {}, '直近のエラー'),
    logTable(errs.data));
});

// ------------------------------------------------------------------- keys

route('keys', async () => {
  const [keys, aliases] = await Promise.all([api('GET', '/keys'), api('GET', '/aliases')]);
  const aliasNames = aliases.data.map((a) => a.name);
  const rows = keys.data.map((k) => h('tr', {},
    h('td', {}, h('strong', {}, k.name), k.imported ? [' ', badge('インポート')] : null),
    h('td', { class: 'mono' }, k.key_prefix),
    h('td', {}, k.status === 'active' ? (k.expired ? badge('期限切れ', 'warn') : badge('有効', 'ok')) : badge('無効', 'err'),
      k.previous_key_valid_until ? h('div', { class: 'small muted' }, '旧キー併用〜' + fmtTime(k.previous_key_valid_until)) : null),
    h('td', {}, k.allowed_aliases.length ? k.allowed_aliases.join(', ') : h('span', { class: 'muted' }, 'すべて')),
    h('td', {}, k.log_mode),
    h('td', { class: 'small' }, fmtTime(k.last_used_at)),
    h('td', { class: 'small' }, k.expires_at ? fmtTime(k.expires_at) : '—'),
    h('td', { class: 'actions' },
      h('button', { class: 'small', onclick: () => editKey(k, aliasNames) }, '編集'), ' ',
      h('button', { class: 'small', onclick: () => rotateKey(k) }, '再発行'), ' ',
      h('button', { class: 'small', onclick: () => guard(async () => {
        await api('POST', `/keys/${k.id}/${k.status === 'active' ? 'disable' : 'enable'}`); render(); }) },
      k.status === 'active' ? '無効化' : '有効化'), ' ',
      h('button', { class: 'small danger', onclick: async () => {
        if (await confirmDialog(`キー「${k.name}」を削除します。元に戻せません。`, '削除')) {
          await guard(() => api('DELETE', `/keys/${k.id}`)); render();
        } } }, '削除'))));
  return h('div', {},
    h('h1', {}, 'Virtual Key', h('span', { class: 'spacer' }), h('button', { class: 'primary', onclick: () => editKey(null, aliasNames) }, '+ キーを発行')),
    h('table', {}, h('thead', {}, h('tr', {}, ['名前', 'キー', '状態', '許可エイリアス', 'ログ', '最終使用', '期限', ''].map((t) => h('th', {}, t)))),
      h('tbody', {}, rows.length ? rows : h('tr', {}, h('td', { colspan: 8, class: 'muted' }, 'キーがありません')))));
});

function aliasPicker(all, selected) {
  const set = new Set(selected);
  const extra = h('input', { placeholder: '一覧にない名前（カンマ区切り）', value: selected.filter((s) => !all.includes(s)).join(', ') });
  const boxes = all.map((n) => h('label', { class: 'check' }, h('input', { type: 'checkbox', value: n, checked: set.has(n) }), n));
  const el = h('div', {}, h('div', { class: 'row' }, boxes.length ? boxes : h('span', { class: 'muted small' }, 'エイリアス未登録')), h('div', { class: 'mt6' }, extra));
  el.getValue = () => [...boxes.filter((b) => b.firstChild.checked).map((b) => b.firstChild.value),
    ...extra.value.split(',').map((s) => s.trim()).filter(Boolean)];
  return el;
}

async function editKey(k, aliasNames) {
  const isNew = !k;
  k = k || { name: '', allowed_aliases: [], log_mode: 'full', rpm_limit: null, max_concurrency: null, expires_at: null, nonstream_keepalive: false };
  const name = h('input', { value: k.name });
  const picker = aliasPicker(aliasNames, k.allowed_aliases);
  const logMode = h('select', {}, ['full', 'metadata_only', 'none'].map((m) => h('option', { value: m, selected: k.log_mode === m ? 'selected' : null }, m)));
  const rpm = h('input', { type: 'number', min: 0, value: k.rpm_limit ?? '' });
  const conc = h('input', { type: 'number', min: 0, value: k.max_concurrency ?? '' });
  const exp = h('input', { type: 'datetime-local', value: localInput(k.expires_at) });
  const nsk = h('input', { type: 'checkbox', checked: k.nonstream_keepalive });
  const body = h('div', {},
    field('名前（用途メモ）', name),
    field('許可エイリアス（未選択なら全許可）', picker),
    h('div', { class: 'grid2' },
      field('ログモード', logMode, 'full=本文も保存 / metadata_only / none=記録しない'),
      field('有効期限', exp, '空欄で無期限'),
      field('RPM上限', rpm, '空欄で無制限'),
      field('同時実行上限', conc, '空欄で無制限')),
    h('label', { class: 'check' }, nsk, '非ストリーミング応答で keepalive（空白）を送る'));
  const payload = () => ({ name: name.value, allowed_aliases: picker.getValue(), log_mode: logMode.value,
    rpm_limit: intOrNull(rpm.value), max_concurrency: intOrNull(conc.value), expires_at: toISO(exp.value),
    nonstream_keepalive: nsk.checked });
  const res = await modal(isNew ? 'キーを発行' : 'キーを編集', body, [
    { label: 'キャンセル' },
    { label: isNew ? '発行' : '保存', class: 'primary', action: async () => guard(async () =>
      isNew ? api('POST', '/keys', payload()) : api('PATCH', `/keys/${k.id}`, payload())) || false }]);
  if (res && res.key) await showPlainKey(res.key);
  if (res) render();
}

async function showPlainKey(key) {
  const input = h('input', { value: key, readonly: true, class: 'mono wfull' });
  await modal('キーを控えてください', h('div', {},
    h('p', {}, 'このキーは今だけ表示されます。閉じると二度と確認できません。'),
    h('div', { class: 'row' }, h('div', { class: 'grow' }, input), h('button', { onclick: () => copy(key) }, 'コピー'))),
  [{ label: '控えました', class: 'primary' }]);
}

async function rotateKey(k) {
  const grace = h('input', { type: 'number', min: 0, max: 10080, value: 0 });
  const res = await modal(`「${k.name}」を再発行`, h('div', {},
    h('p', {}, '名前・権限・設定はそのままに新しいキーへ差し替えます。'),
    field('旧キーの猶予（分）', grace, '0 なら旧キーは即時無効')),
  [{ label: 'キャンセル' }, { label: '再発行', class: 'primary', action: () =>
    guard(() => api('POST', `/keys/${k.id}/rotate`, { grace_minutes: intOrNull(grace.value) || 0 })) || false }]);
  if (res && res.key) { await showPlainKey(res.key); render(); }
}

// -------------------------------------------------------------- providers

const timeoutFields = [
  ['connect_timeout_ms', '接続', 'connect_timeout'], ['queue_timeout_ms', 'キュー待ち', 'queue_timeout'],
  ['ttft_timeout_ms', '初回トークン(TTFT)', 'ttft_timeout'], ['idle_timeout_ms', '無通信', 'idle_timeout'],
  ['max_duration_ms', '全体上限', 'max_duration']];

route('providers', async () => {
  const prov = await api('GET', '/providers');
  const rows = prov.data.map((p) => h('tr', {},
    h('td', {}, h('strong', {}, p.name), p.enabled ? null : [' ', badge('無効')]),
    h('td', {}, p.type),
    h('td', { class: 'mono small' }, p.base_url),
    h('td', {}, badge(p.health.status === 'ok' ? 'OK' : p.health.status === 'error' ? 'エラー' : '未確認',
      p.health.status === 'ok' ? 'ok' : p.health.status === 'error' ? 'err' : ''),
    p.health.error ? h('div', { class: 'small err-text' }, p.health.error) : null),
    h('td', {}, p.supports_anthropic ? badge('対応', 'ok') : h('span', { class: 'muted' }, '—')),
    h('td', {}, p.models.length),
    h('td', { class: 'small' }, p.aliases.join(', ') || '—'),
    h('td', { class: 'actions' },
      h('button', { class: 'small', onclick: () => testProvider(p) }, '疎通確認'), ' ',
      h('button', { class: 'small', onclick: () => editProvider(p) }, '編集'), ' ',
      h('button', { class: 'small danger', onclick: async () => {
        if (await confirmDialog(`プロバイダ「${p.name}」を削除します。`, '削除')) { await guard(() => api('DELETE', `/providers/${p.id}`)); render(); }
      } }, '削除'))));
  return h('div', {},
    h('h1', {}, 'プロバイダ', h('span', { class: 'spacer' }), h('button', { class: 'primary', onclick: () => editProvider(null) }, '+ プロバイダを追加')),
    h('table', {}, h('thead', {}, h('tr', {}, ['名前', '種別', 'ベースURL', '状態', 'Anthropic互換', 'モデル', '使用エイリアス', ''].map((t) => h('th', {}, t)))),
      h('tbody', {}, rows.length ? rows : h('tr', {}, h('td', { colspan: 8, class: 'muted' }, 'プロバイダがありません')))));
});

function timeoutInputs(obj, effective, unit) {
  const inputs = {};
  const els = timeoutFields.map(([key, label, ekey]) => {
    const eff = effective && effective[ekey];
    const inp = h('input', { type: 'number', min: 0, step: 'any', value: msToSec(obj[key]),
      placeholder: eff ? `継承: ${fmtDur(eff.ms)}（${srcLabel[eff.source] || eff.source}）` : '継承' });
    const warn = h('div', { class: 'warn-text' });
    const check = () => { warn.textContent = inp.value.trim() === '0' && key !== 'max_duration_ms' ? '0 は無制限（非推奨）' : ''; };
    inp.addEventListener('input', check); check();
    inputs[key] = inp;
    return field(`${label}（秒）`, h('div', {}, inp, warn));
  });
  return { el: h('div', { class: 'grid2' }, els), values: () => Object.fromEntries(Object.entries(inputs).map(([k, i]) => [k, secToMs(i.value)])), unit };
}

async function editProvider(p) {
  const isNew = !p;
  p = p || { name: '', type: 'ollama', base_url: 'http://localhost:11434', enabled: true, supports_anthropic: false, max_concurrency: null, has_api_key: false };
  const name = h('input', { value: p.name });
  const type = h('select', {}, [['ollama', 'Ollama'], ['openai_compatible', 'OpenAI互換 (LM Studio / llama.cpp / vLLM…)']]
    .map(([v, l]) => h('option', { value: v, selected: p.type === v ? 'selected' : null }, l)));
  const url = h('input', { value: p.base_url, class: 'mono' });
  const key = h('input', { type: 'password', autocomplete: 'off', placeholder: p.has_api_key ? '設定済み（変更する場合のみ入力）' : '任意' });
  const clearKey = h('input', { type: 'checkbox' });
  const anth = h('input', { type: 'checkbox', checked: p.supports_anthropic });
  const enabled = h('input', { type: 'checkbox', checked: p.enabled });
  const conc = h('input', { type: 'number', min: 0, value: p.max_concurrency ?? '' });
  const to = timeoutInputs(p, p.effective);
  const body = h('div', {},
    h('div', { class: 'grid2' }, field('名前', name), field('種別', type)),
    field('ベースURL', url, 'Ollama: http://host:11434 / OpenAI互換: http://host:1234/v1（パス省略時は /v1）'),
    field('APIキー（暗号化して保存）', key),
    p.has_api_key ? h('label', { class: 'check' }, clearKey, '保存済みのAPIキーを削除') : null,
    h('div', { class: 'row mv8' },
      h('label', { class: 'check' }, enabled, '有効'),
      h('label', { class: 'check' }, anth, 'Anthropic互換（/v1/messages）をパススルー',
        isNew ? h('span', { class: 'muted small' }, '（未チェックなら追加時に自動判定）') : null)),
    field('同時実行上限', conc, '空欄で無制限'),
    h('h2', {}, 'タイムアウト（空欄は全体設定を継承）'), to.el);
  const payload = () => {
    const o = { name: name.value, type: type.value, base_url: url.value, enabled: enabled.checked,
      max_concurrency: intOrNull(conc.value), ...to.values() };
    if (!isNew || anth.checked) o.supports_anthropic = anth.checked;
    if (key.value) o.api_key = key.value; else if (clearKey.checked) o.api_key = null;
    return o;
  };
  const res = await modal(isNew ? 'プロバイダを追加' : 'プロバイダを編集', body, [
    { label: 'キャンセル' },
    { label: '保存', class: 'primary', action: () => guard(() =>
      isNew ? api('POST', '/providers', payload()) : api('PATCH', `/providers/${p.id}`, payload())) || false }], { wide: true });
  if (res) {
    if (res.check) toast(res.check.ok ? `接続OK（モデル ${res.check.models.length} 件）` : '接続失敗: ' + res.check.error, !res.check.ok);
    render();
  }
}

async function testProvider(p) {
  const res = await guard(() => api('POST', `/providers/${p.id}/test`));
  if (!res) return;
  const c = res.check;
  const detected = c.anthropic_detected;
  const applyBtn = detected !== null && detected !== undefined && detected !== p.supports_anthropic
    ? h('button', { onclick: async () => { await guard(() => api('POST', `/providers/${p.id}/test`, { apply_anthropic: true })); toast('反映しました'); render(); } },
      `Anthropic互換を「${detected ? '対応' : '非対応'}」に変更`) : null;
  await modal(`疎通確認: ${p.name}`, h('div', {},
    h('dl', { class: 'kv' },
      h('dt', {}, '結果'), h('dd', {}, c.ok ? badge('OK', 'ok') : badge('失敗', 'err')),
      h('dt', {}, '所要時間'), h('dd', {}, fmtMs(c.latency_ms)),
      c.error ? [h('dt', {}, 'エラー'), h('dd', { class: 'err-text' }, c.error)] : null,
      h('dt', {}, 'Anthropic互換'), h('dd', {}, detected === null || detected === undefined ? '判定不可' : detected ? '検出' : '未検出',
        ' / 現在の設定: ', p.supports_anthropic ? '対応' : '非対応', ' ', applyBtn)),
    h('h2', {}, `モデル一覧（${c.models.length}）`),
    c.models.length ? h('pre', {}, c.models.join('\n')) : h('p', { class: 'muted' }, 'なし')));
  render();
}

// ---------------------------------------------------------------- aliases

route('aliases', async () => {
  const [aliases, prov] = await Promise.all([api('GET', '/aliases'), api('GET', '/providers')]);
  const rows = aliases.data.map((a) => h('tr', {},
    h('td', {}, h('strong', { class: 'mono' }, a.name), a.enabled ? null : [' ', badge('無効')],
      a.description ? h('div', { class: 'small muted' }, a.description) : null),
    h('td', {}, a.targets.map((t, i) => h('div', { class: 'small' }, `${i + 1}. `, h('span', {}, t.provider_name), ' → ',
      h('span', { class: 'mono' }, t.upstream_model), t.provider_enabled ? null : [' ', badge('プロバイダ無効', 'warn')]))),
    h('td', { class: 'small' }, Object.keys(a.params || {}).length ? Object.keys(a.params).join(', ') : '—'),
    h('td', { class: 'actions' },
      h('a', { class: 'btn small', href: `#/playground/${encodeURIComponent(a.name)}` }, 'Playgroundで試す'), ' ',
      h('button', { class: 'small', onclick: () => editAlias(a, prov.data) }, '編集'), ' ',
      h('button', { class: 'small danger', onclick: async () => {
        if (await confirmDialog(`エイリアス「${a.name}」を削除します。`, '削除')) { await guard(() => api('DELETE', `/aliases/${a.id}`)); render(); }
      } }, '削除'))));
  return h('div', {},
    h('h1', {}, 'モデルエイリアス', h('span', { class: 'spacer' }),
      h('button', { class: 'primary', onclick: () => editAlias(null, prov.data), disabled: !prov.data.length }, '+ エイリアスを追加')),
    prov.data.length ? null : h('p', { class: 'muted' }, '先にプロバイダを登録してください。'),
    h('table', {}, h('thead', {}, h('tr', {}, ['公開モデル名', 'ターゲット（優先順）', 'パラメータ', ''].map((t) => h('th', {}, t)))),
      h('tbody', {}, rows.length ? rows : h('tr', {}, h('td', { colspan: 4, class: 'muted' }, 'エイリアスがありません')))));
});

function jsonArea(value, rows = 4) {
  const ta = h('textarea', { rows }, value && Object.keys(value).length ? JSON.stringify(value, null, 2) : '');
  ta.getValue = () => {
    const t = ta.value.trim();
    if (!t) return undefined;
    return JSON.parse(t);
  };
  return ta;
}

async function editAlias(a, providers) {
  const isNew = !a;
  a = a || { name: '', description: '', enabled: true, targets: [], params: {}, model_info: {}, fallback_on_ttft_timeout: null, effective: [] };
  const name = h('input', { value: a.name, class: 'mono', placeholder: '例: qwen3-coder / claude-sonnet-4-5' });
  const desc = h('input', { value: a.description });
  const enabled = h('input', { type: 'checkbox', checked: a.enabled });
  const list = h('div', {});
  const datalists = h('div', {});
  providers.forEach((p) => datalists.append(h('datalist', { id: `models-${p.id}` }, p.models.map((m) => h('option', { value: m })))));
  const addRow = (t) => {
    const sel = h('select', {}, providers.map((p) => h('option', { value: p.id, selected: t && t.provider_id === p.id ? 'selected' : null }, p.name)));
    const model = h('input', { class: 'mono', value: t ? t.upstream_model : '', placeholder: '上流のモデル名' });
    const syncList = () => model.setAttribute('list', `models-${sel.value}`);
    sel.addEventListener('change', syncList); syncList();
    const row = h('div', { class: 'target-row' }, sel, model,
      h('button', { class: 'small', title: '上へ', onclick: () => { if (row.previousSibling) list.insertBefore(row, row.previousSibling); } }, '↑'),
      h('button', { class: 'small danger', onclick: () => row.remove() }, '✕'));
    row.getValue = () => ({ provider_id: parseInt(sel.value, 10), upstream_model: model.value.trim() });
    list.append(row);
  };
  (a.targets.length ? a.targets : [null]).forEach(addRow);
  const to = {};
  const toEls = timeoutFields.filter(([k]) => ['ttft_timeout_ms', 'idle_timeout_ms', 'max_duration_ms'].includes(k)).map(([key, label]) => {
    const inp = h('input', { type: 'number', min: 0, step: 'any', value: msToSec(a[key]), placeholder: '継承' });
    const warn = h('div', { class: 'warn-text' });
    const check = () => { warn.textContent = inp.value.trim() === '0' && key !== 'max_duration_ms' ? '0 は無制限（非推奨）' : ''; };
    inp.addEventListener('input', check); check();
    to[key] = inp;
    return field(`${label}（秒）`, h('div', {}, inp, warn));
  });
  const fb = h('select', {}, [['', '継承'], ['true', 'する'], ['false', 'しない']].map(([v, l]) =>
    h('option', { value: v, selected: String(a.fallback_on_ttft_timeout ?? '') === v ? 'selected' : null }, l)));
  const effTable = a.effective.length ? h('table', {}, h('thead', {}, h('tr', {}, ['ターゲット', '接続', 'キュー', 'TTFT', '無通信', '全体', 'TTFT時FB'].map((t) => h('th', {}, t)))),
    h('tbody', {}, a.effective.map((e) => h('tr', {}, h('td', {}, e.provider_name),
      ['connect_timeout', 'queue_timeout', 'ttft_timeout', 'idle_timeout', 'max_duration'].map((k) =>
        h('td', { class: 'small' }, fmtDur(e[k].ms), h('div', { class: 'muted' }, srcLabel[e[k].source] || e[k].source))),
      h('td', {}, e.fallback_on_ttft_timeout ? 'する' : 'しない'))))) : h('p', { class: 'muted small' }, '保存後に実効値が表示されます');
  const params = a.params || {};
  const defaults = jsonArea(params.defaults);
  const overrides = jsonArea(params.overrides);
  const spMode = h('select', {}, [['', 'なし'], ['prepend', '先頭に追加'], ['replace', '置換']].map(([v, l]) =>
    h('option', { value: v, selected: ((params.system_prompt || {}).mode || '') === v ? 'selected' : null }, l)));
  const spText = h('textarea', { rows: 3 }, (params.system_prompt || {}).text || '');
  const mi = a.model_info || {};
  const miCtx = h('input', { type: 'number', min: 1, step: 1, value: mi.context_length ?? '', placeholder: '自動' });
  const triSelect = (v) => h('select', {}, [['', '自動'], ['true', 'あり'], ['false', 'なし']].map(([x, l]) =>
    h('option', { value: x, selected: String(v ?? '') === x ? 'selected' : null }, l)));
  const miTools = triSelect(mi.tools);
  const miVision = triSelect(mi.vision);
  const triValue = (sel) => (sel.value === '' ? null : sel.value === 'true');
  const body = h('div', {}, datalists,
    h('div', { class: 'grid2' }, field('公開モデル名', name, 'コロン可（name:tag）。Ollama クライアントからは :latest 省略可'), field('説明', desc)),
    h('label', { class: 'check mb12' }, enabled, '有効'),
    h('h2', {}, 'ターゲット（上から順に試行。接続失敗・応答開始前の5xxで次へフォールバック）'),
    list, h('button', { class: 'small', onclick: () => addRow(null) }, '+ ターゲットを追加'),
    h('h2', {}, 'タイムアウト'),
    h('div', { class: 'grid2' }, toEls, field('TTFTタイムアウト時にフォールバック', fb)),
    h('h2', {}, '実効値（どこから継承したか）'), effTable,
    h('h2', {}, 'パラメータ上書き'),
    h('div', { class: 'grid2' },
      field('defaults（未指定時のみ適用・JSON）', defaults, '例: {"temperature": 0.7}'),
      field('overrides（常に強制・JSON）', overrides, '例: {"num_ctx": 32768, "keep_alive": "30m", "max_tokens": 4096}')),
    h('div', { class: 'grid2' }, field('システムプロンプト', spMode), field('システムプロンプト本文', spText)),
    h('h2', {}, 'モデル情報（LM Studio互換の /api/v1/models で返す値の上書き）'),
    h('div', { class: 'grid2' },
      field('コンテキスト長（トークン）', miCtx, '空欄なら自動（Ollama: エイリアス／Modelfile の num_ctx → 実行中の値 → 学習時の最大長）'),
      field('ツール呼び出し', miTools, '自動: Ollama は capabilities、LM Studio は trained_for_tool_use から判定'),
      field('画像入力（vision）', miVision)));
  const payload = () => {
    const p = {};
    const d = defaults.getValue(); if (d !== undefined) p.defaults = d;
    const o = overrides.getValue(); if (o !== undefined) p.overrides = o;
    if (spMode.value) p.system_prompt = { mode: spMode.value, text: spText.value };
    return { name: name.value, description: desc.value, enabled: enabled.checked,
      targets: [...list.children].map((r) => r.getValue()),
      ttft_timeout_ms: secToMs(to.ttft_timeout_ms.value), idle_timeout_ms: secToMs(to.idle_timeout_ms.value),
      max_duration_ms: secToMs(to.max_duration_ms.value),
      fallback_on_ttft_timeout: fb.value === '' ? null : fb.value === 'true', params: p,
      model_info: { context_length: miCtx.value.trim() === '' ? null : parseInt(miCtx.value, 10),
        tools: triValue(miTools), vision: triValue(miVision) } };
  };
  const res = await modal(isNew ? 'エイリアスを追加' : `エイリアスを編集: ${a.name}`, body, [
    { label: 'キャンセル' },
    { label: '保存', class: 'primary', action: () => guard(() => {
      let body; try { body = payload(); } catch (e) { throw new Error('パラメータの JSON が不正です: ' + e.message); }
      return isNew ? api('POST', '/aliases', body) : api('PATCH', `/aliases/${a.id}`, body);
    }) || false }], { wide: true });
  if (res) render();
}

// ------------------------------------------------------------- playground

const pgState = { messages: [], abort: null };

route('playground', async (aliasParam) => {
  const [aliases, keys] = await Promise.all([api('GET', '/aliases'), api('GET', '/keys')]);
  const format = h('select', {}, [['openai', 'OpenAI (/v1/chat/completions)'], ['ollama', 'Ollama (/api/chat)'], ['anthropic', 'Anthropic (/v1/messages)']]
    .map(([v, l]) => h('option', { value: v }, l)));
  const authMode = h('select', {}, h('option', { value: 'as' }, '管理者権限でキーとして実行'), h('option', { value: 'plain' }, '平文キーを入力'));
  const keySel = h('select', {}, h('option', { value: '0' }, '（キーなし・全エイリアス）'),
    keys.data.filter((k) => k.status === 'active').map((k) => h('option', { value: k.id }, k.name)));
  const keyPlain = h('input', { type: 'password', placeholder: 'sk-gw-…', autocomplete: 'off' });
  const keyBox = h('div', {}, keySel);
  authMode.addEventListener('change', () => keyBox.replaceChildren(authMode.value === 'as' ? keySel : keyPlain));
  const alias = h('select', {}, aliases.data.filter((a) => a.enabled).map((a) =>
    h('option', { value: a.name, selected: decodeURIComponent(aliasParam || '') === a.name ? 'selected' : null }, a.name)));
  const system = h('textarea', { rows: 3, placeholder: '（任意）' });
  const temp = h('input', { type: 'number', step: '0.1', min: 0, max: 2, placeholder: '既定' });
  const maxTok = h('input', { type: 'number', min: 1, placeholder: '既定', value: '' });
  const stream = h('input', { type: 'checkbox', checked: true });
  const think = h('select', {}, [['', '既定'], ['on', 'オン'], ['off', 'オフ']].map(([v, l]) => h('option', { value: v }, l)));
  const chat = h('div', { class: 'chat' });
  const input = h('textarea', { rows: 3, placeholder: 'メッセージ（Ctrl+Enter で送信）' });
  const reqPre = h('pre', {}, '');
  const resPre = h('pre', {}, '');
  const curlPre = h('pre', {}, '');
  const tabsEl = h('div', {});
  let tab = 'req';
  const tabBtns = [['req', '送信リクエスト'], ['res', '受信レスポンス'], ['curl', 'curl']].map(([k, l]) =>
    h('button', { class: k === tab ? 'active' : '', onclick: () => { tab = k; tabBtns.forEach((b) => b.classList.toggle('active', b.dataset.k === k)); showTab(); }, 'data-k': k }, l));
  const showTab = () => tabsEl.replaceChildren({ req: reqPre, res: resPre, curl: curlPre }[tab]);
  showTab();
  const sendBtn = h('button', { class: 'primary' }, '送信');
  const stopBtn = h('button', { disabled: true }, '停止');

  const drawHistory = () => {
    chat.replaceChildren(...pgState.messages.map((m) => h('div', { class: 'msg ' + m.role }, h('div', { class: 'role' }, m.role),
      m.thinking ? h('div', { class: 'thinking' }, m.thinking) : null, m.content)));
    if (!pgState.messages.length) chat.append(h('p', { class: 'muted' }, 'メッセージを送信するとここに表示されます。'));
  };
  drawHistory();

  const buildRequest = () => {
    const f = format.value, msgs = pgState.messages.filter((m) => m.role === 'user' || m.role === 'assistant')
      .map((m) => ({ role: m.role, content: m.content }));
    const t = temp.value === '' ? undefined : parseFloat(temp.value);
    const mt = maxTok.value === '' ? undefined : parseInt(maxTok.value, 10);
    let path, body;
    if (f === 'openai') {
      path = '/v1/chat/completions';
      body = { model: alias.value, messages: system.value ? [{ role: 'system', content: system.value }, ...msgs] : msgs, stream: stream.checked };
      if (t !== undefined) body.temperature = t;
      if (mt !== undefined) body.max_tokens = mt;
      if (think.value) body.reasoning_effort = think.value === 'on' ? 'medium' : 'none';
      if (stream.checked) body.stream_options = { include_usage: true };
    } else if (f === 'ollama') {
      path = '/api/chat';
      body = { model: alias.value, messages: system.value ? [{ role: 'system', content: system.value }, ...msgs] : msgs, stream: stream.checked };
      const opts = {}; if (t !== undefined) opts.temperature = t; if (mt !== undefined) opts.num_predict = mt;
      if (Object.keys(opts).length) body.options = opts;
      if (think.value) body.think = think.value === 'on';
    } else {
      path = '/v1/messages';
      body = { model: alias.value, max_tokens: mt || 1024, messages: msgs, stream: stream.checked };
      if (system.value) body.system = system.value;
      if (t !== undefined) body.temperature = t;
      if (think.value === 'on') body.thinking = { type: 'enabled', budget_tokens: Math.max(1024, Math.floor((mt || 4096) / 2)) };
      else if (think.value === 'off') body.thinking = { type: 'disabled' };
    }
    return { path, body };
  };
  const curlFor = (path, body) => {
    const authH = format.value === 'anthropic' ? `-H "x-api-key: $LLMGW_KEY"` : `-H "Authorization: Bearer $LLMGW_KEY"`;
    return `curl ${body.stream ? '-N ' : ''}${location.origin}${path} \\\n  ${authH} \\\n  -H 'Content-Type: application/json' \\\n  -d '${JSON.stringify(body).replace(/'/g, "'\\''")}'`;
  };
  const refreshPreview = () => { const { path, body } = buildRequest(); curlPre.textContent = curlFor(path, body); };
  [format, alias, system, temp, maxTok, stream, think].forEach((el) => el.addEventListener('change', refreshPreview));

  // Extract text/thinking from one parsed stream event or full response.
  const extract = (f, obj) => {
    if (f === 'openai') {
      const c = (obj.choices || [])[0] || {};
      const d = c.delta || c.message || {};
      return { text: d.content || '', thinking: d.reasoning_content || d.reasoning || '',
        tools: (d.tool_calls || []).map((t) => (t.function || {}).name).filter(Boolean) };
    }
    if (f === 'ollama') {
      const m = obj.message || {};
      return { text: m.content || '', thinking: m.thinking || '', tools: (m.tool_calls || []).map((t) => t.function.name) };
    }
    if (obj.type === 'content_block_delta') {
      return { text: obj.delta.text || '', thinking: obj.delta.thinking || '', tools: [] };
    }
    if (obj.type === 'message') {
      let text = '', thinking = '';
      for (const b of obj.content || []) { if (b.type === 'text') text += b.text; if (b.type === 'thinking') thinking += b.thinking; }
      return { text, thinking, tools: (obj.content || []).filter((b) => b.type === 'tool_use').map((b) => b.name) };
    }
    return { text: '', thinking: '', tools: [] };
  };

  const send = async () => {
    const text = input.value.trim();
    if (!text || !alias.value) return;
    pgState.messages.push({ role: 'user', content: text });
    input.value = '';
    const { path, body } = buildRequest();
    reqPre.textContent = `POST ${path}\n\n` + JSON.stringify(body, null, 2);
    curlPre.textContent = curlFor(path, body);
    resPre.textContent = '';
    const reply = { role: 'assistant', content: '', thinking: '' };
    pgState.messages.push(reply);
    drawHistory();
    const bubble = chat.lastChild;
    const update = () => { bubble.replaceChildren(h('div', { class: 'role' }, 'assistant'),
      reply.thinking ? h('div', { class: 'thinking' }, reply.thinking) : null, reply.content || '…'); };
    const headers = { 'Content-Type': 'application/json' };
    let url;
    if (authMode.value === 'as') {
      url = `/admin/api/playground${path}?key_id=${keySel.value}`;
      headers['X-CSRF-Token'] = state.csrf;
    } else {
      url = path;
      if (format.value === 'anthropic') headers['x-api-key'] = keyPlain.value; else headers.Authorization = 'Bearer ' + keyPlain.value;
    }
    pgState.abort = new AbortController();
    sendBtn.disabled = true; stopBtn.disabled = false;
    const f = format.value;
    try {
      const res = await fetch(url, { method: 'POST', headers, body: JSON.stringify(body), credentials: 'same-origin', signal: pgState.abort.signal });
      if (!res.ok || !res.body || !body.stream) {
        const raw = await res.text();
        resPre.textContent = `HTTP ${res.status}\n\n` + raw;
        let obj; try { obj = JSON.parse(raw); } catch { obj = null; }
        if (!res.ok) throw new Error((obj && (obj.error && (obj.error.message || obj.error))) || raw || res.statusText);
        const x = extract(f, obj || {});
        reply.content = x.text + (x.tools.length ? `\n[tool_use: ${x.tools.join(', ')}]` : '');
        reply.thinking = x.thinking;
        update();
      } else {
        resPre.textContent = `HTTP ${res.status}\n\n`;
        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          const chunk = dec.decode(value, { stream: true });
          resPre.textContent += chunk;
          buf += chunk;
          let idx;
          while ((idx = buf.indexOf('\n')) >= 0) {
            const line = buf.slice(0, idx).trim(); buf = buf.slice(idx + 1);
            let payload = line;
            if (f !== 'ollama') { if (!line.startsWith('data:')) continue; payload = line.slice(5).trim(); }
            if (!payload || payload === '[DONE]') continue;
            let obj; try { obj = JSON.parse(payload); } catch { continue; }
            if (obj.error || obj.type === 'error') throw new Error(typeof obj.error === 'string' ? obj.error : obj.error.message);
            const x = extract(f, obj);
            reply.content += x.text + (x.tools.length ? `\n[tool_use: ${x.tools.join(', ')}]` : '');
            reply.thinking += x.thinking;
            update();
          }
        }
      }
    } catch (e) {
      if (e.name !== 'AbortError') { reply.role = 'error'; reply.content = (reply.content ? reply.content + '\n\n' : '') + 'エラー: ' + e.message; }
      drawHistory();
    } finally {
      sendBtn.disabled = false; stopBtn.disabled = true; pgState.abort = null;
    }
  };
  sendBtn.addEventListener('click', send);
  stopBtn.addEventListener('click', () => pgState.abort && pgState.abort.abort());
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) send(); });
  refreshPreview();

  return h('div', {},
    h('h1', {}, 'Playground', h('span', { class: 'spacer' }),
      h('button', { onclick: () => { pgState.messages = []; drawHistory(); } }, '会話をクリア')),
    h('div', { class: 'pg' },
      h('div', { class: 'panel' },
        field('API形式', format), field('認証', authMode), field('キー', keyBox), field('エイリアス', alias),
        field('システムプロンプト', system),
        h('div', { class: 'grid2' }, field('温度', temp), field('最大トークン', maxTok)),
        field('思考（thinking）', think),
        h('label', { class: 'check' }, stream, 'ストリーミング')),
      h('div', {},
        chat,
        h('div', { class: 'panel mt12' }, input, h('div', { class: 'row mt8' }, sendBtn, stopBtn)),
        h('div', { class: 'tabs' }, tabBtns), tabsEl,
        h('div', { class: 'mt6' }, h('button', { class: 'small', onclick: () => copy(curlPre.textContent) }, 'curl をコピー')))));
});

// ------------------------------------------------------------------- logs

function logTable(rows, onMore) {
  const body = rows.map((l) => h('tr', { class: 'clickable', onclick: () => showLog(l.id) },
    h('td', { class: 'small' }, fmtTime(l.created_at)),
    h('td', {}, l.key_name || h('span', { class: 'muted' }, '—')),
    h('td', { class: 'mono small' }, l.alias || '—'),
    h('td', { class: 'small' }, l.endpoint, h('div', { class: 'muted' }, l.inbound_format, l.upstream_format ? ' → ' + l.upstream_format : '', l.passthrough ? '（パススルー）' : '')),
    h('td', {}, statusBadge(l.status_code), l.timeout_phase ? [' ', badge('timeout:' + l.timeout_phase, 'err')] : null,
      l.interrupted ? [' ', badge('中断', 'warn')] : null, l.cancelled_by_client ? [' ', badge('切断', 'warn')] : null),
    h('td', { class: 'small' }, fmtMs(l.latency_ms)),
    h('td', { class: 'small' }, fmtMs(l.ttft_ms)),
    h('td', { class: 'small' }, l.prompt_tokens !== null ? `${l.prompt_tokens} / ${l.completion_tokens}` : '—')));
  return h('div', {},
    h('table', {}, h('thead', {}, h('tr', {}, ['日時', 'キー', 'エイリアス', 'エンドポイント', 'ステータス', 'レイテンシ', 'TTFT', 'トークン(入/出)'].map((t) => h('th', {}, t)))),
      h('tbody', {}, body.length ? body : h('tr', {}, h('td', { colspan: 8, class: 'muted' }, 'ログがありません')))),
    onMore ? h('div', { class: 'mt10' }, onMore) : null);
}

async function showLog(id) {
  const [l, prov] = await Promise.all([guard(() => api('GET', `/logs/${id}`)), api('GET', '/providers').catch(() => ({ data: [] }))]);
  if (!l) return;
  const provName = (prov.data.find((p) => p.id === l.provider_id) || {}).name || l.provider_id || '—';
  const kv = [
    ['日時', fmtTime(l.created_at)], ['キー', l.key_name || '—'], ['エイリアス', l.alias || '—'],
    ['プロバイダ / モデル', `${provName} / ${l.upstream_model || '—'}`],
    ['エンドポイント', l.endpoint], ['形式', `${l.inbound_format} → ${l.upstream_format || '—'}${l.passthrough ? '（パススルー）' : ''}`],
    ['ストリーム', l.stream ? 'はい' : 'いいえ'], ['ステータス', String(l.status_code)],
    ['レイテンシ / TTFT / キュー', `${fmtMs(l.latency_ms)} / ${fmtMs(l.ttft_ms)} / ${fmtMs(l.queue_ms)}`],
    ['トークン(入/出)', l.prompt_tokens !== null ? `${l.prompt_tokens} / ${l.completion_tokens}` : '—'],
    ['タイムアウト段階', l.timeout_phase || '—'], ['中断 / 切断', `${l.interrupted ? '中断' : '—'} / ${l.cancelled_by_client ? 'クライアント切断' : '—'}`],
    ['エラー', l.error || '—'], ['クライアント', `${l.client_ip || '—'}  ${l.user_agent || ''}`],
  ];
  const attempts = (l.attempts || []).length ? h('table', {}, h('thead', {}, h('tr', {}, ['#', 'プロバイダ', 'モデル', '結果', 'ステータス', '所要', 'エラー'].map((t) => h('th', {}, t)))),
    h('tbody', {}, l.attempts.map((a, i) => h('tr', {}, h('td', {}, i + 1), h('td', {}, a.provider), h('td', { class: 'mono small' }, a.upstream_model),
      h('td', {}, badge(a.result, a.result === 'ok' ? 'ok' : 'err')), h('td', {}, a.status || '—'), h('td', {}, fmtMs(a.duration_ms)),
      h('td', { class: 'small' }, a.error || ''))))) : h('p', { class: 'muted' }, 'なし');
  await modal(`ログ #${l.id}`, h('div', {},
    h('dl', { class: 'kv' }, kv.map(([k, v]) => [h('dt', {}, k), h('dd', {}, v)])),
    l.body_truncated ? h('p', { class: 'warn-text' }, '本文はサイズ上限で切り詰められています') : null,
    h('h2', {}, '試行履歴'), attempts,
    h('h2', {}, 'リクエスト'), h('pre', {}, pretty(l.request_body)),
    h('h2', {}, 'レスポンス'), h('pre', {}, pretty(l.response_body)),
    l.upstream_request_body ? [h('h2', {}, '上流へのリクエスト'), h('pre', {}, pretty(l.upstream_request_body))] : null), null, { wide: true });
}

route('logs', async () => {
  const [keys, aliases] = await Promise.all([api('GET', '/keys'), api('GET', '/aliases')]);
  const from = h('input', { type: 'datetime-local' });
  const to = h('input', { type: 'datetime-local' });
  const key = h('select', {}, h('option', { value: '' }, 'すべてのキー'), keys.data.map((k) => h('option', { value: k.id }, k.name)));
  const alias = h('select', {}, h('option', { value: '' }, 'すべてのエイリアス'), aliases.data.map((a) => h('option', { value: a.name }, a.name)));
  const status = h('select', {}, [['', 'すべて'], ['2xx', '2xx'], ['4xx', '4xx'], ['5xx', '5xx'], ['error', 'エラーのみ']].map(([v, l]) => h('option', { value: v }, l)));
  const q = h('input', { placeholder: '本文の部分一致' });
  const result = h('div', {});
  const params = () => {
    const p = new URLSearchParams();
    if (from.value) p.set('from', toISO(from.value));
    if (to.value) p.set('to', toISO(to.value));
    if (key.value) p.set('key_id', key.value);
    if (alias.value) p.set('alias', alias.value);
    if (status.value) p.set('status', status.value);
    if (q.value.trim()) p.set('q', q.value.trim());
    return p;
  };
  let rows = [];
  const load = async (before) => {
    const p = params(); p.set('limit', '100'); if (before) p.set('before_id', before);
    const res = await guard(() => api('GET', '/logs?' + p));
    if (!res) return;
    rows = before ? rows.concat(res.data) : res.data;
    const more = res.data.length === 100 ? h('button', { onclick: () => load(res.next_before_id) }, 'さらに読み込む') : null;
    result.replaceChildren(logTable(rows, more));
  };
  const exportLink = h('a', { class: 'btn', href: '#', onclick: (e) => { e.preventDefault(); location.href = '/admin/api/logs/export?' + params(); } }, 'JSONL エクスポート');
  await load();
  return h('div', {},
    h('h1', {}, 'リクエストログ', h('span', { class: 'spacer' }), exportLink),
    h('form', { class: 'panel row', onsubmit: (e) => { e.preventDefault(); load(); } },
      h('span', { class: 'small muted' }, '期間'), from, '〜', to, key, alias, status, h('div', { class: 'grow' }, q),
      h('button', { class: 'primary', type: 'submit' }, '検索')),
    result);
});

// --------------------------------------------------------------- settings

const settingDefs = [
  ['タイムアウト（全体デフォルト）', [
    ['connect_timeout_ms', '接続（秒）', 'sec'], ['queue_timeout_ms', 'キュー待ち（秒）', 'sec'],
    ['ttft_timeout_ms', '初回トークン TTFT（秒）', 'sec'], ['idle_timeout_ms', '無通信（秒）', 'sec'],
    ['max_duration_ms', '全体上限（秒）', 'sec'], ['keepalive_interval_ms', 'keepalive 間隔（秒）', 'sec'],
    ['fallback_on_ttft_timeout', 'TTFT タイムアウト時にフォールバック', 'bool']]],
  ['ログ', [
    ['log_retention_days', '保持期間（日、0=無期限）', 'int'], ['log_body_max_bytes', '本文サイズ上限（バイト）', 'int'],
    ['log_upstream_request', '上流へのリクエストも保存（デバッグ用）', 'bool']]],
  ['ヘルスチェック', [['health_check_interval_ms', '間隔（秒）', 'sec']]],
];

route('settings', async () => {
  const s = await api('GET', '/settings');
  const inputs = {};
  const sections = settingDefs.map(([title, defs]) => h('div', { class: 'panel' }, h('h2', { class: 'mt0' }, title),
    h('div', { class: 'grid2' }, defs.map(([key, label, kind]) => {
      const v = s.values[key], d = s.defaults[key];
      let inp;
      if (kind === 'bool') inp = h('input', { type: 'checkbox', checked: v });
      else inp = h('input', { type: 'number', min: 0, step: kind === 'sec' ? 'any' : '1', value: kind === 'sec' ? msToSec(v) : v });
      inputs[key] = { inp, kind };
      const hint = `既定: ${kind === 'sec' ? fmtDur(d) : kind === 'bool' ? (d ? 'オン' : 'オフ') : d}`;
      return kind === 'bool' ? h('label', { class: 'check field' }, inp, label, h('span', { class: 'muted small' }, hint)) : field(label, inp, hint);
    }))));
  const save = h('button', { class: 'primary', onclick: () => guard(async () => {
    const body = {};
    for (const [k, { inp, kind }] of Object.entries(inputs)) {
      if (kind === 'bool') body[k] = inp.checked;
      else if (inp.value !== '') body[k] = kind === 'sec' ? secToMs(inp.value) : parseInt(inp.value, 10);
    }
    await api('PATCH', '/settings', body);
    toast('保存しました');
  }) }, '設定を保存');
  const cur = h('input', { type: 'password', autocomplete: 'current-password' });
  const nw = h('input', { type: 'password', autocomplete: 'new-password' });
  const nw2 = h('input', { type: 'password', autocomplete: 'new-password' });
  const pw = state.mode === 'session' ? h('div', { class: 'panel' }, h('h2', { class: 'mt0' }, '管理者パスワード変更'),
    h('div', { class: 'grid2' }, field('現在のパスワード', cur), field('新しいパスワード（12文字以上）', nw), field('新しいパスワード（確認）', nw2)),
    h('button', { onclick: () => guard(async () => {
      if (nw.value !== nw2.value) throw new Error('確認用パスワードが一致しません');
      await api('POST', '/password', { current_password: cur.value, new_password: nw.value });
      cur.value = nw.value = nw2.value = ''; toast('パスワードを変更しました（他のセッションは失効）');
    }) }, '変更')) : h('p', { class: 'muted' }, '信頼ヘッダ経由でログインしているため、パスワード変更はできません。');
  const backup = h('div', { class: 'panel' }, h('h2', { class: 'mt0' }, 'バックアップ'),
    h('p', { class: 'muted small' }, '稼働中のまま、データベース全体（設定・キー・ログ）の一貫したコピーをダウンロードします。CLI では llmgw backup <path>。'),
    h('a', { class: 'btn', href: '/admin/api/backup' }, 'バックアップをダウンロード'));
  return h('div', {}, h('h1', {}, '設定', h('span', { class: 'spacer' }), save), sections, backup, pw);
});

// ------------------------------------------------------------------ audit

route('audit', async () => {
  const result = h('div', {});
  let rows = [];
  const load = async (before) => {
    const res = await api('GET', '/audit-logs?limit=100' + (before ? '&before_id=' + before : ''));
    rows = before ? rows.concat(res.data) : res.data;
    const more = res.data.length === 100 ? h('button', { onclick: () => guard(() => load(rows[rows.length - 1].id)) }, 'さらに読み込む') : null;
    result.replaceChildren(h('table', {}, h('thead', {}, h('tr', {}, ['日時', '操作者', '操作', '対象', '詳細', 'IP'].map((t) => h('th', {}, t)))),
      h('tbody', {}, rows.map((e) => h('tr', {}, h('td', { class: 'small' }, fmtTime(e.created_at)), h('td', {}, e.actor),
        h('td', {}, badge(e.action, e.action.includes('failure') ? 'err' : '')), h('td', { class: 'small' }, e.target_type ? `${e.target_type} #${e.target_id || ''}` : '—'),
        h('td', { class: 'mono small' }, e.detail ? JSON.stringify(e.detail) : ''), h('td', { class: 'small' }, e.client_ip || ''))))), more);
  };
  await load();
  return h('div', {}, h('h1', {}, '監査ログ'), result);
});

// ------------------------------------------------------------------- boot

window.addEventListener('hashchange', render);
document.addEventListener('DOMContentLoaded', async () => {
  document.getElementById('logout').addEventListener('click', async () => {
    await guard(() => api('POST', '/logout')); state.csrf = ''; showLogin();
  });
  try {
    setSession(await api('GET', '/session'));
    render();
  } catch (e) {
    if (!(e instanceof APIError && e.status === 401)) toast(e.message, true);
  }
});

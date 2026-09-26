/*
 * NorthGod Kiro-Go user self-service portal.
 * Read-only view scoped to a single API key (card): balance, usage, records,
 * recharges, sub-cards and integration help. All requests authenticate with
 * `Authorization: Bearer <key>` against the /user/api/* endpoints.
 */
(() => {
  'use strict';

  // ── State ──
  const baseUrl = location.origin;
  const KEY_PAGE_SIZE = 20;
  const REMEMBER_FLAG = 'user_key_remember';
  const STORE_KEY = 'user_api_key';

  let apiKey = sessionStorage.getItem(STORE_KEY) ||
    (localStorage.getItem(REMEMBER_FLAG) === '1' ? (localStorage.getItem(STORE_KEY) || '') : '');
  let currentLang = localStorage.getItem('kiro_lang') || 'zh';
  const dict = { en: null, zh: null };

  // Portal-only i18n additions. Kept inline so we never touch the shared
  // locale files; merged into the runtime dict inside loadLocale().
  const USER_I18N = {
    zh: {
      'user.overview.summaryHint': '积分 / 余额为计费单位，Token 与请求数仅供参考。',
      'user.overview.cacheHitRate': '缓存命中率',
      // Reseller / sub-card self-service portal
      'res.poolTitle': '额度池',
      'res.poolHint': '子卡密从你的可分配额度中划拨；你自己的消耗与已分配额度共享同一预算。',
      'res.budget': '总预算',
      'res.ownUsed': '自己已用',
      'res.allocated': '已分配子卡',
      'res.allocatable': '可分配',
      'res.createTitle': '开设子卡密',
      'res.name': '名称',
      'res.namePh': '例如：客户A / 团队B',
      'res.credit': '额度 (credits)',
      'res.creditPh': '例如 1000',
      'res.days': '有效期 (天)',
      'res.daysPh': '留空 = 跟随本卡到期',
      'res.create': '创建子卡密',
      'res.creating': '创建中…',
      'res.listTitle': '子卡密',
      'res.empty': '还没有子卡密，用上面的表单开一张吧。',
      'res.copyKey': '复制 Key',
      'res.copyUrl': '复制地址',
      'res.edit': '编辑',
      'res.save': '保存',
      'res.cancel': '取消',
      'res.delete': '删除',
      'res.enable': '启用',
      'res.disable': '禁用',
      'res.balance': '余额',
      'res.granted': '额度',
      'res.used': '已用',
      'res.createdAt': '创建于',
      'res.expiresAt': '到期',
      'res.never': '永久',
      'res.nameRequired': '请填写子卡密名称',
      'res.creditRequired': '请填写大于 0 的额度',
      'res.created': '子卡密已创建',
      'res.updated': '已更新',
      'res.deleted': '子卡密已删除',
      'res.confirmDelete': '确认删除子卡密「{0}」？已消耗的额度不退回，此操作不可撤销。',
      'res.allocatableHint': '当前可分配：{0} credits'
    },
    en: {
      'user.overview.summaryHint': 'Credits / balance is the billing unit; tokens and requests are shown for reference.',
      'user.overview.cacheHitRate': 'Cache hit rate',
      'res.poolTitle': 'Credit pool',
      'res.poolHint': 'Sub-cards draw from your allocatable credits; your own usage and allocations share one budget.',
      'res.budget': 'Budget',
      'res.ownUsed': 'Own used',
      'res.allocated': 'Allocated',
      'res.allocatable': 'Allocatable',
      'res.createTitle': 'Create sub-card',
      'res.name': 'Name',
      'res.namePh': 'e.g. Customer A / Team B',
      'res.credit': 'Credits',
      'res.creditPh': 'e.g. 1000',
      'res.days': 'Valid days',
      'res.daysPh': 'blank = follow this card',
      'res.create': 'Create sub-card',
      'res.creating': 'Creating…',
      'res.listTitle': 'Sub-cards',
      'res.empty': 'No sub-cards yet — create one with the form above.',
      'res.copyKey': 'Copy Key',
      'res.copyUrl': 'Copy URL',
      'res.edit': 'Edit',
      'res.save': 'Save',
      'res.cancel': 'Cancel',
      'res.delete': 'Delete',
      'res.enable': 'Enable',
      'res.disable': 'Disable',
      'res.balance': 'Balance',
      'res.granted': 'Quota',
      'res.used': 'Used',
      'res.createdAt': 'Created',
      'res.expiresAt': 'Expires',
      'res.never': 'Never',
      'res.nameRequired': 'Please enter a sub-card name',
      'res.creditRequired': 'Please enter a quota greater than 0',
      'res.created': 'Sub-card created',
      'res.updated': 'Updated',
      'res.deleted': 'Sub-card deleted',
      'res.confirmDelete': 'Delete sub-card "{0}"? Spent credits are not refunded and this cannot be undone.',
      'res.allocatableHint': 'Allocatable now: {0} credits'
    }
  };

  let meData = null;
  let usageData = null;
  let currentTab = 'overview';
  let recordsPage = 1;
  let rechargesPage = 1;
  let usageKeyRevealed = false;
  let loggingIn = false;
  let resellerData = null;   // cached GET /children overview (pool + sub-cards)
  let editingChildId = null; // id of the sub-card currently in inline-edit mode

  // ── DOM helpers ──
  const $ = (id) => document.getElementById(id);
  const qsa = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  function escapeHtml(s) {
    const d = document.createElement('div');
    d.textContent = s == null ? '' : String(s);
    return d.innerHTML;
  }
  function escapeAttr(s) {
    return escapeHtml(s).replace(/"/g, '&quot;');
  }
  async function copyText(text) {
    const str = String(text == null ? '' : text);
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try { await navigator.clipboard.writeText(str); return; } catch (e) { }
    }
    const ta = document.createElement('textarea');
    ta.value = str;
    ta.readOnly = true;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) { }
    document.body.removeChild(ta);
  }

  // ── Toast bridge ──
  const toast = function (msg, variant, opts) {
    if (typeof window.toast === 'function') return window.toast(msg, variant, opts);
    return function () { };
  };

  // ── i18n ──
  async function loadLocale(lang) {
    if (dict[lang]) return dict[lang];
    try {
      const res = await fetch('/admin/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      dict[lang] = await res.json();
    } catch (e) {
      dict[lang] = {};
    }
    // Layer portal-specific keys on top of the shared locale.
    Object.assign(dict[lang], (USER_I18N[lang] || {}));
    return dict[lang];
  }
  function t(key, ...args) {
    const active = dict[currentLang] || {};
    const fallback = dict.zh || {};
    let text = active[key] != null ? active[key] : (fallback[key] != null ? fallback[key] : key);
    args.forEach((arg, idx) => { text = text.replace('{' + idx + '}', arg); });
    return text;
  }
  function applyTranslations() {
    qsa('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
    qsa('[data-i18n-aria-label]').forEach(el => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('user.pageTitle');
    document.documentElement.lang = currentLang;
    updateLangButtons();
    applyTheme(getThemePref());
  }
  async function setLang(lang) {
    currentLang = lang;
    localStorage.setItem('kiro_lang', lang);
    await loadLocale(lang);
    applyTranslations();
    if (!$('userMainPage').classList.contains('hidden')) {
      renderStats();
      renderCurrentTab();
    }
  }
  function updateLangButtons() {
    qsa('.lang-btn').forEach(btn => btn.classList.toggle('active', btn.dataset.lang === currentLang));
  }
  function toggleLang() { setLang(currentLang === 'zh' ? 'en' : 'zh'); }

  // ── Theme (mirrors admin behaviour, shared localStorage keys) ──
  const THEME_ORDER = ['system', 'light', 'dark'];
  const themeMQ = window.matchMedia('(prefers-color-scheme: dark)');
  function resolveTheme(pref) {
    if (pref === 'dark') return 'dark';
    if (pref === 'light') return 'light';
    return themeMQ.matches ? 'dark' : 'light';
  }
  function applyTheme(pref) {
    const resolved = resolveTheme(pref);
    const root = document.documentElement;
    root.classList.toggle('dark', resolved === 'dark');
    root.dataset.themePref = pref;
    qsa('.theme-toggle').forEach(btn => {
      btn.dataset.theme = pref;
      const label = t('theme.status', t('theme.' + pref));
      btn.setAttribute('aria-label', label);
      btn.setAttribute('title', label);
    });
  }
  function getThemePref() {
    const saved = localStorage.getItem('kiro_theme');
    return THEME_ORDER.includes(saved) ? saved : 'system';
  }
  function initTheme() {
    applyTheme(getThemePref());
    themeMQ.addEventListener('change', () => { if (getThemePref() === 'system') applyTheme('system'); });
  }
  function toggleTheme() {
    const cur = getThemePref();
    const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
    localStorage.setItem('kiro_theme', next);
    applyTheme(next);
  }

  // ── Formatting ──
  function formatNumber(n) {
    if (n == null || isNaN(n)) return '0';
    if (Math.abs(n) >= 1 && Math.floor(n) === n) return Number(n).toLocaleString('en-US');
    return Number(n).toLocaleString('en-US', { maximumFractionDigits: 1 });
  }
  // Credits for a SINGLE request are routinely below 0.05 — a 6.8K-token opus-5
  // call costs about 0.028 — and formatNumber's one-decimal cap rendered those as
  // "0", so a customer reading their own statement saw a page of apparently free
  // requests. Scale precision to magnitude and never round a real charge down to
  // zero. Aggregates (balance / granted) deliberately keep formatNumber: they are
  // large, whole-ish top-up figures where extra decimals only add noise.
  function formatCredits(v) {
    const n = Number(v);
    if (!isFinite(n) || n === 0) return '0';
    const abs = Math.abs(n);
    if (abs >= 100) return n.toFixed(1);
    if (abs >= 1) return n.toFixed(2);
    if (abs >= 0.01) return n.toFixed(3);
    if (abs >= 0.0001) return n.toFixed(4);
    return n.toExponential(1); // vanishingly small, but never rendered as zero
  }
  function formatCompact(n) {
    n = Number(n) || 0;
    if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (Math.abs(n) >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return formatNumber(n);
  }
  function formatDateTime(ts) {
    if (!ts) return '-';
    try { return new Date(ts * 1000).toLocaleString(); } catch (e) { return '-'; }
  }
  function maskKey(key) {
    const s = String(key || '');
    if (s.length <= 12) return '\u2022\u2022\u2022\u2022\u2022\u2022';
    return s.slice(0, 7) + '\u2022\u2022\u2022\u2022\u2022\u2022' + s.slice(-4);
  }
  const INFINITY = '\u221e';

  // ── Session / auth ──
  function persistKey(key, remember) {
    apiKey = key;
    sessionStorage.setItem(STORE_KEY, key);
    if (remember) {
      localStorage.setItem(STORE_KEY, key);
      localStorage.setItem(REMEMBER_FLAG, '1');
    } else {
      localStorage.removeItem(STORE_KEY);
      localStorage.removeItem(REMEMBER_FLAG);
    }
  }
  function clearKey() {
    apiKey = '';
    sessionStorage.removeItem(STORE_KEY);
    localStorage.removeItem(STORE_KEY);
    localStorage.removeItem(REMEMBER_FLAG);
  }
  function showLogin() {
    $('userMainPage').classList.add('hidden');
    $('userLoginPage').classList.remove('hidden');
  }
  function showMain() {
    $('userLoginPage').classList.add('hidden');
    $('userMainPage').classList.remove('hidden');
    // Fade in the panel that is active on entry (Overview by default).
    fadeInPanel($('userTab' + currentTab.charAt(0).toUpperCase() + currentTab.slice(1)));
  }
  function handleUnauthorized() {
    clearKey();
    meData = null;
    usageData = null;
    showLogin();
    const field = $('userKeyField');
    if (field) field.value = '';
    toast(t('user.login.sessionExpired'), 'warning');
  }

  // Authenticated GET. Returns { ok, status, data }. On 401 triggers re-login.
  async function apiGet(path) {
    let res;
    try {
      res = await fetch('/user/api' + path, {
        headers: { 'Authorization': 'Bearer ' + apiKey },
        cache: 'no-store'
      });
    } catch (e) {
      return { ok: false, status: 0, data: null, network: true };
    }
    if (res.status === 401) {
      handleUnauthorized();
      return { ok: false, status: 401, data: null };
    }
    let data = null;
    try { data = await res.json(); } catch (e) { }
    return { ok: res.ok, status: res.status, data };
  }

  // Authenticated write (POST/PUT/DELETE). Returns { ok, status, data }. On 401
  // triggers re-login, mirroring apiGet.
  async function apiSend(method, path, body) {
    let res;
    try {
      res = await fetch('/user/api' + path, {
        method: method,
        headers: { 'Authorization': 'Bearer ' + apiKey, 'Content-Type': 'application/json' },
        body: body != null ? JSON.stringify(body) : undefined,
        cache: 'no-store'
      });
    } catch (e) {
      return { ok: false, status: 0, data: null, network: true };
    }
    if (res.status === 401) {
      handleUnauthorized();
      return { ok: false, status: 401, data: null };
    }
    let data = null;
    try { data = await res.json(); } catch (e) { }
    return { ok: res.ok, status: res.status, data };
  }

  // ── Styled confirm dialog (replaces the native window.confirm) ──
  let confirmResolveU = null;
  function closeConfirmU(value) {
    if (!confirmResolveU) return;
    const resolve = confirmResolveU;
    confirmResolveU = null;
    const modal = $('confirmModal');
    if (modal) { modal.classList.remove('active'); modal.onkeydown = null; }
    resolve(!!value);
  }
  // Returns a Promise<boolean>. opts: { title, confirmText, cancelText, variant:'danger' }.
  function confirmDialog(message, opts) {
    opts = opts || {};
    const modal = $('confirmModal'), title = $('confirmTitle'), msg = $('confirmMessage');
    const ok = $('confirmOk'), cancel = $('confirmCancel'), close = $('confirmClose');
    if (!modal || !title || !msg || !ok || !cancel || !close) {
      return Promise.resolve(window.confirm(message || ''));
    }
    if (confirmResolveU) closeConfirmU(false);
    title.textContent = opts.title || t('common.confirm');
    msg.textContent = message || '';
    ok.textContent = opts.confirmText || t('common.confirm');
    cancel.textContent = opts.cancelText || t('common.cancel');
    ok.className = 'btn ' + (opts.variant === 'danger' ? 'btn-danger' : 'btn-primary');
    cancel.className = 'btn btn-secondary';
    ok.onclick = () => closeConfirmU(true);
    cancel.onclick = () => closeConfirmU(false);
    close.onclick = () => closeConfirmU(false);
    modal.onclick = (e) => { if (e.target === modal) closeConfirmU(false); };
    modal.onkeydown = (e) => { if (e.key === 'Escape') closeConfirmU(false); };
    const pending = new Promise(resolve => { confirmResolveU = resolve; });
    modal.classList.add('active');
    setTimeout(() => { try { ok.focus({ preventScroll: true }); } catch (e) { } }, 0);
    return pending;
  }

  async function login() {
    if (loggingIn) return;
    const field = $('userKeyField');
    const key = (field.value || '').trim();
    if (!key) { toast(t('user.login.empty'), 'warning'); return; }
    loggingIn = true;
    const btn = $('userLoginBtn');
    const label = btn ? btn.querySelector('span') : null;
    if (btn) btn.disabled = true;
    if (label) label.textContent = t('user.login.querying');
    try {
      const res = await fetch('/user/api/me', {
        headers: { 'Authorization': 'Bearer ' + key },
        cache: 'no-store'
      });
      if (res.ok) {
        const remember = $('userRemember');
        persistKey(key, !!(remember && remember.checked));
        meData = await res.json().catch(() => ({}));
        showMain();
        applyKeyState();
        await loadAll(true);
      } else if (res.status === 401 || res.status === 403) {
        toast(t('user.login.invalid'), 'error');
      } else {
        toast(t('user.login.invalid'), 'error');
      }
    } catch (e) {
      toast(t('user.login.connectError'), 'error');
    } finally {
      loggingIn = false;
      if (btn) btn.disabled = false;
      if (label) label.textContent = t('user.login.submit');
    }
  }

  async function tryAutoLogin() {
    if (!apiKey) { showLogin(); return; }
    const res = await apiGet('/me');
    if (res.ok && res.data) {
      meData = res.data;
      showMain();
      applyKeyState();
      await loadAll(true);
    } else if (res.status !== 401) {
      // Transient error: keep the stored key, prefill and let the user retry.
      const field = $('userKeyField');
      if (field && apiKey) field.value = apiKey;
      showLogin();
    }
    // On 401 handleUnauthorized already switched to login.
  }

  function logout() {
    clearKey();
    meData = null;
    usageData = null;
    location.reload();
  }

  // Toggle the sub-cards tab based on whether this key can manage sub-cards
  // (a budgeted card that is not itself a sub-card). Budgeted cards see the tab
  // even before they have opened any sub-card, so they can create the first one.
  function applyKeyState() {
    const canResell = !!(meData && meData.canManageSubKeys);
    const tab = $('userChildrenTab');
    if (tab) tab.classList.toggle('hidden', !canResell);
    if (!canResell && currentTab === 'children') switchTab('overview');
  }

  // ── Data loading ──
  async function loadAll(resetTab) {
    if (resetTab) {
      recordsPage = 1;
      rechargesPage = 1;
    }
    const [meRes, usageRes] = await Promise.all([apiGet('/me'), apiGet('/usage')]);
    if (meRes.ok && meRes.data) meData = meRes.data;
    if (usageRes.ok && usageRes.data) usageData = usageRes.data;
    else if (usageRes.status === 401) return; // re-login already triggered
    applyKeyState();
    renderStats();
    renderCurrentTab();
  }

  // ── Derived helpers ──
  function grantedOf() {
    if (!meData) return 0;
    if (meData.creditsGranted != null && meData.creditsGranted > 0) return meData.creditsGranted;
    if (usageData && usageData.creditsGranted != null && usageData.creditsGranted > 0) return usageData.creditsGranted;
    if (meData.creditLimit != null && meData.creditLimit > 0) return meData.creditLimit;
    return 0;
  }
  function usedOf() {
    if (usageData && usageData.creditsUsed != null) return usageData.creditsUsed;
    if (meData && meData.creditsUsed != null) return meData.creditsUsed;
    return 0;
  }
  function balanceOf() {
    if (usageData && usageData.balance != null) return usageData.balance;
    if (meData && meData.balance != null) return meData.balance;
    const g = grantedOf();
    return g > 0 ? g - usedOf() : 0;
  }
  function tokensOf() {
    if (usageData && usageData.tokensUsed != null) return usageData.tokensUsed;
    return (meData && meData.tokensUsed) || 0;
  }
  function requestsOf() {
    if (usageData && usageData.requestsCount != null) return usageData.requestsCount;
    return (meData && meData.requestsCount) || 0;
  }
  function usedPct() {
    const g = grantedOf();
    if (g <= 0) return 0;
    return Math.max(0, Math.min(100, (usedOf() / g) * 100));
  }
  function statusInfo() {
    if (meData && meData.expired) return { key: 'user.status.expired', cls: 'badge-error' };
    if (meData && meData.enabled === false) return { key: 'user.status.disabled', cls: 'badge-disabled' };
    return { key: 'user.status.active', cls: 'badge-success' };
  }

  // ── Stats grid (always visible) ──
  function renderStats() {
    const g = grantedOf();
    $('upStatBalance').textContent = g > 0 ? formatNumber(balanceOf()) : INFINITY;
    $('upStatGranted').textContent = g > 0 ? formatNumber(g) : t('user.overview.unlimited');
    // "Used" starts out as a fraction on a fresh card, where formatNumber shows 0.
    $('upStatUsed').textContent = formatCredits(usedOf());
    $('upStatTokens').textContent = formatCompact(tokensOf());
    $('upStatRequests').textContent = formatNumber(requestsOf());
  }

  // ── Overview tab ──
  function summaryItem(label, value, variant) {
    return '<div class="key-summary-item' + (variant ? ' key-summary-item--' + variant : '') + '">' +
      '<div class="key-summary-value">' + escapeHtml(String(value)) + '</div>' +
      '<div class="key-summary-label">' + escapeHtml(label) + '</div>' +
      '</div>';
  }
  function infoItem(label, value) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div>' +
      '<div class="detail-value">' + escapeHtml(value) + '</div></div>';
  }
  // Prompt-cache hit-rate tile: colored % (green >=60 / amber >=30 / red below),
  // or a dash when there's no input yet. Mirrors the plugin panel's coloring.
  function cacheHitItem() {
    const hr = (usageData && typeof usageData.cacheHitRate === 'number') ? usageData.cacheHitRate : null;
    const txt = hr == null ? '\u2014' : (hr * 100).toFixed(1) + '%';
    const color = hr == null ? '' : (hr >= 0.6 ? '#16a34a' : hr >= 0.3 ? '#ca8a04' : '#dc2626');
    const style = color ? ' style="color:' + color + '"' : '';
    return '<div class="key-summary-item"><div class="key-summary-value"' + style + '>' + escapeHtml(txt) + '</div>' +
      '<div class="key-summary-label">' + escapeHtml(t('user.overview.cacheHitRate')) + '</div></div>';
  }
  function renderOverview() {
    const c = $('upOverview');
    if (!c) return;
    if (!meData) {
      c.innerHTML = '<div class="up-loading"><i class="fa-solid fa-spinner fa-spin"></i> ' + escapeHtml(t('detail.loading')) + '</div>';
      return;
    }
    const g = grantedOf();
    const pct = usedPct();
    const st = statusInfo();
    const pctClass = pct > 90 ? 'critical' : pct > 70 ? 'high' : '';
    const parentBadge = meData.isParent
      ? ' <span class="badge badge-info">' + escapeHtml(t('user.overview.parentBadge')) + '</span>'
      : '';

    // Balance card with progress
    let balanceCard =
      '<div class="card up-balance-card">' +
      '<div class="card-header"><span class="card-title"><i class="fa-solid fa-wallet card-title-icon"></i>' +
      escapeHtml(t('user.overview.balanceTitle')) + '</span>' +
      '<span class="badge ' + st.cls + '">' + escapeHtml(t(st.key)) + '</span></div>' +
      '<div class="up-balance-figure">' +
      '<span class="up-balance-value">' + (g > 0 ? escapeHtml(formatNumber(balanceOf())) : INFINITY) + '</span>' +
      '<span class="up-balance-suffix">' + escapeHtml(t('user.overview.balance')) + parentBadge + '</span>' +
      '</div>';
    if (g > 0) {
      balanceCard +=
        '<div class="usage-bar"><div class="usage-fill ' + pctClass + '" style="width:' + pct.toFixed(1) + '%"></div></div>' +
        '<div class="up-balance-sub"><span>' + escapeHtml(t('user.overview.used')) + ': ' + escapeHtml(formatNumber(usedOf())) +
        ' / ' + escapeHtml(formatNumber(g)) + '</span><span>' + escapeHtml(t('user.overview.usedPct', pct.toFixed(1))) + '</span></div>';
    } else {
      balanceCard += '<div class="up-balance-sub"><span>' + escapeHtml(t('user.overview.granted')) + ': ' +
        escapeHtml(t('user.overview.unlimited')) + '</span><span>' + escapeHtml(t('user.overview.used')) + ': ' +
        escapeHtml(formatNumber(usedOf())) + '</span></div>';
    }
    balanceCard += '</div>';

    // Key info card
    const expiresText = meData.expiresAt ? formatDateTime(meData.expiresAt) : t('user.overview.neverExpires');
    // Tri-state: null/absent = inherit system default, 0 = unlimited, N = value.
    const limitText = (v) => v == null ? t('user.overview.concurrencyDefault')
      : (v === 0 ? t('user.overview.unlimited') : String(v));
    const concurrencyText = limitText(meData.maxConcurrency);
    const infoCard =
      '<div class="card">' +
      '<div class="card-header"><span class="card-title"><i class="fa-solid fa-id-card card-title-icon"></i>' +
      escapeHtml(t('user.overview.keyInfoTitle')) + '</span></div>' +
      '<div class="detail-grid">' +
      infoItem(t('user.overview.name'), meData.name || '-') +
      infoItem(t('user.overview.status'), t(st.key)) +
      infoItem(t('user.overview.expiresAt'), expiresText) +
      infoItem(t('user.overview.maxConcurrency'), concurrencyText) +
      infoItem(t('user.overview.createdAt'), meData.createdAt ? formatDateTime(meData.createdAt) : '-') +
      infoItem(t('user.overview.lastUsedAt'), meData.lastUsedAt ? formatDateTime(meData.lastUsedAt) : '-') +
      '</div></div>';

    // Usage summary card (tokens / requests / by model)
    const byModel = (usageData && Array.isArray(usageData.byModel)) ? usageData.byModel : [];
    let usageCard =
      '<div class="card">' +
      '<div class="card-header"><span class="card-title"><i class="fa-solid fa-chart-simple card-title-icon"></i>' +
      escapeHtml(t('user.overview.usageTitle')) + '</span></div>' +
      '<p class="card-subtitle">' + escapeHtml(t('user.overview.summaryHint')) + '</p>' +
      '<div class="ng-grid ng-grid-4 up-usage-metrics">' +
      summaryItem(t('user.overview.balance'), g > 0 ? formatNumber(balanceOf()) : INFINITY, 'balance') +
      summaryItem(t('user.overview.used'), formatCredits(usedOf()), 'used') +
      summaryItem(t('user.overview.tokens'), formatNumber(tokensOf()), '') +
      summaryItem(t('user.overview.requests'), formatNumber(requestsOf()), '') +
      cacheHitItem() +
      '</div>';
    usageCard += '<div class="key-detail-subheading">' + escapeHtml(t('user.overview.byModel')) + '</div>';
    if (byModel.length) {
      usageCard += '<table class="data-table"><thead><tr>' +
        '<th>' + escapeHtml(t('user.model')) + '</th>' +
        '<th class="ta-right">' + escapeHtml(t('user.credits')) + '</th>' +
        '<th class="ta-right">' + escapeHtml(t('user.overview.requests')) + '</th>' +
        '</tr></thead><tbody>';
      byModel.forEach(m => {
        usageCard += '<tr>' +
          '<td class="font-mono">' + escapeHtml(m.model || '-') + '</td>' +
          '<td class="ta-right">' + escapeHtml(formatCredits(m.credits)) + '</td>' +
          '<td class="ta-right">' + escapeHtml(formatNumber(m.requests || 0)) + '</td>' +
          '</tr>';
      });
      usageCard += '</tbody></table>';
    } else {
      usageCard += '<div class="empty-state">' + escapeHtml(t('user.overview.noModelData')) + '</div>';
    }
    usageCard += '</div>';

    c.innerHTML =
      '<div class="up-section-gap">' +
      '<div class="ng-grid ng-grid-2">' + balanceCard + infoCard + '</div>' +
      usageCard +
      '</div>';
  }

  // ── Paged tables ──
  function loadingBlock() {
    return '<div class="up-loading"><i class="fa-solid fa-spinner fa-spin"></i> ' + escapeHtml(t('detail.loading')) + '</div>';
  }
  function unavailableBlock() {
    return '<div class="empty-state key-detail-unavailable"><i class="fa-solid fa-plug-circle-xmark"></i>' +
      '<span>' + escapeHtml(t('user.notAvailable')) + '</span></div>';
  }
  function failedBlock() {
    return '<div class="empty-state key-detail-unavailable"><i class="fa-solid fa-triangle-exclamation"></i>' +
      '<span>' + escapeHtml(t('user.loadFailed')) + '</span></div>';
  }
  function pagerHtml(kind, page, total) {
    const pages = Math.max(1, Math.ceil((total || 0) / KEY_PAGE_SIZE));
    return '<div class="data-pager">' +
      '<span class="data-pager-total">' + escapeHtml(t('page.total', total || 0)) + '</span>' +
      '<div class="data-pager-ctrl">' +
      '<button class="btn btn-outline btn-xs" type="button" data-pager="' + kind + '" data-dir="-1"' + (page <= 1 ? ' disabled' : '') + '>' + escapeHtml(t('page.prev')) + '</button>' +
      '<span class="data-pager-info">' + escapeHtml(t('page.info', page, pages)) + '</span>' +
      '<button class="btn btn-outline btn-xs" type="button" data-pager="' + kind + '" data-dir="1"' + (page >= pages ? ' disabled' : '') + '>' + escapeHtml(t('page.next')) + '</button>' +
      '</div></div>';
  }

  async function renderRecords() {
    const c = $('upRecords');
    if (!c) return;
    c.innerHTML = loadingBlock();
    const res = await apiGet('/usage/records?page=' + recordsPage + '&pageSize=' + KEY_PAGE_SIZE);
    if (res.status === 401) return;
    if (res.status === 404) { c.innerHTML = unavailableBlock(); return; }
    if (!res.ok || !res.data) { c.innerHTML = failedBlock(); return; }
    const records = Array.isArray(res.data.records) ? res.data.records : [];
    const total = res.data.total != null ? res.data.total : records.length;
    if (!records.length) { c.innerHTML = '<div class="empty-state">' + escapeHtml(t('user.empty')) + '</div>'; return; }
    let html = '<table class="data-table"><thead><tr>' +
      '<th>' + escapeHtml(t('usageRec.time')) + '</th>' +
      '<th>' + escapeHtml(t('usageRec.model')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.input')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.cacheRead')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.output')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.credits')) + '</th>' +
      '</tr></thead><tbody>';
    records.forEach(r => {
      html += '<tr>' +
        '<td>' + escapeHtml(formatDateTime(r.createdAt)) + '</td>' +
        '<td class="font-mono">' + escapeHtml(r.model || '-') + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.inputTokens || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.cacheReadInputTokens || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.outputTokens || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatCredits(r.credits)) + '</td>' +
        '</tr>';
    });
    html += '</tbody></table>' + pagerHtml('records', recordsPage, total);
    c.innerHTML = html;
  }

  async function renderRecharges() {
    const c = $('upRecharges');
    if (!c) return;
    c.innerHTML = loadingBlock();
    const res = await apiGet('/recharges?page=' + rechargesPage + '&pageSize=' + KEY_PAGE_SIZE);
    if (res.status === 401) return;
    if (res.status === 404) { c.innerHTML = unavailableBlock(); return; }
    if (!res.ok || !res.data) { c.innerHTML = failedBlock(); return; }
    const records = Array.isArray(res.data.records) ? res.data.records : [];
    const total = res.data.total != null ? res.data.total : records.length;
    if (!records.length) { c.innerHTML = '<div class="empty-state">' + escapeHtml(t('user.empty')) + '</div>'; return; }
    let html = '<table class="data-table"><thead><tr>' +
      '<th>' + escapeHtml(t('recharge.time')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('recharge.amount')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('recharge.balanceAfter')) + '</th>' +
      '<th>' + escapeHtml(t('recharge.note')) + '</th>' +
      '</tr></thead><tbody>';
    records.forEach(r => {
      html += '<tr>' +
        '<td>' + escapeHtml(formatDateTime(r.createdAt)) + '</td>' +
        '<td class="ta-right data-pos">+' + escapeHtml(formatNumber(r.amount || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.balanceAfter || 0)) + '</td>' +
        '<td class="data-note">' + escapeHtml(r.note || '-') + '</td>' +
        '</tr>';
    });
    html += '</tbody></table>' + pagerHtml('recharges', rechargesPage, total);
    c.innerHTML = html;
  }

  async function renderChildren() {
    const c = $('upChildren');
    if (!c) return;
    if (!meData || !meData.canManageSubKeys) {
      c.innerHTML = '<div class="empty-state">' + escapeHtml(t('user.children.onlyParent')) + '</div>';
      return;
    }
    if (!resellerData) c.innerHTML = loadingBlock();
    const res = await apiGet('/children');
    if (res.status === 401) return;
    if (res.status === 404) { c.innerHTML = unavailableBlock(); return; }
    if (!res.ok || !res.data) { c.innerHTML = failedBlock(); return; }
    resellerData = res.data;
    c.innerHTML = childrenHtml(resellerData);
    wireChildrenEvents(c);
  }

  // A single value/label metric for the portal (mirrors admin ngMetric).
  function ngMetricU(val, label, valCls) {
    return '<div class="ng-metric"><span class="ng-metric-val' + (valCls ? ' ' + valCls : '') + '">' +
      escapeHtml(String(val)) + '</span><span class="ng-metric-label">' + escapeHtml(label) + '</span></div>';
  }

  // Full reseller panel: credit pool summary + create form + sub-card list.
  function childrenHtml(data) {
    const budget = data.budget || 0;
    const allocatable = data.allocatable || 0;
    const children = Array.isArray(data.children) ? data.children : [];
    const pool =
      '<div class="card">' +
      '<div class="card-header"><span class="card-title"><i class="fa-solid fa-sitemap card-title-icon"></i>' + escapeHtml(t('res.poolTitle')) + '</span></div>' +
      '<p class="card-subtitle">' + escapeHtml(t('res.poolHint')) + '</p>' +
      '<div class="key-summary-grid res-pool-grid">' +
        summaryItem(t('res.budget'), budget > 0 ? formatNumber(budget) : INFINITY) +
        summaryItem(t('res.ownUsed'), formatNumber(data.ownUsed || 0), 'used') +
        summaryItem(t('res.allocated'), formatNumber(data.allocated || 0)) +
        summaryItem(t('res.allocatable'), budget > 0 ? formatNumber(allocatable) : INFINITY, 'balance') +
      '</div></div>';

    const form =
      '<div class="card">' +
      '<div class="card-header"><span class="card-title"><i class="fa-solid fa-plus card-title-icon"></i>' + escapeHtml(t('res.createTitle')) + '</span></div>' +
      '<div class="res-create-grid">' +
        '<div class="form-group"><label>' + escapeHtml(t('res.name')) + '</label><input type="text" id="resName" autocomplete="off" placeholder="' + escapeAttr(t('res.namePh')) + '" /></div>' +
        '<div class="form-group"><label>' + escapeHtml(t('res.credit')) + '</label><input type="number" id="resCredit" min="0" step="any" placeholder="' + escapeAttr(t('res.creditPh')) + '" /></div>' +
        '<div class="form-group"><label>' + escapeHtml(t('res.days')) + '</label><input type="number" id="resDays" min="0" step="1" placeholder="' + escapeAttr(t('res.daysPh')) + '" /></div>' +
        '<button class="btn btn-primary res-create-btn" type="button" id="resCreateBtn"><i class="fa-solid fa-plus"></i><span>' + escapeHtml(t('res.create')) + '</span></button>' +
      '</div>' +
      '<small class="res-alloc-hint"><i class="fa-solid fa-circle-info"></i> ' + escapeHtml(t('res.allocatableHint', budget > 0 ? formatNumber(allocatable) : INFINITY)) + '</small>' +
      '</div>';

    let list = '<div class="card"><div class="card-header"><span class="card-title"><i class="fa-solid fa-code-branch card-title-icon"></i>' +
      escapeHtml(t('res.listTitle')) + ' (' + children.length + ')</span></div>';
    if (!children.length) {
      list += '<div class="empty-state">' + escapeHtml(t('res.empty')) + '</div>';
    } else {
      list += '<div class="res-child-list">' + children.map(childRowHtml).join('') + '</div>';
    }
    list += '</div>';
    return '<div class="up-section-stack">' + pool + form + list + '</div>';
  }

  function childRowHtml(ch) {
    const id = escapeAttr(ch.id);
    const g = ch.creditsGranted || 0;
    const used = ch.creditsUsed || 0;
    const bal = ch.balance != null ? ch.balance : (g - used);
    const status = ch.status || (ch.enabled === false ? 'disabled' : 'active');
    const stCls = status === 'expired' ? 'badge-error' : (status === 'disabled' ? 'badge-disabled' : 'badge-success');
    const stKey = status === 'expired' ? 'user.status.expired' : (status === 'disabled' ? 'user.status.disabled' : 'user.status.active');
    const dotCls = status === 'expired' ? 'is-bad' : (ch.enabled === false ? 'is-warn' : 'is-ok');

    // Inline edit state for this row.
    if (String(ch.id) === String(editingChildId)) {
      return '<div class="res-child res-child--editing" data-child-id="' + id + '">' +
        '<div class="res-edit-grid">' +
          '<div class="form-group"><label>' + escapeHtml(t('res.name')) + '</label><input type="text" id="resEditName" value="' + escapeAttr(ch.name || '') + '" /></div>' +
          '<div class="form-group"><label>' + escapeHtml(t('res.credit')) + '</label><input type="number" id="resEditCredit" min="0" step="any" value="' + escapeAttr(g) + '" /></div>' +
        '</div>' +
        '<div class="res-child-actions">' +
          '<button class="btn btn-primary btn-sm" type="button" data-res-act="save" data-id="' + id + '">' + escapeHtml(t('res.save')) + '</button>' +
          '<button class="btn btn-outline btn-sm" type="button" data-res-act="cancelEdit" data-id="' + id + '">' + escapeHtml(t('res.cancel')) + '</button>' +
        '</div></div>';
    }

    return '<div class="res-child" data-child-id="' + id + '">' +
      '<div class="res-child-main">' +
        '<div class="res-child-head">' +
          '<span class="ng-status-dot ' + dotCls + '" aria-hidden="true"></span>' +
          '<span class="res-child-name">' + escapeHtml(ch.name || t('keys.unnamed')) + '</span>' +
          '<span class="badge ' + stCls + '">' + escapeHtml(t(stKey)) + '</span>' +
        '</div>' +
        '<div class="res-child-key"><code>' + escapeHtml(ch.key || '') + '</code>' +
          '<button class="btn btn-outline btn-xs" type="button" data-res-act="copyKey" data-id="' + id + '"><i class="fa-regular fa-copy"></i>' + escapeHtml(t('res.copyKey')) + '</button>' +
          '<button class="btn btn-outline btn-xs" type="button" data-res-act="copyUrl" data-id="' + id + '"><i class="fa-solid fa-link"></i>' + escapeHtml(t('res.copyUrl')) + '</button>' +
        '</div>' +
        '<div class="res-child-meta"><span><i class="fa-regular fa-clock"></i> ' + escapeHtml(t('res.expiresAt') + ': ' + (ch.expiresAt ? formatDateTime(ch.expiresAt) : t('res.never'))) + '</span></div>' +
      '</div>' +
      '<div class="ng-row-blocks"><div class="ng-block ng-block--credit">' +
        ngMetricU(g > 0 ? formatNumber(bal) : INFINITY, t('res.balance'), bal >= 0 ? 'success-text' : '') +
        ngMetricU(g > 0 ? formatNumber(g) : t('user.overview.unlimited'), t('res.granted')) +
        ngMetricU(formatNumber(used), t('res.used')) +
      '</div></div>' +
      '<div class="res-child-actions">' +
        '<label class="switch" title="' + escapeAttr(ch.enabled ? t('res.disable') : t('res.enable')) + '"><input type="checkbox" data-res-act="toggle" data-id="' + id + '"' + (ch.enabled ? ' checked' : '') + ' /><span class="slider"></span></label>' +
        '<button class="btn btn-outline btn-sm" type="button" data-res-act="edit" data-id="' + id + '">' + escapeHtml(t('res.edit')) + '</button>' +
        '<button class="btn btn-danger btn-sm" type="button" data-res-act="delete" data-id="' + id + '">' + escapeHtml(t('res.delete')) + '</button>' +
      '</div>' +
    '</div>';
  }

  function findChild(id) {
    if (!resellerData || !Array.isArray(resellerData.children)) return null;
    return resellerData.children.find(x => String(x.id) === String(id)) || null;
  }

  function rerenderChildrenFromCache() {
    const c = $('upChildren');
    if (!c || !resellerData) return;
    c.innerHTML = childrenHtml(resellerData);
    wireChildrenEvents(c);
  }

  function wireChildrenEvents(c) {
    const createBtn = $('resCreateBtn');
    if (createBtn) createBtn.addEventListener('click', createChild);
    c.querySelectorAll('[data-res-act]').forEach(el => {
      const act = el.dataset.resAct;
      const id = el.dataset.id;
      if (act === 'toggle') el.addEventListener('change', () => toggleChild(id, el.checked));
      else el.addEventListener('click', () => handleChildAction(act, id));
    });
  }

  async function handleChildAction(act, id) {
    if (act === 'copyKey') {
      const ch = findChild(id);
      if (ch && ch.key) { await copyText(ch.key); toast(t('common.copied'), 'primary'); }
    } else if (act === 'copyUrl') {
      await copyText(baseUrl); toast(t('common.copied'), 'primary');
    } else if (act === 'edit') {
      editingChildId = id; rerenderChildrenFromCache();
    } else if (act === 'cancelEdit') {
      editingChildId = null; rerenderChildrenFromCache();
    } else if (act === 'save') {
      await saveEditChild(id);
    } else if (act === 'delete') {
      await deleteChild(id);
    }
  }

  async function createChild() {
    const name = (($('resName') && $('resName').value) || '').trim();
    const credit = parseFloat($('resCredit') && $('resCredit').value);
    const days = parseFloat($('resDays') && $('resDays').value);
    if (!name) { toast(t('res.nameRequired'), 'warning'); return; }
    if (isNaN(credit) || credit <= 0) { toast(t('res.creditRequired'), 'warning'); return; }
    const btn = $('resCreateBtn');
    if (btn) { btn.disabled = true; const s = btn.querySelector('span'); if (s) s.textContent = t('res.creating'); }
    const body = { name: name, creditLimit: credit };
    if (!isNaN(days) && days > 0) body.durationDays = days;
    const res = await apiSend('POST', '/children', body);
    if (res.status === 401) return;
    if (res.ok && res.data && res.data.success) {
      toast(t('res.created'), 'success');
      await loadAll(false);
    } else {
      toast((res.data && res.data.error) || t('user.loadFailed'), 'error');
      if (btn) { btn.disabled = false; const s = btn.querySelector('span'); if (s) s.textContent = t('res.create'); }
    }
  }

  async function toggleChild(id, enabled) {
    const res = await apiSend('PUT', '/children/' + encodeURIComponent(id), { enabled: enabled });
    if (res.status === 401) return;
    if (!res.ok) toast((res.data && res.data.error) || t('user.loadFailed'), 'error');
    await loadAll(false);
  }

  async function saveEditChild(id) {
    const name = (($('resEditName') && $('resEditName').value) || '').trim();
    const credit = parseFloat($('resEditCredit') && $('resEditCredit').value);
    const body = {};
    if (name) body.name = name;
    if (!isNaN(credit) && credit > 0) body.creditLimit = credit;
    const res = await apiSend('PUT', '/children/' + encodeURIComponent(id), body);
    if (res.status === 401) return;
    if (res.ok) { editingChildId = null; toast(t('res.updated'), 'success'); await loadAll(false); }
    else { toast((res.data && res.data.error) || t('user.loadFailed'), 'error'); }
  }

  async function deleteChild(id) {
    const ch = findChild(id);
    const okc = await confirmDialog(t('res.confirmDelete', ch ? (ch.name || '') : ''), { title: t('res.delete'), confirmText: t('res.delete'), variant: 'danger' });
    if (!okc) return;
    const res = await apiSend('DELETE', '/children/' + encodeURIComponent(id));
    if (res.status === 401) return;
    if (res.ok) { toast(t('res.deleted'), 'success'); await loadAll(false); }
    else { toast((res.data && res.data.error) || t('user.loadFailed'), 'error'); }
  }

  // ── How to use tab ──
  function endpointRow(icon, title, url) {
    return '<section class="api-endpoint-item">' +
      '<div class="api-endpoint-meta">' +
      '<span class="api-endpoint-icon"><i class="' + icon + '"></i></span>' +
      '<div><p class="api-endpoint-title">' + escapeHtml(title) + '</p>' +
      '<p class="api-endpoint-method">POST</p></div>' +
      '</div>' +
      '<div class="endpoint api-code-row"><span class="api-code">' + escapeHtml(url) + '</span>' +
      '<button class="api-copy-btn" type="button" data-copy="' + escapeAttr(url) + '" aria-label="' + escapeAttr(t('common.copy')) + '"><i class="fa-regular fa-copy"></i></button>' +
      '</div></section>';
  }
  function renderUsage() {
    const c = $('upUsage');
    if (!c) return;
    const keyShown = usageKeyRevealed ? apiKey : maskKey(apiKey);
    const keyForCmd = usageKeyRevealed ? apiKey : maskKey(apiKey);
    const claudeUrl = baseUrl + '/v1/messages';
    const openaiUrl = baseUrl + '/v1/chat/completions';
    const curl =
      'curl ' + claudeUrl + ' \\\n' +
      '  -H "Authorization: Bearer ' + keyForCmd + '" \\\n' +
      '  -H "Content-Type: application/json" \\\n' +
      '  -d \'{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}\'';

    let html =
      '<section class="api-card">' +
      '<div class="card-header api-card-header">' +
      '<span class="card-title"><i class="fa-solid fa-plug card-title-icon"></i>' + escapeHtml(t('user.usage.title')) + '</span>' +
      '<span class="api-pill">HTTP</span>' +
      '</div>' +
      '<p class="api-hint"><i class="fa-solid fa-circle-info"></i><span>' + escapeHtml(t('user.usage.intro')) + '</span></p>';

    // Base URL
    html += '<div class="form-group"><label>' + escapeHtml(t('user.usage.baseUrl')) + '</label>' +
      '<div class="endpoint api-code-row"><span class="api-code">' + escapeHtml(baseUrl) + '</span>' +
      '<button class="api-copy-btn" type="button" data-copy="' + escapeAttr(baseUrl) + '" aria-label="' + escapeAttr(t('common.copy')) + '"><i class="fa-regular fa-copy"></i></button>' +
      '</div></div>';

    // Your API key with reveal toggle
    html += '<div class="form-group"><label>' + escapeHtml(t('user.usage.yourKey')) + '</label>' +
      '<div class="endpoint api-code-row"><span class="api-code up-key-code" id="upUsageKey">' + escapeHtml(keyShown) + '</span>' +
      '<button class="btn btn-outline btn-xs" type="button" id="upKeyReveal">' +
      '<i class="fa-solid ' + (usageKeyRevealed ? 'fa-eye-slash' : 'fa-eye') + '"></i>' +
      '<span>' + escapeHtml(usageKeyRevealed ? t('user.usage.hideKey') : t('user.usage.revealKey')) + '</span></button>' +
      '<button class="api-copy-btn" type="button" data-copy-key="1" aria-label="' + escapeAttr(t('common.copy')) + '"><i class="fa-regular fa-copy"></i></button>' +
      '</div>' +
      '<small>' + escapeHtml(t('user.usage.keyHiddenNote')) + '</small></div>';

    // Endpoints
    html += '<div class="form-section-title">' + escapeHtml(t('user.usage.endpointsTitle')) + '</div>' +
      '<div class="api-endpoint-list">' +
      endpointRow('fa-solid fa-message', t('user.usage.claude'), claudeUrl) +
      endpointRow('fa-solid fa-wand-magic-sparkles', t('user.usage.openai'), openaiUrl) +
      '</div>';

    // cURL example
    html += '<div class="form-section-title">' + escapeHtml(t('user.usage.curlExample')) + '</div>' +
      '<div class="up-key-inline" style="margin-bottom:0.5rem;justify-content:flex-end">' +
      '<button class="btn btn-outline btn-xs" type="button" data-copy-curl="1"><i class="fa-regular fa-copy"></i><span>' + escapeHtml(t('common.copy')) + '</span></button>' +
      '</div>' +
      '<pre class="code-card" id="upCurl">' + escapeHtml(curl) + '</pre>' +
      '<p class="api-hint"><i class="fa-solid fa-lightbulb"></i><span>' + escapeHtml(t('user.usage.clientHint')) + '</span></p>';

    html += '</section>';
    c.innerHTML = html;

    // Wire copy buttons for this render
    qsa('[data-copy]', c).forEach(btn => btn.addEventListener('click', async () => {
      await copyText(btn.dataset.copy);
      toast(t('common.copied'), 'primary');
    }));
    const keyCopy = c.querySelector('[data-copy-key]');
    if (keyCopy) keyCopy.addEventListener('click', async () => {
      await copyText(apiKey);
      toast(t('common.copied'), 'primary');
    });
    const curlCopy = c.querySelector('[data-copy-curl]');
    if (curlCopy) curlCopy.addEventListener('click', async () => {
      const realCurl =
        'curl ' + claudeUrl + ' \\\n' +
        '  -H "Authorization: Bearer ' + apiKey + '" \\\n' +
        '  -H "Content-Type: application/json" \\\n' +
        '  -d \'{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}\'';
      await copyText(realCurl);
      toast(t('common.copied'), 'primary');
    });
    const reveal = $('upKeyReveal');
    if (reveal) reveal.addEventListener('click', () => { usageKeyRevealed = !usageKeyRevealed; renderUsage(); });
  }

  // ── Tabs ──
  function renderCurrentTab() {
    if (currentTab === 'overview') renderOverview();
    else if (currentTab === 'records') renderRecords();
    else if (currentTab === 'recharges') renderRecharges();
    else if (currentTab === 'children') renderChildren();
    else if (currentTab === 'usage') renderUsage();
  }
  // Restart the shared entry animation on whichever panel just became visible.
  function fadeInPanel(panel) {
    if (!panel) return;
    panel.classList.remove('ng-fade-in');
    void panel.offsetWidth; // force reflow so the animation re-triggers on re-show
    panel.classList.add('ng-fade-in');
  }
  function switchTab(tab) {
    currentTab = tab;
    qsa('#userTabBar .tab').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
    qsa('.tab-content').forEach(c => { c.classList.add('hidden'); c.classList.remove('ng-fade-in'); });
    const panel = $('userTab' + tab.charAt(0).toUpperCase() + tab.slice(1));
    if (panel) { panel.classList.remove('hidden'); fadeInPanel(panel); }
    renderCurrentTab();
  }

  // ── Events ──
  function onPagerClick(e) {
    const pager = e.target.closest('[data-pager]');
    if (!pager || pager.disabled) return;
    const dir = parseInt(pager.dataset.dir, 10) || 0;
    if (pager.dataset.pager === 'records') { recordsPage = Math.max(1, recordsPage + dir); renderRecords(); }
    else if (pager.dataset.pager === 'recharges') { rechargesPage = Math.max(1, rechargesPage + dir); renderRecharges(); }
  }

  function wireEvents() {
    // Login
    $('userLoginBtn').addEventListener('click', login);
    $('userKeyField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });
    const keyToggle = $('userKeyToggle');
    if (keyToggle) keyToggle.addEventListener('click', () => {
      const f = $('userKeyField');
      const willShow = f.type === 'password';
      f.type = willShow ? 'text' : 'password';
      keyToggle.dataset.shown = String(willShow);
      keyToggle.setAttribute('aria-label', willShow ? t('user.login.hideKey') : t('user.login.showKey'));
      keyToggle.innerHTML = willShow ? '<i class="fa-solid fa-eye-slash"></i>' : '<i class="fa-solid fa-eye"></i>';
    });

    // Shell
    $('loginThemeToggle').addEventListener('click', toggleTheme);
    $('mainThemeToggle').addEventListener('click', toggleTheme);
    $('userLogoutBtn').addEventListener('click', logout);

    document.body.addEventListener('click', e => {
      const lb = e.target.closest('.lang-btn');
      if (lb) setLang(lb.dataset.lang);
    });

    qsa('#userTabBar .tab').forEach(tab => tab.addEventListener('click', () => switchTab(tab.dataset.tab)));

    // Delegated pager clicks for records / recharges
    const recEl = $('upRecords');
    if (recEl) recEl.addEventListener('click', onPagerClick);
    const rchEl = $('upRecharges');
    if (rchEl) rchEl.addEventListener('click', onPagerClick);
  }

  // ── Init ──
  async function init() {
    initTheme();
    await loadLocale(currentLang);
    if (currentLang !== 'zh') await loadLocale('zh');
    applyTranslations();
    const yr = $('userFooterYear');
    if (yr) yr.textContent = new Date().getFullYear();
    wireEvents();
    // Prefill remembered key into the login field (masked type keeps it hidden).
    if (apiKey) { const f = $('userKeyField'); if (f) f.value = apiKey; }
    await tryAutoLogin();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();

/*
 * NorthGod Kiro-Go admin UI logic.
 */
(() => {
  'use strict';

  // State
  const baseUrl = location.origin;
  if (localStorage.getItem('kiro_remember') !== '1') {
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
  }
  let password = sessionStorage.getItem('admin_password') || localStorage.getItem('admin_password') || '';
  let currentLang = localStorage.getItem('kiro_lang') || 'zh';
  const dict = { en: null, zh: null };
  let accountsData = [];
  const selectedAccounts = new Set();
  let filterKeyword = '';
  let filterStatus = 'all';
  let accountsSortBy = 'priority';
  let privacyModeEnabled = true;
  let promptRules = [];
  let exportSelectedIds = new Set();
  let currentVersion = '';
  let testLogs = [];
  let testModalAccountId = '';
  let testModalModels = [];
  let testModalLoadingModels = false;
  let testModalModelError = false;
  let testModalRunning = false;
  let customSelectUid = 0;
  let customSelectObserver = null;
  let customSelectRefreshQueued = false;
  // Unified auto-refresh: track the active tab + last-rendered data signatures
  // so the 1s loop only re-renders lists when data actually changed.
  let currentTab = 'overview';
  let autoRefreshTimer = null;
  let lastAccountsSig = '';
  let lastKeysSig = '';
  let lastLogsSig = '';
  let lastDailySig = '';

  // DOM helpers
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
  async function copyText(input) {
    const isPromise = input && typeof input.then === 'function';
    if (isPromise && typeof ClipboardItem !== 'undefined' && navigator.clipboard && navigator.clipboard.write) {
      const blobPromise = Promise.resolve(input).then(t => new Blob([String(t == null ? '' : t)], { type: 'text/plain' }));
      await navigator.clipboard.write([new ClipboardItem({ 'text/plain': blobPromise })]);
      return;
    }
    const text = isPromise ? await input : input;
    const str = String(text == null ? '' : text);
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(str);
        return;
      } catch (e) { }
    }
    const ta = document.createElement('textarea');
    ta.value = str;
    ta.readOnly = true;
    ta.className = 'clipboard-proxy';
    document.body.appendChild(ta);
    const range = document.createRange();
    range.selectNodeContents(ta);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    ta.setSelectionRange(0, str.length);
    document.execCommand('copy');
    sel.removeAllRanges();
    document.body.removeChild(ta);
  }
  function renderEndpointCode(id, value) {
    const el = $(id);
    if (!el) return;
    const raw = String(value || '');
    el.dataset.rawValue = raw;
    try {
      const url = new URL(raw);
      const path = url.pathname + url.search + url.hash;
      el.innerHTML =
        '<span class="api-code-protocol">' + escapeHtml(url.protocol + '//') + '</span>' +
        '<span class="api-code-host">' + escapeHtml(url.host) + '</span>' +
        '<span class="api-code-path">' + escapeHtml(path) + '</span>';
    } catch (e) {
      el.textContent = raw;
    }
  }

  // i18n
  // Inline admin-only strings merged into the runtime dict so we never touch
  // the shared locale JSON files. t() falls back to the key when missing.
  const ADMIN_I18N = {
    zh: {
      'tabs.overview': '概览',
      'overview.title': '概览',
      'overview.totalRpm': '总实时 RPM',
      'overview.promptCacheHitRate': '缓存命中率(7天)',
      'accounts.remainingQuota': '剩余额度',
      'overview.accounts': '账号',
      'overview.cards': '卡密',
      'overview.totalRequests': '总请求',
      'overview.successRate': '成功率',
      'overview.todayCredits': '今日消耗积分',
      'overview.inflight': '当前并发',
      'overview.stickySessions': '粘性会话',
      'overview.rpmTrend': '实时 RPM 趋势',
      'overview.dailyStats': '每日统计',
      'overview.cacheTitle': '缓存 / 并发',
      'overview.activeAccounts': '活跃账号',
      'overview.activeKeys': '活跃卡密',
      'overview.uptime': '运行时间',
      'settings.advanced': '高级设置',
      'keys.total': '总卡密',
      'keys.rpmLabel': '实时 RPM',
      'keys.sortLabel': '排序方式',
      'keys.sortBalance': '按余额',
      'keys.sortRpm': '按实时 RPM',
      'keys.sortCreatedDesc': '最新创建',
      'keys.copyKey': '复制 Key',
      'keys.copyUrl': '复制地址',
      'keys.copyKeyDone': 'Key 已复制',
      'keys.copyUrlDone': '接口地址已复制',
      'keys.baseUrl': '接口地址',
      'apiKeys.concurrencyModeDefault': '默认（系统限制）',
      'apiKeys.concurrencyModeCustom': '自定义',
      'apiKeys.concurrencyModeUnlimited': '无限',
      'accounts.rpm': '实时 RPM',
      'accounts.priority': '优先级',
      'accounts.priorityHint': '数值越高优先级越高，自动负载均衡时优先调度（0 = 最低，默认 1）',
      'accounts.sortLabel': '排序方式',
      'accounts.sortPriority': '按优先级',
      'accounts.sortRpm': '按实时 RPM',
      'accounts.sortUsage': '按用量',
      'accounts.usage': '用量',
      'overview.sectionRealtime': '实时',
      'overview.sectionTraffic': '流量',
      'overview.sectionAccounts': '账号',
      'overview.sectionCards': '卡密',
      'overview.sectionConcurrency': '会话 & 缓存',
      'overview.realtimeRpm': '实时 RPM',
      'overview.realtimeTpm': '实时 TPM',
      'overview.enabled': '启用',
      'overview.disabled': '禁用',
      'overview.tokenTrend': '实时 Token 趋势',
      'overview.tokensPerMin': 'Tokens/分钟',
      'overview.dailyTable': '历史数据',
      'overview.tableDate': '日期',
      'overview.tableRequests': '请求数',
      'overview.tableCredits': '积分',
      'overview.tableTokens': 'Tokens',
      'overview.tableEmpty': '暂无历史数据',
      'keys.filterLabel': '筛选',
      'keys.parentTag': '父卡',
      'keys.childAllocated': '子卡额度',
      'keys.childCount': '{0} 张子卡',
      'apiKeys.createdCopyHint': '卡密已创建，可在列表中直接复制',
      'apiKeys.createdCopied': '卡密已创建，Key 已复制到剪贴板',
      'iam.openNote': '在浏览器打开，用你的 AWS 用户名/密码登录并授权',
      'iam.authorized': '我已授权，完成添加'
    },
    en: {
      'tabs.overview': 'Overview',
      'overview.title': 'Overview',
      'overview.totalRpm': 'Total RPM',
      'overview.promptCacheHitRate': 'Cache Hit Rate (7d)',
      'accounts.remainingQuota': 'Remaining Quota',
      'overview.accounts': 'Accounts',
      'overview.cards': 'Cards',
      'overview.totalRequests': 'Total Requests',
      'overview.successRate': 'Success Rate',
      'overview.todayCredits': "Today's Credits",
      'overview.inflight': 'In-flight',
      'overview.stickySessions': 'Sticky Sessions',
      'overview.rpmTrend': 'Live RPM Trend',
      'overview.dailyStats': 'Daily Stats',
      'overview.cacheTitle': 'Cache & Concurrency',
      'overview.activeAccounts': 'Active Accounts',
      'overview.activeKeys': 'Active Cards',
      'overview.uptime': 'Uptime',
      'settings.advanced': 'Advanced Settings',
      'keys.total': 'Total Cards',
      'keys.rpmLabel': 'Live RPM',
      'keys.sortLabel': 'Sort by',
      'keys.sortBalance': 'By balance',
      'keys.sortRpm': 'By live RPM',
      'keys.sortCreatedDesc': 'Newest first',
      'keys.copyKey': 'Copy Key',
      'keys.copyUrl': 'Copy URL',
      'keys.copyKeyDone': 'Key copied',
      'keys.copyUrlDone': 'Base URL copied',
      'keys.baseUrl': 'Base URL',
      'apiKeys.concurrencyModeDefault': 'Default (system limit)',
      'apiKeys.concurrencyModeCustom': 'Custom',
      'apiKeys.concurrencyModeUnlimited': 'Unlimited',
      'accounts.rpm': 'Live RPM',
      'accounts.priority': 'Priority',
      'accounts.priorityHint': 'Higher value = higher priority; served first by the auto load-balancer (0 = lowest, default 1)',
      'accounts.sortLabel': 'Sort by',
      'accounts.sortPriority': 'By priority',
      'accounts.sortRpm': 'By live RPM',
      'accounts.sortUsage': 'By usage',
      'accounts.usage': 'Usage',
      'overview.sectionRealtime': 'Realtime',
      'overview.sectionTraffic': 'Traffic',
      'overview.sectionAccounts': 'Accounts',
      'overview.sectionCards': 'Cards',
      'overview.sectionConcurrency': 'Session & Cache',
      'overview.realtimeRpm': 'Realtime RPM',
      'overview.realtimeTpm': 'Realtime TPM',
      'overview.enabled': 'Enabled',
      'overview.disabled': 'Disabled',
      'overview.tokenTrend': 'Live Token Trend',
      'overview.tokensPerMin': 'Tokens/min',
      'overview.dailyTable': 'Daily History',
      'overview.tableDate': 'Date',
      'overview.tableRequests': 'Requests',
      'overview.tableCredits': 'Credits',
      'overview.tableTokens': 'Tokens',
      'overview.tableEmpty': 'No history yet',
      'keys.filterLabel': 'Filter',
      'keys.parentTag': 'Parent',
      'keys.childAllocated': 'Allocated',
      'keys.childCount': '{0} sub-cards',
      'apiKeys.createdCopyHint': 'Card created \u2014 copy it directly from the list',
      'apiKeys.createdCopied': 'Card created \u2014 key copied to clipboard',
      'iam.openNote': 'Open it in a browser and sign in with your AWS username/password to authorize',
      'iam.authorized': "I've authorized \u2014 finish"
    }
  };
  async function loadLocale(lang) {
    if (dict[lang]) return dict[lang];
    try {
      const res = await fetch('/admin/locales/' + lang + '.json?v=' + Date.now(), { cache: 'no-store' });
      dict[lang] = await res.json();
    } catch (e) {
      dict[lang] = {};
    }
    if (ADMIN_I18N[lang]) Object.assign(dict[lang], ADMIN_I18N[lang]);
    return dict[lang];
  }
  function t(key, ...args) {
    const active = dict[currentLang] || {};
    const fallback = dict.zh || {};
    let text = active[key] || fallback[key] || key;
    args.forEach((arg, idx) => { text = text.replace('{' + idx + '}', arg); });
    return text;
  }
  function applyTranslations() {
    qsa('[data-i18n]').forEach(el => { el.textContent = t(el.dataset.i18n); });
    qsa('[data-i18n-placeholder]').forEach(el => { el.placeholder = t(el.dataset.i18nPlaceholder); });
    qsa('[data-i18n-title]').forEach(el => { el.title = t(el.dataset.i18nTitle); });
    qsa('[data-i18n-aria-label]').forEach(el => { el.setAttribute('aria-label', t(el.dataset.i18nAriaLabel)); });
    document.title = t('app.title');
    document.documentElement.lang = currentLang;
    updateLangButtons();
    applyTheme(getThemePref());
    refreshCustomSelects();
  }
  async function setLang(lang) {
    currentLang = lang;
    localStorage.setItem('kiro_lang', lang);
    await loadLocale(lang);
    applyTranslations();
    renderVersionBadge();
    renderAccounts();
    renderApiKeys();
    renderPromptRules();
    renderLogs(logsCache);
    if (overviewData) {
      renderOverview(overviewData);
      refreshOverviewCharts(); // recreates with translated legend when visible
    }
  }
  function updateLangButtons() {
    qsa('.lang-btn').forEach(btn => btn.classList.toggle('active', btn.dataset.lang === currentLang));
    qsa('.lang-toggle').forEach(btn => {
      const label = btn.querySelector('.lang-toggle-label');
      if (label) label.textContent = currentLang === 'zh' ? t('lang.zh') : t('lang.en');
    });
  }
  function toggleLang() {
    setLang(currentLang === 'zh' ? 'en' : 'zh');
  }

  // Custom select
  function getCustomSelectLabel(select) {
    const option = select.selectedOptions && select.selectedOptions[0];
    return ((option && option.textContent) || select.value || '').trim();
  }
  function syncCustomSelect(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const value = wrap.querySelector('.custom-select-value');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (value) value.textContent = getCustomSelectLabel(select);
    if (trigger) trigger.disabled = select.disabled;
    wrap.classList.toggle('is-disabled', select.disabled);
    qsa('.custom-select-option', wrap).forEach(option => {
      const selected = option.dataset.index === String(select.selectedIndex);
      option.classList.toggle('is-selected', selected);
      option.setAttribute('aria-selected', String(selected));
    });
  }
  function renderCustomSelectOptions(select) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const content = wrap.querySelector('.custom-select-content');
    const trigger = wrap.querySelector('.custom-select-trigger');
    if (!content) return;
    if (trigger) labelCustomSelect(select, trigger, content, select.id);
    content.innerHTML = '';
    Array.from(select.options).forEach((option, index) => {
      const item = document.createElement('button');
      item.type = 'button';
      item.className = 'custom-select-option';
      item.setAttribute('role', 'option');
      item.dataset.index = String(index);
      item.disabled = option.disabled;
      item.textContent = (option.textContent || option.value || '').trim();
      content.appendChild(item);
    });
    syncCustomSelect(select);
  }
  function placeCustomSelectContent(select) {
    const wrap = select && select.__customSelect;
    if (!wrap || !wrap.classList.contains('is-open')) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    const rect = trigger.getBoundingClientRect();
    const gap = 4;
    const below = window.innerHeight - rect.bottom - gap;
    const above = rect.top - gap;
    const openUp = below < 180 && above > below;
    const available = Math.max(96, Math.min(224, (openUp ? above : below) - 4));
    content.style.left = Math.round(rect.left) + 'px';
    content.style.width = Math.round(rect.width) + 'px';
    content.style.maxHeight = Math.round(available) + 'px';
    content.style.top = openUp ? 'auto' : Math.round(rect.bottom + gap) + 'px';
    content.style.bottom = openUp ? Math.round(window.innerHeight - rect.top + gap) + 'px' : 'auto';
    content.dataset.side = openUp ? 'top' : 'bottom';
  }
  function setCustomSelectOpen(select, open) {
    const wrap = select && select.__customSelect;
    if (!wrap) return;
    const trigger = wrap.querySelector('.custom-select-trigger');
    const content = wrap.querySelector('.custom-select-content');
    if (!trigger || !content) return;
    if (open && !select.disabled) {
      closeAllCustomSelects(select);
      renderCustomSelectOptions(select);
      wrap.classList.add('is-open');
      trigger.setAttribute('aria-expanded', 'true');
      content.hidden = false;
      placeCustomSelectContent(select);
      requestAnimationFrame(() => placeCustomSelectContent(select));
      const selected = content.querySelector('.custom-select-option.is-selected:not(:disabled)') || content.querySelector('.custom-select-option:not(:disabled)');
      if (selected) selected.focus({ preventScroll: true });
    } else {
      wrap.classList.remove('is-open');
      trigger.setAttribute('aria-expanded', 'false');
      content.hidden = true;
    }
  }
  function closeAllCustomSelects(except) {
    qsa('select.custom-select-native').forEach(select => {
      if (select !== except) setCustomSelectOpen(select, false);
    });
  }
  function chooseCustomSelectOption(select, index) {
    const option = select.options[index];
    if (!option || option.disabled) return;
    select.value = option.value;
    select.dispatchEvent(new Event('input', { bubbles: true }));
    select.dispatchEvent(new Event('change', { bubbles: true }));
    syncCustomSelect(select);
    setCustomSelectOpen(select, false);
    const trigger = select.__customSelect && select.__customSelect.querySelector('.custom-select-trigger');
    if (trigger && trigger.isConnected) trigger.focus({ preventScroll: true });
  }
  function focusSiblingCustomOption(current, dir) {
    const options = qsa('.custom-select-option:not(:disabled)', current.parentElement);
    const index = options.indexOf(current);
    const next = options[(index + dir + options.length) % options.length];
    if (next) next.focus({ preventScroll: true });
  }
  function getCustomSelectLabelElement(select) {
    const explicit = qsa('label').find(label => label.htmlFor === select.id);
    if (explicit) return explicit;
    const group = select.closest('.form-group');
    return group ? group.querySelector('label') : null;
  }
  function labelCustomSelect(select, trigger, content, id) {
    trigger.id = id + '-trigger';
    const valueId = id + '-value';
    const value = trigger.querySelector('.custom-select-value');
    if (value) value.id = valueId;
    const label = getCustomSelectLabelElement(select);
    if (label) {
      if (!label.id) label.id = id + '-label';
      trigger.removeAttribute('aria-label');
      trigger.setAttribute('aria-labelledby', label.id + ' ' + valueId);
    } else {
      trigger.removeAttribute('aria-labelledby');
      trigger.setAttribute('aria-label', select.getAttribute('aria-label') || getCustomSelectLabel(select));
    }
    content.setAttribute('aria-labelledby', trigger.id);
  }
  function enhanceCustomSelect(select) {
    if (!select || select.__customSelect || select.dataset.nativeSelect === 'true') return;

    const id = select.id || 'custom-select-' + (++customSelectUid);
    if (!select.id) select.id = id;

    const wrap = document.createElement('div');
    wrap.className = 'custom-select';
    wrap.dataset.customSelect = 'true';
    if (select.id === 'filterStatusSelect') wrap.classList.add('custom-select-filter');

    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'custom-select-trigger';
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', id + '-menu');
    trigger.innerHTML =
      '<span class="custom-select-value"></span>' +
      '<i class="fa-solid fa-chevron-down custom-select-icon" aria-hidden="true"></i>';

    const content = document.createElement('div');
    content.id = id + '-menu';
    content.className = 'custom-select-content';
    content.setAttribute('role', 'listbox');
    content.hidden = true;
    labelCustomSelect(select, trigger, content, id);

    wrap.appendChild(trigger);
    wrap.appendChild(content);
    select.insertAdjacentElement('afterend', wrap);
    select.classList.add('custom-select-native');
    select.setAttribute('aria-hidden', 'true');
    select.tabIndex = -1;
    select.__customSelect = wrap;
    wrap.__nativeSelect = select;

    trigger.addEventListener('click', () => setCustomSelectOpen(select, !wrap.classList.contains('is-open')));
    trigger.addEventListener('keydown', e => {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
        e.preventDefault();
        setCustomSelectOpen(select, true);
      }
    });
    content.addEventListener('click', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      chooseCustomSelectOption(select, parseInt(option.dataset.index, 10));
    });
    content.addEventListener('keydown', e => {
      const option = e.target.closest('.custom-select-option');
      if (!option) return;
      if (e.key === 'ArrowDown') { e.preventDefault(); focusSiblingCustomOption(option, 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); focusSiblingCustomOption(option, -1); }
      else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); chooseCustomSelectOption(select, parseInt(option.dataset.index, 10)); }
      else if (e.key === 'Escape') { e.preventDefault(); setCustomSelectOpen(select, false); trigger.focus({ preventScroll: true }); }
    });
    select.addEventListener('change', () => syncCustomSelect(select));
    renderCustomSelectOptions(select);
  }
  function enhanceCustomSelects(root) {
    qsa('select:not(.custom-select-native)', root || document).forEach(enhanceCustomSelect);
  }
  function refreshCustomSelects(root) {
    enhanceCustomSelects(root);
    qsa('select.custom-select-native', root || document).forEach(renderCustomSelectOptions);
  }
  function positionOpenCustomSelects() {
    qsa('select.custom-select-native').forEach(placeCustomSelectContent);
  }
  function queueCustomSelectRefresh() {
    if (customSelectRefreshQueued) return;
    customSelectRefreshQueued = true;
    requestAnimationFrame(() => {
      customSelectRefreshQueued = false;
      refreshCustomSelects();
      positionOpenCustomSelects();
    });
  }
  function initCustomSelectObserver() {
    if (customSelectObserver || !document.body || typeof MutationObserver === 'undefined') return;
    customSelectObserver = new MutationObserver(mutations => {
      let shouldRefresh = false;
      for (const mutation of mutations) {
        const target = mutation.target;
        if (target && target.closest && target.closest('.custom-select')) continue;
        if (target && target.matches && target.matches('select')) {
          shouldRefresh = true;
          break;
        }
        for (const node of mutation.addedNodes || []) {
          if (node.nodeType !== 1) continue;
          if ((node.matches && node.matches('select')) || (node.querySelector && node.querySelector('select'))) {
            shouldRefresh = true;
            break;
          }
        }
        if (shouldRefresh) break;
      }
      if (shouldRefresh) queueCustomSelectRefresh();
    });
    customSelectObserver.observe(document.body, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['disabled', 'class', 'id', 'data-native-select']
    });
  }

  // Theme
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
      const themeLabel = t('theme.status', t('theme.' + pref));
      btn.setAttribute('aria-label', themeLabel);
      btn.setAttribute('title', themeLabel);
    });
  }
  function getThemePref() {
    const saved = localStorage.getItem('kiro_theme');
    return THEME_ORDER.includes(saved) ? saved : 'system';
  }
  function initTheme() {
    applyTheme(getThemePref());
    themeMQ.addEventListener('change', () => {
      if (getThemePref() === 'system') {
        applyTheme('system');
        refreshOverviewCharts();
      }
    });
  }
  function toggleTheme() {
    const cur = getThemePref();
    const next = THEME_ORDER[(THEME_ORDER.indexOf(cur) + 1) % THEME_ORDER.length];
    localStorage.setItem('kiro_theme', next);
    applyTheme(next);
    refreshOverviewCharts();
  }

  // Privacy and email mask
  function initPrivacyMode() {
    const saved = localStorage.getItem('privacyMode');
    privacyModeEnabled = saved === null ? true : saved === 'true';
    const toggle = $('privacyModeToggle');
    if (toggle) toggle.checked = privacyModeEnabled;
  }
  // Admin panel is operator-only: never mask emails/tokens. Kept as a
  // pass-through so existing call sites keep working after the privacy toggle
  // was removed.
  function maskEmail(email) {
    return email;
  }
  function getDisplayEmail(email, id) {
    const raw = email || (id ? id.substring(0, 12) + '...' : '-');
    return maskEmail(raw);
  }

  // Toast bridge
  const toast = function (msg, variant, opts) {
    if (typeof window.toast === 'function') return window.toast(msg, variant, opts);
    try { console.warn('[toast missing]', variant, msg); } catch (_) { }
    return function () {};
  };
  const toastPrimary = (msg, opts) => toast(msg, 'primary', opts);
  const toastWarning = (msg, opts) => toast(msg, 'warning', opts);
  const toastError = (msg, opts) => toast(msg, 'error', opts);

  // Modal helpers
  let modalScrollY = 0;
  let confirmResolve = null;
  const modalFocusStack = [];
  function lockModalScroll() {
    if (document.body.classList.contains('modal-open')) return;
    modalScrollY = window.scrollY || document.documentElement.scrollTop || 0;
    document.body.style.top = '-' + modalScrollY + 'px';
    document.body.classList.add('modal-open');
  }
  function unlockModalScrollIfIdle() {
    if (qsa('.modal.active').length > 0) return;
    if (!document.body.classList.contains('modal-open')) return;
    document.body.classList.remove('modal-open');
    document.body.style.top = '';
    window.scrollTo(0, modalScrollY);
  }
  function getModalFocusable(modal) {
    return qsa('a[href], button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])', modal)
      .filter(el => !el.closest('[hidden]'));
  }
  function prepareDialog(modal) {
    modal.setAttribute('role', 'dialog');
    modal.setAttribute('aria-modal', 'true');
    modal.setAttribute('aria-hidden', 'false');
    if (!modal.hasAttribute('tabindex')) modal.tabIndex = -1;
    const title = modal.querySelector('.modal-title');
    if (title) {
      if (!title.id) title.id = modal.id + 'Title';
      modal.setAttribute('aria-labelledby', title.id);
    }
  }
  function focusDialog(modal) {
    if (modal.contains(document.activeElement) && document.activeElement !== modal) return;
    const focusable = getModalFocusable(modal);
    const target = focusable[0] || modal;
    if (target && target.focus) target.focus({ preventScroll: true });
  }
  function trapDialogFocus(e) {
    const modal = e.currentTarget;
    if (e.key !== 'Tab' || !modal.classList.contains('active')) return;
    const focusable = getModalFocusable(modal);
    if (!focusable.length) {
      e.preventDefault();
      modal.focus({ preventScroll: true });
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus({ preventScroll: true });
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus({ preventScroll: true });
    }
  }
  function openDialog(id) {
    const modal = $(id);
    if (!modal) return;
    prepareDialog(modal);
    modalFocusStack.push({ id, el: document.activeElement });
    modal.removeEventListener('keydown', trapDialogFocus);
    modal.addEventListener('keydown', trapDialogFocus);
    modal.classList.add('active');
    lockModalScroll();
    focusDialog(modal);
    setTimeout(() => focusDialog(modal), 0);
  }
  function closeDialog(id) {
    const modal = $(id);
    if (!modal) return;
    modal.classList.remove('active');
    modal.setAttribute('aria-hidden', 'true');
    const stackIndex = modalFocusStack.map(item => item.id).lastIndexOf(id);
    const previous = stackIndex >= 0 ? modalFocusStack.splice(stackIndex, 1)[0].el : null;
    unlockModalScrollIfIdle();
    if (previous && previous.isConnected && previous.focus) {
      requestAnimationFrame(() => previous.focus({ preventScroll: true }));
    }
  }
  function bindDialogBackdropClose(id, closeFn) {
    const modal = $(id);
    if (!modal) return;
    let startedOnBackdrop = false;
    modal.addEventListener('pointerdown', e => {
      startedOnBackdrop = e.target === modal;
    });
    modal.addEventListener('click', e => {
      if (startedOnBackdrop && e.target === modal) closeFn();
      startedOnBackdrop = false;
    });
  }
  function closeConfirm(value) {
    if (!confirmResolve) return;
    const resolve = confirmResolve;
    confirmResolve = null;
    closeDialog('confirmModal');
    resolve(!!value);
  }
  function confirmAction(message, opts) {
    opts = opts || {};
    if (confirmResolve) closeConfirm(false);
    const modal = $('confirmModal');
    const title = $('confirmTitle');
    const msg = $('confirmMessage');
    const ok = $('confirmOk');
    const cancel = $('confirmCancel');
    const close = $('confirmClose');
    if (!modal || !title || !msg || !ok || !cancel || !close) {
      return Promise.resolve(false);
    }
    title.textContent = opts.title || t('common.confirm');
    msg.textContent = message || '';
    ok.textContent = opts.confirmText || t('common.confirm');
    cancel.textContent = opts.cancelText || t('common.cancel');
    ok.className = 'btn ' + (opts.variant === 'danger' ? 'btn-danger' : 'btn-primary');
    cancel.className = 'btn btn-secondary';
    ok.onclick = () => closeConfirm(true);
    cancel.onclick = () => closeConfirm(false);
    close.onclick = () => closeConfirm(false);
    const pending = new Promise(resolve => { confirmResolve = resolve; });
    openDialog('confirmModal');
    ok.focus({ preventScroll: true });
    return pending;
  }

  // Fetch wrapper
  function api(path, opts) {
    opts = opts || {};
    opts.headers = Object.assign({ 'X-Admin-Password': password }, opts.headers || {});
    if (opts.body && !opts.headers['Content-Type']) opts.headers['Content-Type'] = 'application/json';
    return fetch('/admin/api' + path, opts);
  }

  // Login
  function clearActivePassword() {
    sessionStorage.removeItem('admin_password');
    sessionStorage.removeItem('admin_login_time');
    localStorage.removeItem('admin_password');
    localStorage.removeItem('admin_login_time');
    password = '';
  }
  function getActiveLoginTime() {
    const storage = sessionStorage.getItem('admin_password') ? sessionStorage : localStorage;
    return parseInt(storage.getItem('admin_login_time') || '0', 10);
  }
  function setActivePassword(nextPassword, remember) {
    const now = Date.now().toString();
    password = nextPassword;
    sessionStorage.setItem('admin_password', nextPassword);
    sessionStorage.setItem('admin_login_time', now);
    if (remember) {
      localStorage.setItem('admin_password', nextPassword);
      localStorage.setItem('admin_login_time', now);
      localStorage.setItem('kiro_remember', '1');
      localStorage.setItem('kiro_remembered_pwd', nextPassword);
    } else {
      localStorage.removeItem('admin_password');
      localStorage.removeItem('admin_login_time');
      localStorage.removeItem('kiro_remember');
      localStorage.removeItem('kiro_remembered_pwd');
    }
  }
  async function tryAutoLogin() {
    if (!password) return;
    const loginTime = getActiveLoginTime();
    if (loginTime && Date.now() - loginTime > 72 * 3600 * 1000) {
      clearActivePassword();
      return;
    }
    try {
      const res = await api('/status');
      if (res.ok) { showMain(); loadData(); }
    } catch (e) { }
  }
  async function login() {
    password = $('pwdField').value;
    try {
      const res = await api('/status');
      if (res.ok) {
        const remember = $('rememberPwd');
        setActivePassword(password, !!(remember && remember.checked));
        showMain(); loadData();
      } else {
        toast(t('login.error'), 'error');
      }
    } catch (e) {
      toast(t('login.connectError'), 'error');
    }
  }
  function initRememberMe() {
    const remember = $('rememberPwd');
    const field = $('pwdField');
    if (!remember || !field) return;
    if (localStorage.getItem('kiro_remember') === '1') {
      remember.checked = true;
      const saved = localStorage.getItem('kiro_remembered_pwd');
      if (saved) field.value = saved;
    }
  }
  function logout() {
    stopAutoRefresh();
    clearActivePassword();
    location.reload();
  }
  function showMain() {
    $('loginPage').classList.add('hidden');
    $('mainPage').classList.remove('hidden');
    let saved = 'overview';
    try { const s = localStorage.getItem('kiro_tab'); if (s && KNOWN_TABS.indexOf(s) !== -1) saved = s; } catch (e) { }
    switchTab(saved);
    startAutoRefresh();
  }

  // Data loaders
  async function loadData() {
    await Promise.all([loadAccounts(), loadSettings(), loadVersion()]);
    loadStats();
  }
  // Global stats now live on the Overview tab; loadStats() just refreshes the
  // per-tab scoped stat panels (rendered from already-loaded caches).
  function loadStats() {
    renderAccountStats();
    renderKeysStats();
  }

  // ===== Stat tiles (ng-stat) shared by overview + per-tab panels =====
  function ngStatTile(label, value, opts) {
    opts = opts || {};
    const cls = 'ng-stat-value' + (opts.valueClass ? ' ' + opts.valueClass : '');
    const countAttr = (opts.count != null) ? ' data-count="' + opts.count + '" data-dec="' + (opts.dec || 0) + '"' : '';
    let valueHtml;
    if (opts.rpm) {
      // Card-format RPM: number only. The ● dot is reserved for INLINE rpm text
      // (overview header + per-account row), never on stat cards.
      valueHtml = '<div class="' + cls + ' ng-rpm"><span' + countAttr + '>' + escapeHtml(String(value)) + '</span></div>';
    } else {
      valueHtml = '<div class="' + cls + '"' + countAttr + '>' + escapeHtml(String(value)) + '</div>';
    }
    const sub = opts.sub ? '<div class="ng-stat-sub">' + escapeHtml(opts.sub) + '</div>' : '';
    return '<div class="ng-stat"><div class="ng-stat-label">' + escapeHtml(label) + '</div>' + valueHtml + sub + '</div>';
  }
  function renderAccountStats() {
    const el = $('accountsStats');
    if (!el) return;
    const total = accountsData.length;
    const enabled = accountsData.filter(a => a.enabled).length;
    const banned = accountsData.filter(a => a.banStatus && a.banStatus !== 'ACTIVE').length;
    const rpm = accountsData.reduce((s, a) => s + (a.rpm || 0), 0);
    // Remaining quota across all accounts: main quota (usageLimit - usageCurrent)
    // plus any active trial quota. Clamped at 0 so an overspent account can't
    // drag the total negative.
    const remaining = accountsData.reduce((s, a) => {
      let r = 0;
      if (a.usageLimit > 0) r += Math.max(0, (a.usageLimit || 0) - (a.usageCurrent || 0));
      if (a.trialUsageLimit > 0) r += Math.max(0, (a.trialUsageLimit || 0) - (a.trialUsageCurrent || 0));
      return s + r;
    }, 0);
    el.innerHTML =
      ngStatTile(t('overview.accounts'), total) +
      ngStatTile(t('accounts.enabled'), enabled) +
      ngStatTile(t('accounts.banned'), banned) +
      ngStatTile(t('accounts.remainingQuota'), formatNumber(remaining), { valueClass: 'success-text' }) +
      ngStatTile(t('overview.totalRpm'), rpm, { rpm: true });
  }
  function renderKeysStats() {
    const el = $('keysStats');
    if (!el) return;
    const keys = apiKeysCache.map(normalizeKey);
    const total = keys.length;
    const active = keys.filter(k => k.enabled && !keyIsExpired(k)).length;
    const used = keys.reduce((s, k) => s + (k.used || 0), 0);
    const balance = keys.reduce((s, k) => s + (k.granted > 0 ? (k.balance || 0) : 0), 0);
    el.innerHTML =
      ngStatTile(t('keys.total'), total) +
      ngStatTile(t('keys.enabled'), active) +
      ngStatTile(t('keys.used'), formatNumber(used)) +
      ngStatTile(t('keys.balance'), formatNumber(balance), { valueClass: 'success-text' });
  }
  // ===== Overview dashboard =====
  let overviewData = null;
  let rpmSamples = [];
  let rpmChart = null;
  // Live tokens-per-minute trend: renders the backend-computed totalTPM
  // (trailing-60s token ring, same semantics as totalRPM) — the frontend no
  // longer derives it by differencing counters.
  let tokenRateSamples = [];
  let tokenChart = null;
  let ovAnimated = false;
  const RPM_WINDOW = 30;

  function isOverviewVisible() {
    const el = $('tabOverview');
    return !!el && !el.classList.contains('hidden');
  }
  function hexToRgba(hex, a) {
    let h = String(hex || '').trim().replace('#', '');
    if (h.length === 3) h = h.split('').map(c => c + c).join('');
    if (h.length !== 6) return 'rgba(45,98,239,' + a + ')';
    const n = parseInt(h, 16);
    return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
  }
  function ngThemeColors() {
    const cs = getComputedStyle(document.documentElement);
    const g = (n, f) => { const v = (cs.getPropertyValue(n) || '').trim(); return v || f; };
    return {
      primary: g('--primary', '#000000'),
      info: g('--info', '#2d62ef'),
      success: g('--success', '#0f766e'),
      border: g('--border', '#e4e4e4'),
      muted: g('--muted-foreground', '#666666')
    };
  }
  function fmtCount(v, dec) {
    if (dec > 0) return Number(v).toFixed(dec);
    return Math.round(v).toLocaleString('en-US');
  }
  function animateCount(el, to, dec) {
    if (!el) return;
    const dur = 620, startT = performance.now();
    function step(now) {
      const p = Math.min(1, (now - startT) / dur);
      const eased = 1 - Math.pow(1 - p, 3);
      el.textContent = fmtCount(to * eased, dec);
      if (p < 1) requestAnimationFrame(step);
      else el.textContent = fmtCount(to, dec);
    }
    requestAnimationFrame(step);
  }
  function runCountUps(root) {
    qsa('[data-count]', root).forEach(el => {
      const to = parseFloat(el.dataset.count);
      if (isNaN(to)) return;
      animateCount(el, to, parseInt(el.dataset.dec || '0', 10));
    });
  }
  async function loadOverview() {
    let d;
    try {
      const res = await api('/overview');
      if (!res.ok) throw new Error('http ' + res.status);
      d = await res.json();
    } catch (e) {
      return;
    }
    overviewData = d;
    const rpm = d.totalRPM || 0;
    rpmSamples.push(rpm);
    if (rpmSamples.length > RPM_WINDOW) rpmSamples.shift();
    // Tokens-per-minute straight from the backend's trailing-60s token ring.
    tokenRateSamples.push(d.totalTPM || 0);
    if (tokenRateSamples.length > RPM_WINDOW) tokenRateSamples.shift();
    renderOverview(d);
    renderOverviewCharts();
  }
  function renderOverview(d) {
    d = d || {};
    const acc = d.accounts || {};
    const keys = d.keys || {};
    const conc = d.concurrency || {};
    const cache = d.cache || {};
    const daily = Array.isArray(d.daily) ? d.daily : [];
    const totalReq = d.totalRequests || 0;
    const okReq = d.successRequests || 0;
    const rate = totalReq > 0 ? ((okReq / totalReq) * 100).toFixed(1) : '0.0';
    const todayCredits = daily.length ? (daily[daily.length - 1].credits || 0) : 0;
    const rpm = d.totalRPM || 0;

    const hv = $('ovHeaderRpmVal');
    if (hv) hv.textContent = String(rpm);
    const hb = $('ovHeaderRpm');
    if (hb) hb.classList.remove('ng-rpm-live'); // header RPM indicator stays neutral (no green/pulse)

    // Overview metrics grouped into labelled sections (流量 / 账号 / 卡密 /
    // 并发 & 缓存) so it reads as an organised dashboard, not one flat block.
    const grid = $('ovStatGrid');
    if (grid) {
      const hits = cache.stickyHits || 0;
      const misses = cache.stickyMisses || 0;
      const totSticky = hits + misses;
      const hitRate = totSticky > 0 ? ((hits / totSticky) * 100).toFixed(1) + '%' : '-';
      // Deployment-wide prompt-cache hit rate over the last 7 days (from usage_records); '-' until there's input in the window.
      const pc = d.promptCache || {};
      const pcHit = (typeof pc.hitRate === 'number') ? (pc.hitRate * 100).toFixed(1) + '%' : '-';
      const pcClass = (typeof pc.hitRate === 'number' && pc.hitRate >= 0.6) ? 'success-text' : '';
      // Live tokens-per-minute: backend-computed trailing-60s figure.
      const tpm = Math.round(d.totalTPM || 0);
      grid.innerHTML =
        ngSection(t('overview.sectionRealtime'),
          ngStatTile(t('overview.realtimeRpm'), rpm, { rpm: true, count: rpm, dec: 0 }) +
          ngStatTile(t('overview.realtimeTpm'), formatNumber(tpm), { rpm: true, count: tpm, dec: 0 }) +
          ngStatTile(t('overview.inflight'), conc.inflight || 0, { count: conc.inflight || 0, dec: 0 })
        ) +
        ngSection(t('overview.sectionTraffic'),
          ngStatTile(t('overview.totalRequests'), formatNumber(totalReq), { count: totalReq, dec: 0 }) +
          ngStatTile(t('overview.successRate'), rate + '%') +
          ngStatTile(t('overview.todayCredits'), Number(todayCredits).toFixed(1), { count: todayCredits, dec: 1, valueClass: 'success-text' }) +
          ngStatTile(t('overview.promptCacheHitRate'), pcHit, { valueClass: pcClass })
        ) +
        ngSection(t('overview.sectionAccounts'),
          ngStatTile(t('overview.accounts'), (acc.available || 0) + ' / ' + (acc.total || 0)) +
          ngStatTile(t('overview.enabled'), acc.enabled || 0, { count: acc.enabled || 0, dec: 0 }) +
          ngStatTile(t('overview.disabled'), acc.disabled || 0, { count: acc.disabled || 0, dec: 0 }) +
          ngStatTile(t('overview.activeAccounts'), conc.activeAccounts || 0, { count: conc.activeAccounts || 0, dec: 0 })
        ) +
        ngSection(t('overview.sectionCards'),
          ngStatTile(t('overview.cards'), (keys.active || 0) + ' / ' + (keys.total || 0)) +
          ngStatTile(t('overview.activeKeys'), conc.activeKeys || 0, { count: conc.activeKeys || 0, dec: 0 })
        ) +
        ngSection(t('overview.sectionConcurrency'),
          ngStatTile(t('overview.stickySessions'), cache.stickySessions || 0, { count: cache.stickySessions || 0, dec: 0 }) +
          ngStatTile(t('concurrency.stickyHits'), formatNumber(hits)) +
          ngStatTile(t('concurrency.stickyMisses'), formatNumber(misses)) +
          ngStatTile(t('concurrency.hitRate'), hitRate)
        );
      if (!ovAnimated) { runCountUps(grid); ovAnimated = true; }
    }
    renderDailyTable(daily);
  }
  function ngSection(title, inner) {
    return '<div class="ng-section"><div class="ng-section-title">' + escapeHtml(title) + '</div>' +
      '<div class="ng-grid">' + inner + '</div></div>';
  }
  // 历史数据表: a compact Rust-style list (not a chart) from overview.daily[].
  function renderDailyTable(daily) {
    const el = $('ovDailyTable');
    if (!el) return;
    // Daily data changes at most once a day; skip the rebuild (and keep the
    // table's scroll position) when nothing changed. Language is part of the
    // signature so a locale switch still re-renders the headers.
    const sig = currentLang + '|' + JSON.stringify(daily || []);
    if (sig === lastDailySig && el.childElementCount) return;
    lastDailySig = sig;
    const rows = Array.isArray(daily) ? daily.slice() : [];
    if (!rows.length) {
      el.innerHTML = '<div class="empty-state">' + escapeHtml(t('overview.tableEmpty')) + '</div>';
      return;
    }
    rows.reverse(); // newest first
    let html = '<table class="data-table"><thead><tr>' +
      '<th>' + escapeHtml(t('overview.tableDate')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('overview.tableRequests')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('overview.tableCredits')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('overview.tableTokens')) + '</th>' +
      '</tr></thead><tbody>';
    rows.forEach(r => {
      html += '<tr>' +
        '<td class="ov-daily-date">' + escapeHtml(r.date || '-') + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.requests || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(Number(r.credits || 0).toFixed(1)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.tokens || 0)) + '</td>' +
        '</tr>';
    });
    html += '</tbody></table>';
    el.innerHTML = html;
  }
  function makeRpmChart() {
    const c = $('ovRpmChart');
    if (!c || typeof Chart === 'undefined') return;
    const col = ngThemeColors();
    rpmChart = new Chart(c.getContext('2d'), {
      type: 'line',
      data: {
        labels: rpmSamples.map((_, i) => i + 1),
        datasets: [{
          data: rpmSamples.slice(),
          borderColor: col.info,
          backgroundColor: hexToRgba(col.info, 0.14),
          borderWidth: 2, fill: true, tension: 0.35, pointRadius: 0
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: false, animation: false,
        plugins: { legend: { display: false } },
        scales: {
          x: { display: false, grid: { display: false } },
          y: { beginAtZero: true, grid: { color: col.border }, ticks: { color: col.muted, precision: 0, maxTicksLimit: 5 } }
        }
      }
    });
  }
  // Live tokens-per-minute trend (same client-sampled pattern as the RPM ring).
  function makeTokenChart() {
    const c = $('ovTokenChart');
    if (!c || typeof Chart === 'undefined') return;
    const col = ngThemeColors();
    tokenChart = new Chart(c.getContext('2d'), {
      type: 'line',
      data: {
        labels: tokenRateSamples.map((_, i) => i + 1),
        datasets: [{
          data: tokenRateSamples.slice(),
          borderColor: col.success,
          backgroundColor: hexToRgba(col.success, 0.14),
          borderWidth: 2, fill: true, tension: 0.35, pointRadius: 0
        }]
      },
      options: {
        responsive: true, maintainAspectRatio: false, animation: false,
        plugins: {
          legend: { display: false },
          tooltip: { callbacks: { label: (ctx) => formatNumber(Math.round(ctx.parsed.y)) + ' ' + t('overview.tokensPerMin') } }
        },
        scales: {
          x: { display: false, grid: { display: false } },
          y: { beginAtZero: true, grid: { color: col.border }, ticks: { color: col.muted, precision: 0, maxTicksLimit: 5 } }
        }
      }
    });
  }
  function ensureCharts() {
    if (typeof Chart === 'undefined') return;
    if (!rpmChart) makeRpmChart();
    if (!tokenChart) makeTokenChart();
  }
  function renderOverviewCharts() {
    if (typeof Chart === 'undefined' || !isOverviewVisible()) return;
    ensureCharts();
    if (rpmChart) {
      rpmChart.data.labels = rpmSamples.map((_, i) => i + 1);
      rpmChart.data.datasets[0].data = rpmSamples.slice();
      rpmChart.update('none');
    }
    if (tokenChart) {
      tokenChart.data.labels = tokenRateSamples.map((_, i) => i + 1);
      tokenChart.data.datasets[0].data = tokenRateSamples.slice();
      tokenChart.update('none');
    }
  }
  function refreshOverviewCharts() {
    if (rpmChart) { rpmChart.destroy(); rpmChart = null; }
    if (tokenChart) { tokenChart.destroy(); tokenChart = null; }
    renderOverviewCharts();
  }
  // ── Unified auto-refresh for the active tab (tiered cadence) ────────────
  // A single 1s interval drives all tabs, but each tab refreshes at its own
  // cadence: 概览→/overview and 日志→/logs stay at ~1s (realtime tiles / new
  // log lines), while 账号→/accounts and 卡密→/api-keys only re-fetch every
  // ~10s (their data moves on the backend's 5-minute refresh cycle, so a 1s
  // poll was pure server load). 设置 is never auto-refreshed (it holds form
  // inputs). The tick pauses while any modal or custom-select dropdown is
  // open, or while the page is hidden; list renders preserve scroll and only
  // rebuild when data changed, so there is no flicker. Regaining visibility
  // resets the slow-tab gate so the first tick refreshes immediately.
  const SLOW_TAB_REFRESH_MS = 10000;
  const slowTabLastFetch = { accounts: 0, keys: 0 };
  let autoRefreshBusy = false;
  function anyModalOpen() { return !!document.querySelector('.modal.active'); }
  function anyCustomSelectOpen() { return !!document.querySelector('.custom-select.is-open'); }
  function resetSlowTabGate() {
    slowTabLastFetch.accounts = 0;
    slowTabLastFetch.keys = 0;
  }
  async function autoRefreshTick() {
    if (autoRefreshBusy || !password) return;
    const main = $('mainPage');
    if (!main || main.classList.contains('hidden')) return;
    if (document.hidden) return;
    if (anyModalOpen() || anyCustomSelectOpen()) return;
    // Guard against overlapping refreshes if a fetch runs longer than the tick.
    autoRefreshBusy = true;
    try {
      if (currentTab === 'overview') await loadOverview();
      else if (currentTab === 'accounts') {
        if (Date.now() - slowTabLastFetch.accounts >= SLOW_TAB_REFRESH_MS) await loadAccounts(true);
      }
      else if (currentTab === 'keys') {
        if (Date.now() - slowTabLastFetch.keys >= SLOW_TAB_REFRESH_MS) await loadApiKeys(true);
      }
      else if (currentTab === 'logs') await loadLogs(true);
    } catch (e) { /* transient error: the next tick retries */ }
    finally { autoRefreshBusy = false; }
  }
  function startAutoRefresh() {
    stopAutoRefresh();
    autoRefreshTimer = setInterval(autoRefreshTick, 1000);
  }
  function stopAutoRefresh() {
    if (autoRefreshTimer) { clearInterval(autoRefreshTimer); autoRefreshTimer = null; }
  }

  // ===== Logs =====
  let logsFilter = 'all';
  let logsCache = [];

  function errorTypeLabel(type) {
    if (!type) return '';
    const key = 'errors.type' + type.charAt(0).toUpperCase() + type.slice(1);
    return t(key) || type;
  }

  function formatLogTime(ts) {
    const d = new Date(ts * 1000);
    const pad = n => String(n).padStart(2, '0');
    return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
      pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  function accountLabel(id) {
    if (!id) return '-';
    const acc = accountsData.find(a => a.id === id);
    if (acc && acc.email) {
      return privacyModeEnabled ? maskEmail(acc.email) : acc.email;
    }
    return id.slice(0, 8);
  }

  async function loadLogs(quiet) {
    try {
      const res = await api('/logs');
      const d = await res.json();
      const logs = d.logs || [];
      renderLogs(logs, quiet);
    } catch (e) {
      // silent
    }
  }

  function renderLogs(logs, quiet) {
    logsCache = logs;
    const list = $('logsList');
    const summary = $('logsSummary');
    if (!list) return;

    // On a quiet auto-refresh, skip the rebuild (keeping the log list's scroll
    // position) unless the logs or the active filter changed.
    const sig = currentLang + '|' + logsFilter + '|' + JSON.stringify(logs);
    if (quiet && sig === lastLogsSig && list.childElementCount) return;
    lastLogsSig = sig;

    const total = logs.length;
    const okCount = logs.filter(l => l.status === 'success').length;
    const errCount = total - okCount;
    summary.innerHTML =
      '<span>' + escapeHtml(t('logs.total')) + ': <strong>' + total + '</strong></span>' +
      '<span>' + escapeHtml(t('logs.success')) + ': <strong>' + okCount + '</strong></span>' +
      '<span>' + escapeHtml(t('logs.errors')) + ': <strong>' + errCount + '</strong></span>';

    const filtered = logs.filter(l => logsFilter === 'all' || l.status === logsFilter);

    if (!filtered.length) {
      list.innerHTML = '<p class="text-muted">' + escapeHtml(t('logs.empty')) + '</p>';
      return;
    }

    // Fixed-layout table: colgroup sets the widths so the table always fits its
    // container (no horizontal scrollbar); long cells truncate/wrap via CSS.
    let html = '<table class="logs-table">' +
      '<colgroup>' +
      '<col class="lc-time" /><col class="lc-status" /><col class="lc-endpoint" />' +
      '<col class="lc-model" /><col class="lc-account" /><col class="lc-tokens" />' +
      '<col class="lc-duration" /><col class="lc-detail" />' +
      '</colgroup>' +
      '<thead><tr>' +
      '<th>' + escapeHtml(t('logs.time')) + '</th>' +
      '<th>' + escapeHtml(t('logs.status')) + '</th>' +
      '<th>' + escapeHtml(t('logs.endpoint')) + '</th>' +
      '<th>' + escapeHtml(t('logs.model')) + '</th>' +
      '<th>' + escapeHtml(t('logs.account')) + '</th>' +
      '<th>' + escapeHtml(t('logs.tokens')) + '</th>' +
      '<th>' + escapeHtml(t('logs.duration')) + '</th>' +
      '<th>' + escapeHtml(t('logs.detail')) + '</th>' +
      '</tr></thead><tbody>';
    for (const l of filtered) {
      const isErr = l.status === 'error';
      const statusCell = '<span class="log-status log-status--' + escapeAttr(l.status) + '">' +
        escapeHtml(isErr ? t('logs.statusError') : t('logs.statusSuccess')) + '</span>';
      let detailCell;
      if (isErr) {
        detailCell = '<span class="err-badge err-badge--' + escapeAttr(l.errorType || 'unknown') + '">' +
          escapeHtml(errorTypeLabel(l.errorType || 'unknown')) + '</span> ' +
          '<span class="log-msg">' + escapeHtml(l.error) + '</span>';
      } else {
        detailCell = '<span class="text-muted">' + (l.credits ? (l.credits.toFixed(1) + ' cr') : '-') + '</span>';
      }
      const endpoint = l.endpoint || '-';
      const model = l.model || '-';
      const account = accountLabel(l.accountId);
      html += '<tr>' +
        '<td>' + escapeHtml(formatLogTime(l.time)) + '</td>' +
        '<td>' + statusCell + '</td>' +
        '<td title="' + escapeAttr(endpoint) + '">' + escapeHtml(endpoint) + '</td>' +
        '<td title="' + escapeAttr(model) + '">' + escapeHtml(model) + '</td>' +
        '<td title="' + escapeAttr(account) + '">' + escapeHtml(account) + '</td>' +
        '<td>' + (l.tokens ? formatNum(l.tokens) : '-') + '</td>' +
        '<td>' + (l.duration ? (l.duration + 'ms') : '-') + '</td>' +
        '<td class="log-cell-wrap" title="' + escapeAttr(isErr ? (l.error || '') : '') + '">' + detailCell + '</td>' +
        '</tr>';
    }
    html += '</tbody></table>';
    const scrollTop = list.scrollTop;
    list.innerHTML = html;
    if (quiet) list.scrollTop = scrollTop;
  }

  async function clearLogs() {
    const ok = await confirmAction(t('logs.clearConfirm'), { title: t('logs.clear'), variant: 'danger' });
    if (!ok) return;
    await api('/logs', { method: 'DELETE' });
    renderLogs([]);
    toast(t('logs.cleared'), 'success');
  }

  async function loadAccounts(quiet) {
    // Any fetch (auto or user-triggered) restarts the slow-tab window.
    slowTabLastFetch.accounts = Date.now();
    let data;
    try {
      const res = await api('/accounts');
      if (!res.ok) throw new Error('http ' + res.status);
      data = await res.json();
    } catch (e) {
      if (quiet) return; // keep the current view on a transient auto-refresh error
      throw e;
    }
    accountsData = data;
    renderAccounts(quiet);
  }

  // Account list
  function getFilteredAccounts() {
    const list = accountsData.filter(a => {
      if (filterStatus === 'enabled' && !a.enabled) return false;
      if (filterStatus === 'disabled' && (a.enabled || (a.banStatus && a.banStatus !== 'ACTIVE'))) return false;
      if (filterStatus === 'banned' && (!a.banStatus || a.banStatus === 'ACTIVE')) return false;
      if (filterKeyword) {
        const kw = filterKeyword.toLowerCase();
        if (!(a.email || '').toLowerCase().includes(kw)) return false;
      }
      return true;
    });
    // All account sorts are high→low (descending), mirroring the card-key list.
    list.sort((a, b) => {
      switch (accountsSortBy) {
        case 'rpm': return (b.rpm || 0) - (a.rpm || 0);
        case 'usage': return (b.usagePercent || 0) - (a.usagePercent || 0);
        case 'priority':
        default: return (b.weight || 0) - (a.weight || 0);
      }
    });
    return list;
  }
  function onFilterChange() {
    filterKeyword = $('filterSearch').value;
    filterStatus = $('filterStatusSelect').value;
    renderAccounts();
  }
  function toggleSelectAll(checked) {
    const filtered = getFilteredAccounts();
    if (checked) filtered.forEach(a => selectedAccounts.add(a.id));
    else selectedAccounts.clear();
    renderAccounts();
    updateBatchBar();
  }
  function toggleSelectAccount(id) {
    if (selectedAccounts.has(id)) selectedAccounts.delete(id);
    else selectedAccounts.add(id);
    updateBatchBar();
  }
  function updateBatchBar() {
    const bar = $('batchBar');
    const count = selectedAccounts.size;
    const cb = $('selectAllCheckbox');
    if (cb) {
      const filtered = getFilteredAccounts();
      const selectedFiltered = filtered.filter(a => selectedAccounts.has(a.id)).length;
      cb.checked = filtered.length > 0 && selectedFiltered === filtered.length;
      cb.indeterminate = selectedFiltered > 0 && selectedFiltered < filtered.length;
    }
    if (count > 0) {
      bar.classList.remove('hidden');
      $('batchCount').textContent = String(count);
    } else {
      bar.classList.add('hidden');
    }
  }

  function formatSubscriptionLabel(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return t('subscription.power');
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return t('subscription.proPlus');
    if (s.includes('PRO')) return t('subscription.pro');
    if (s.includes('FREE')) return t('subscription.free');
    return type || t('subscription.free');
  }
  function getSubBadge(type) {
    const s = (type || '').toUpperCase();
    if (s.includes('POWER')) return '<span class="badge badge-power">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO_PLUS') || s.includes('PROPLUS')) return '<span class="badge badge-proplus">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    if (s.includes('PRO')) return '<span class="badge badge-pro">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
    return '<span class="badge badge-free">' + escapeHtml(formatSubscriptionLabel(type)) + '</span>';
  }
  function getTrialBadge(a) {
    if (a.trialStatus === 'ACTIVE' && a.trialUsageLimit > 0) {
      return '<span class="badge badge-trial">' + escapeHtml(t('accounts.trial')) + '</span>';
    }
    return '';
  }
  function formatTrialExpiry(ts) {
    if (!ts) return '';
    const date = new Date(ts * 1000);
    const diffDays = Math.ceil((date - new Date()) / (1000 * 60 * 60 * 24));
    if (diffDays < 0) return '(' + t('accounts.trialExpired') + ')';
    if (diffDays === 0) return '(' + t('accounts.trialToday') + ')';
    if (diffDays <= 7) return '(' + diffDays + t('accounts.trialDays') + ')';
    return '';
  }
  function formatAuthMethod(method) {
    if (!method) return '-';
    const normalized = String(method).toLowerCase();
    if (normalized === 'idc') return t('auth.enterprise');
    if (normalized === 'social') return t('auth.social');
    if (normalized === 'builderid') return 'BuilderID';
    if (normalized === 'github') return t('local.providerGithub');
    if (normalized === 'google') return t('local.providerGoogle');
    return method;
  }
  function getStatusBadge(a) {
    const out = [];
    const isBanned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (isBanned) {
      if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned">' + escapeHtml(t('accounts.banned')) + '</span>');
      else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended">' + escapeHtml(t('accounts.suspended')) + '</span>');
      out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    } else {
      if (!a.hasToken)
        out.push('<span class="badge badge-error">' + escapeHtml(t('accounts.noToken')) + '</span>');
      else if (accountTokenExpired(a))
        out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.expired')) + '</span>');
      else
        out.push('<span class="badge badge-success">' + escapeHtml(t('accounts.normal')) + '</span>');
      out.push(a.enabled
        ? '<span class="badge badge-info">' + escapeHtml(t('accounts.enabled')) + '</span>'
        : '<span class="badge badge-warning">' + escapeHtml(t('accounts.disabled')) + '</span>');
    }
    return out.join('');
  }
  function formatTokenExpiry(ts) {
    if (!ts) return '-';
    const diff = ts - Date.now() / 1000;
    if (diff <= 0) return t('time.expired');
    if (diff < 3600) return Math.floor(diff / 60) + t('time.minutes');
    if (diff < 86400) return Math.floor(diff / 3600) + t('time.hours');
    return Math.floor(diff / 86400) + t('time.days');
  }
  // Compact number for dense card blocks: 1.7B / 21.9K / 940. Keeps every value
  // to <=5 glyphs so fixed-width block columns never overflow (no ellipsis).
  function formatNum(n) {
    n = Number(n) || 0;
    const abs = Math.abs(n);
    if (abs >= 1e9) return (n / 1e9).toFixed(abs >= 1e10 ? 0 : 1) + 'B';
    if (abs >= 1e6) return (n / 1e6).toFixed(abs >= 1e7 ? 0 : 1) + 'M';
    if (abs >= 1e3) return (n / 1e3).toFixed(abs >= 1e4 ? 0 : 1) + 'K';
    if (Math.floor(n) === n) return n.toString();
    return n.toFixed(1);
  }
  // Relative "time ago" for a Unix-seconds timestamp (used by account added-at / logs / records).
  function formatRelTime(ts) {
    if (!ts) return '';
    let diff = Date.now() / 1000 - ts;
    if (diff < 0) diff = 0;
    if (diff < 60) return t('reltime.now');
    if (diff < 3600) return t('reltime.minutes', Math.floor(diff / 60));
    if (diff < 86400) return t('reltime.hours', Math.floor(diff / 3600));
    if (diff < 86400 * 30) return t('reltime.days', Math.floor(diff / 86400));
    if (diff < 86400 * 365) return t('reltime.months', Math.floor(diff / (86400 * 30)));
    return t('reltime.years', Math.floor(diff / (86400 * 365)));
  }
  // Compact duration (e.g. account uptime since createdAt).
  function formatDurationShort(seconds) {
    seconds = Math.max(0, Math.floor(seconds));
    const d = Math.floor(seconds / 86400);
    const h = Math.floor((seconds % 86400) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    if (d > 0) return d + t('time.days') + (h > 0 ? ' ' + h + t('time.hours') : '');
    if (h > 0) return h + t('time.hours') + (m > 0 ? ' ' + m + t('time.minutes') : '');
    if (m > 0) return m + t('time.minutes');
    return seconds + 's';
  }
  function formatDateTime(ts) {
    if (!ts) return '-';
    try { return new Date(ts * 1000).toLocaleString(); } catch (e) { return '-'; }
  }
  function applyUsageBars(root) {
    qsa('.usage-fill[data-usage-pct]', root).forEach(el => {
      const pct = Math.max(0, Math.min(100, parseFloat(el.dataset.usagePct) || 0));
      el.style.width = pct + '%';
    });
  }

  function renderAccounts(quiet) {
    renderAccountStats();
    const container = $('accountsList');
    if (!container) return;
    const filtered = getFilteredAccounts();
    // Quiet auto-refresh: bail out (preserving scroll/selection/focus) unless
    // the filtered rows, filters, sort, selection or language changed.
    const sig = currentLang + '|' + filterStatus + '|' + filterKeyword + '|' + accountsSortBy + '|' +
      Array.from(selectedAccounts).sort().join(',') + '|' + JSON.stringify(filtered);
    if (quiet && sig === lastAccountsSig && container.childElementCount) return;
    lastAccountsSig = sig;
    if (filtered.length === 0) {
      container.innerHTML = '<div class="empty-state">' + escapeHtml(t('accounts.empty')) + '</div>';
      return;
    }
    const scrollY = window.scrollY;
    container.innerHTML = filtered.map(a => renderAccountRow(a)).join('');
    applyUsageBars(container);
    enhanceCustomSelects(container);
    if (quiet) window.scrollTo(0, scrollY);
  }
  // One dense row per account: identity + badges on the left, aligned metric
  // segments in the middle, actions on the right (detail / copyJSON /
  // enable-disable / test / delete — no refresh: freshness is backend-owned).
  function renderAccountRow(a) {
    const usagePct = (a.usagePercent || 0) * 100;
    const usageClass = usagePct > 90 ? 'critical' : usagePct > 70 ? 'high' : '';
    const hasUsage = a.usageLimit > 0;
    const hasTrial = a.trialUsageLimit > 0;
    const isSelected = selectedAccounts.has(a.id);
    const weight = a.weight || 0;
    const weightBadge = '<span class="badge badge-priority" title="' + escapeAttr(t('accounts.priorityHint')) + '">' + escapeHtml(t('accounts.priority')) + ': ' + weight + '</span>';
    const overageBadge = renderOverageBadge(a);
    const banned = a.banStatus && a.banStatus !== 'ACTIVE';
    const idAttr = escapeAttr(a.id);
    const displayEmail = getDisplayEmail(a.email, a.id);
    const selectLabel = t('accounts.selectAccount', displayEmail);
    const rpm = a.rpm || 0;

    const seg = (html, title) => '<span class="ng-sub-seg"' + (title ? ' title="' + escapeAttr(title) + '"' : '') + '>' + html + '</span>';
    const subSegs = [];
    subSegs.push(seg('<i class="fa-solid fa-shield-halved"></i>' + escapeHtml(formatAuthMethod(a.provider || a.authMethod))));
    subSegs.push(seg('<i class="fa-regular fa-hourglass-half"></i>' + escapeHtml(t('accounts.expiry') + ' ' + formatTokenExpiry(a.expiresAt))));
    if (a.createdAt) {
      subSegs.push(seg('<i class="fa-regular fa-clock"></i>' + escapeHtml(t('accounts.addedAt') + ' ' + formatRelTime(a.createdAt)), formatDateTime(a.createdAt)));
      subSegs.push(seg(escapeHtml(t('accounts.uptime') + ' ' + formatDurationShort(Date.now() / 1000 - a.createdAt))));
    }
    if (hasUsage) subSegs.push(seg(escapeHtml(t('accounts.mainQuota') + ' ' + (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + '/' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0))));
    if (hasTrial) subSegs.push(seg(escapeHtml(t('accounts.trialQuota') + ' ' + (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + '/' + (a.trialUsageLimit != null ? a.trialUsageLimit.toFixed(0) : 0) + ' ' + formatTrialExpiry(a.trialExpiresAt))));

    // RPM value only — no live dot inside the dense card rows (per design: the
    // pulsing dot is reserved for the single big "total RPM" figure elsewhere).
    const rpmVal = escapeHtml(String(rpm));
    const usageExtra = hasUsage ? '<div class="ng-usage-mini"><div class="usage-fill ' + usageClass + '" data-usage-pct="' + escapeAttr(usagePct) + '"></div></div>' : '';
    // Two soft-tinted data blocks: 计费 (credits + usage%) and 流量 (rpm/req/tokens).
    // Compact numbers keep each fixed-width column from overflowing; full value on hover.
    const credits = a.totalCredits || 0;
    const creditBlock = '<div class="ng-block ng-block--credit">' +
      ngMetric(escapeHtml(formatNum(credits)), t('accounts.credits'), '', '', credits.toFixed(2)) +
      ngMetric(hasUsage ? escapeHtml(usagePct.toFixed(0) + '%') : '\u2014', t('accounts.usage'), usageExtra) +
      '</div>';
    const trafficBlock = '<div class="ng-block ng-block--traffic">' +
      ngMetric(rpmVal, t('accounts.rpm')) +
      ngMetric(escapeHtml(formatNum(a.requestCount || 0)), t('accounts.requests'), '', '', formatNumber(a.requestCount || 0)) +
      ngMetric(escapeHtml(formatNum(a.totalTokens || 0)), t('accounts.tokens'), '', '', formatNumber(a.totalTokens || 0)) +
      '</div>';
    const blocks = creditBlock + trafficBlock;
    const dotCls = accountDotClass(a);

    const userSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';
    const copySvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
    const actions =
      '<button class="btn btn-icon btn-sm btn-ghost" data-action="detail" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.detail')) + '">' + userSvg + '</button>' +
      '<button class="btn btn-icon btn-sm btn-ghost" data-action="copyJSON" data-id="' + idAttr + '" title="' + escapeAttr(t('accounts.copyJSON')) + '">' + copySvg + '</button>' +
      (banned ? '' :
        '<button class="btn btn-sm ' + (a.enabled ? 'btn-outline' : 'btn-primary') + '" data-action="toggle" data-id="' + idAttr + '" data-enabled="' + (!a.enabled) + '">' +
        escapeHtml(a.enabled ? t('accounts.disable') : t('accounts.enable')) + '</button>') +
      '<button class="btn btn-sm btn-secondary" data-action="test" data-id="' + idAttr + '" id="test-' + idAttr + '">' + escapeHtml(t('accounts.test')) + '</button>' +
      '<button class="btn btn-sm btn-danger" data-action="delete" data-id="' + idAttr + '">' + escapeHtml(t('accounts.delete')) + '</button>';

    return '<div class="account-card' + (isSelected ? ' selected' : '') + '" data-id="' + idAttr + '">' +
      '<input type="checkbox" class="account-checkbox" ' + (isSelected ? 'checked' : '') + ' data-id="' + idAttr + '" aria-label="' + escapeAttr(selectLabel) + '" />' +
      '<div class="ng-row-main">' +
        '<div class="ng-row-title">' +
          '<span class="ng-status-dot ' + dotCls + '" aria-hidden="true"></span>' +
          '<span class="ng-row-name">' + escapeHtml(displayEmail) + '</span>' +
        '</div>' +
        '<div class="ng-row-tags">' +
          getSubBadge(a.subscriptionType) + getTrialBadge(a) + overageBadge + weightBadge + accountStateBadges(a) +
        '</div>' +
        '<div class="ng-row-sub">' + subSegs.join('') + '</div>' +
      '</div>' +
      '<div class="ng-row-blocks">' + blocks + '</div>' +
      '<div class="ng-row-actions">' + actions + '</div>' +
    '</div>';
  }
  // Aligned metric segment (value + label, optional extra element e.g. mini bar,
  // optional value color class e.g. success-text for a positive balance).
  function ngMetric(valHtml, label, extra, valCls, title) {
    const titleAttr = title ? ' title="' + escapeAttr(String(title)) + '"' : '';
    return '<div class="ng-metric"' + titleAttr + '><span class="ng-metric-val' + (valCls ? ' ' + valCls : '') + '">' + valHtml + '</span>' +
      '<span class="ng-metric-label">' + escapeHtml(label) + '</span>' + (extra || '') + '</div>';
  }
  // A short-lived access token past its expiry is normal and self-healing for
  // accounts that can renew it: OAuth/IdC accounts refresh via refreshToken, and
  // api_key (ksk_) accounts use the key itself as a long-lived bearer. The backend
  // auto-refreshes on demand and every 30 min, so only accounts that genuinely
  // *cannot* refresh should ever read as "expired". canRefresh comes from the API.
  function accountTokenExpired(a) {
    return a.expiresAt && a.expiresAt < Date.now() / 1000 && !a.canRefresh;
  }
  // Overall-health dot shown on the account title line. Red = banned / no-token /
  // token-expired; amber = manually disabled; green = normal & enabled. The full
  // reasons live in the grouped tag strip below.
  function accountDotClass(a) {
    const banned = a.banStatus && a.banStatus !== 'ACTIVE';
    if (banned || !a.hasToken || accountTokenExpired(a)) return 'is-bad';
    if (!a.enabled) return 'is-warn';
    return 'is-ok';
  }
  // Exceptional state badges only (banned / suspended / no-token / expired /
  // disabled). The healthy "normal + enabled" case is conveyed by the green dot,
  // so it adds no badge here — keeping the strip focused on what needs attention.
  function accountStateBadges(a) {
    const banned = a.banStatus && a.banStatus !== 'ACTIVE';
    const out = [];
    if (a.banStatus === 'BANNED') out.push('<span class="badge badge-banned">' + escapeHtml(t('accounts.banned')) + '</span>');
    else if (a.banStatus === 'SUSPENDED') out.push('<span class="badge badge-suspended">' + escapeHtml(t('accounts.suspended')) + '</span>');
    if (!a.hasToken) out.push('<span class="badge badge-error">' + escapeHtml(t('accounts.noToken')) + '</span>');
    else if (accountTokenExpired(a)) out.push('<span class="badge badge-warning">' + escapeHtml(t('accounts.expired')) + '</span>');
    if (!a.enabled && !banned) out.push('<span class="badge badge-muted">' + escapeHtml(t('accounts.disabled')) + '</span>');
    return out.join('');
  }
  // Overall-health dot for a card row: red = expired; amber = disabled or an
  // overspent (negative) balance; green otherwise.
  function keyDotClass(k, expired) {
    if (expired) return 'is-bad';
    if (!k.enabled) return 'is-warn';
    if (k.granted > 0 && k.balance < 0) return 'is-warn';
    return 'is-ok';
  }

  // Account actions
  async function toggleAccount(id, enabled) {
    await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify({ enabled }) });
    loadAccounts();
  }
  async function deleteAccount(id) {
    const ok = await confirmAction(t('accounts.confirmDelete'), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/accounts/' + id, { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('accounts.deleteSuccess'), 'danger', { icon: 'fa-solid fa-trash' });
      loadAccounts(); loadStats();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function copyAccountJSON(id, btn) {
    try {
      // Copy the same Kiro Account Manager (KAM) format as the export/download,
      // scoped to this one account, so region / authMethod / startUrl survive —
      // critical for IdC (IAM Identity Center) and enterprise accounts, which the
      // old 4-field {clientId,clientSecret,accessToken,refreshToken} copy dropped.
      const jsonPromise = api('/export', { method: 'POST', body: JSON.stringify({ ids: [id] }) }).then(async res => {
        if (!res.ok) throw new Error('Failed');
        const data = await res.json();
        return JSON.stringify(data, null, 2);
      });
      await copyText(jsonPromise);
      flashCopySuccess(btn);
      toastPrimary(t('accounts.copyJSONSuccess'));
    } catch (e) {
      toastError(t('common.failed'));
    }
  }
  function flashCopySuccess(btn) {
    if (!btn) return;
    const html = btn.innerHTML, cls = btn.className;
    btn.disabled = true;
    btn.className = 'btn btn-icon btn-sm btn-success';
    btn.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg>';
    setTimeout(() => { btn.disabled = false; btn.className = cls; btn.innerHTML = html; }, 800);
  }

  // Batch actions (enable / disable — the batch refresh triggers are gone: data
  // freshness is backend-owned, the UI only mutates real state).
  async function batchAction(action) {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmKey = 'batch.confirm' + action.charAt(0).toUpperCase() + action.slice(1);
    const ok = await confirmAction(t(confirmKey, ids.length), {
      title: t('common.confirm'),
      confirmText: t('common.confirm'),
      variant: action === 'disable' ? 'danger' : 'primary'
    });
    if (!ok) return;
    const dismiss = toast(t('batch.processing'), 'info', { duration: 0 });
    try {
      const res = await api('/accounts/batch', { method: 'POST', body: JSON.stringify({ ids, action }) });
      const d = await res.json();
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      dismiss();
      if (action === 'enable') {
        toast(t('batch.enableResult', d.count || ids.length), 'success');
      } else if (action === 'disable') {
        toast(t('batch.disableResult', d.count || ids.length), 'success');
      } else {
        toast(t('batch.done'), 'success');
      }
      selectedAccounts.clear();
      updateBatchBar();
      loadAccounts(); loadStats();
    } catch (e) {
      dismiss();
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }
  async function batchDelete() {
    const ids = Array.from(selectedAccounts);
    if (!ids.length) return;
    const confirmed = await confirmAction(t('batch.confirmDelete', ids.length), {
      title: t('accounts.delete'),
      confirmText: t('accounts.delete'),
      variant: 'danger'
    });
    if (!confirmed) return;
    const dismiss = toast(t('batch.deleting'), 'info', { duration: 0 });
    let ok = 0, fail = 0;
    for (const id of ids) {
      try {
        const res = await api('/accounts/' + id, { method: 'DELETE' });
        const d = await res.json().catch(() => ({}));
        if (res.ok && d.success !== false) ok++; else fail++;
      } catch { fail++; }
    }
    dismiss();
    toast(t('batch.deleteResult', ok, fail), fail ? 'warning' : 'success', { icon: 'fa-solid fa-trash' });
    selectedAccounts.clear();
    updateBatchBar();
    loadAccounts(); loadStats();
  }

  // Detail modal
  function detailItem(label, value) {
    return '<div class="detail-item"><div class="detail-label">' + escapeHtml(label) + '</div><div class="detail-value">' + escapeHtml(value) + '</div></div>';
  }
  // Opening the modal fetches CURRENT data from the backend (auto-refresh
  // polling pauses while a modal is open, so the list snapshot could be stale)
  // and renders the account's cached models — read-only, no refresh triggers:
  // freshness is owned entirely by the backend's periodic refresh.
  async function showDetail(id) {
    let a = accountsData.find(x => x.id === id);
    try {
      const res = await api('/accounts/' + encodeURIComponent(id));
      if (res.ok) {
        const fresh = await res.json();
        if (fresh && fresh.id) a = fresh;
      }
    } catch (e) { /* fall back to the list snapshot */ }
    if (!a) return;
    renderDetail(a);
    openDialog('detailModal');
    loadCachedModels(id);
  }
  function renderDetail(a) {
    const id = a.id;
    const idAttr = escapeAttr(id);
    $('detailBody').innerHTML =
      '<div class="detail-section"><h4>' + escapeHtml(t('detail.basicInfo')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.email'), getDisplayEmail(a.email, null)) +
      detailItem(t('detail.userId'), a.userId || '-') +
      detailItem(t('detail.authMethod'), formatAuthMethod(a.provider || a.authMethod)) +
      detailItem(t('detail.region'), a.region || 'us-east-1') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.machineId')) + '</h4><div class="machine-id-row">' +
      '<input type="text" id="machineIdInput" value="' + escapeAttr(a.machineId || '') + '" placeholder="UUID" />' +
      '<button class="btn btn-sm btn-outline" id="generateMachineIdBtn" type="button">' + escapeHtml(t('detail.generate')) + '</button>' +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('accounts.priority')) + '</h4>' +
      '<div class="form-group">' +
      '<input type="number" id="weightInput" value="' + (a.weight || 0) + '" min="0" step="1" placeholder="1" />' +
      '<small>' + escapeHtml(t('accounts.priorityHint')) + '</small>' +
      '</div>' +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.proxyURL')) + '</h4>' +
      '<div class="form-group">' +
      '<input type="text" id="proxyURLInput" value="' + escapeAttr(a.proxyURL || '') + '" placeholder="socks5://host:port" />' +
      '<small>' + escapeHtml(t('detail.proxyHint')) + '</small>' +
      '</div>' +
      '</div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.overage')) + '</h4>' +
      '<p class="help-block">' + escapeHtml(t('detail.overageHint')) + '</p>' +
      renderOverageBlock(a, idAttr) +
      '</div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.subscription')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.subscriptionType'), a.subscriptionTitle || (a.subscriptionType ? formatSubscriptionLabel(a.subscriptionType) : '-')) +
      detailItem(t('detail.tokenExpiry'), a.expiresAt ? new Date(a.expiresAt * 1000).toLocaleString() : '-') +
      detailItem(t('detail.mainQuota'), (a.usageCurrent != null ? a.usageCurrent.toFixed(1) : 0) + ' / ' + (a.usageLimit != null ? a.usageLimit.toFixed(0) : 0)) +
      detailItem(t('detail.resetDate'), a.nextResetDate || '-') +
      (a.trialUsageLimit > 0 ?
        detailItem(t('detail.trialQuota'), (a.trialUsageCurrent != null ? a.trialUsageCurrent.toFixed(1) : 0) + ' / ' + a.trialUsageLimit.toFixed(0)) +
        detailItem(t('detail.trialStatus'), a.trialStatus || '-') +
        detailItem(t('detail.trialExpiry'), a.trialExpiresAt ? new Date(a.trialExpiresAt * 1000).toLocaleString() : '-')
        : '') +
      '</div></div>' +

      '<div class="detail-section"><h4>' + escapeHtml(t('detail.statistics')) + '</h4><div class="detail-grid">' +
      detailItem(t('detail.requestCount'), a.requestCount || 0) +
      detailItem(t('detail.errorCount'), a.errorCount || 0) +
      detailItem(t('detail.totalTokens'), formatNum(a.totalTokens || 0)) +
      detailItem(t('detail.totalCredits'), (a.totalCredits || 0).toFixed(2)) +
      '</div></div>' +

      '<div class="detail-section">' +
      '<h4>' + escapeHtml(t('detail.models')) + '</h4>' +
      '<div id="modelsList" class="model-list"></div>' +
      '</div>';

    $('detailFooter').innerHTML =
      '<button class="btn btn-primary" data-detail-action="saveDetail" data-id="' + idAttr + '" type="button">' + escapeHtml(t('detail.save')) + '</button>';
  }
  // Renders the account's models from the backend's route cache (kept fresh by
  // the periodic background refresh) — no live upstream call from the browser.
  async function loadCachedModels(id) {
    const c = $('modelsList');
    if (!c) return;
    c.innerHTML = '<p class="empty-state">' + escapeHtml(t('detail.loading')) + '</p>';
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/models/cached');
      const d = await res.json();
      if (d.success && Array.isArray(d.models)) {
        const sorted = d.models.slice().sort((a, b) => {
          if (a.modelId === 'auto') return -1;
          if (b.modelId === 'auto') return 1;
          return (a.rateMultiplier || 1) - (b.rateMultiplier || 1);
        });
        c.innerHTML = sorted.map(m => {
          const ratio = m.rateMultiplier || 1;
          return '<div class="model-item">' +
            '<div class="model-name">' + escapeHtml(m.modelId) + '</div>' +
            '<div class="model-credit"><span class="credit-ratio">' + escapeHtml(t('detail.creditMultiplier', ratio)) + '</span></div>' +
            '<div class="model-info">' + escapeHtml(m.description || '') + '</div>' +
            '</div>';
        }).join('') || '<p class="empty-state">' + escapeHtml(t('detail.noModels')) + '</p>';
      } else {
        c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + ': ' + escapeHtml(d.error || '') + '</p>';
      }
    } catch (e) {
      c.innerHTML = '<p class="message message-error">' + escapeHtml(t('detail.loadFailed')) + '</p>';
    }
  }
  async function generateMachineId() {
    try {
      const res = await api('/generate-machine-id');
      const d = await res.json();
      if (d.machineId) $('machineIdInput').value = d.machineId;
    } catch (e) {
      toast(t('detail.generateFailed'), 'error');
    }
  }
  async function putAccount(id, body, successMsg) {
    try {
      const res = await api('/accounts/' + id, { method: 'PUT', body: JSON.stringify(body) });
      const d = await res.json();
      if (d.success) {
        toast(successMsg, 'success');
        loadAccounts();
      } else {
        toast(t('detail.saveFailed') + (d.error ? ': ' + d.error : ''), 'error');
      }
    } catch (e) {
      toast(t('detail.saveFailed'), 'error');
    }
  }
  // Unified save for the detail form: machineId + weight + proxyURL in one PUT.
  async function saveAccountDetail(id) {
    const m = $('machineIdInput').value.trim();
    if (m && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(m) && !/^[0-9a-f]{32}$/i.test(m)) {
      toast(t('detail.machineIdError'), 'warning'); return;
    }
    const proxyURL = $('proxyURLInput').value.trim();
    if (proxyURL && !/^(socks5|socks5h|http|https):\/\//.test(proxyURL)) {
      toast(t('detail.proxyFormatError'), 'warning'); return;
    }
    const weight = Math.max(0, parseInt($('weightInput').value, 10) || 0);
    await putAccount(id, { machineId: m, weight, proxyURL }, t('detail.saved'));
  }
  function renderOverageBadge(a) {
    const status = (a.overageStatus || '').toUpperCase();
    if (status === 'ENABLED') {
      return '<span class="badge badge-warning">' + escapeHtml(t('accounts.overageOn')) + '</span>';
    }
    if (status === 'DISABLED') {
      return '<span class="badge badge-muted">' + escapeHtml(t('accounts.overageOff')) + '</span>';
    }
    return '';
  }
  function renderOverageBlock(a, idAttr) {
    const status = (a.overageStatus || '').toUpperCase();
    const capable = !a.overageCapability || a.overageCapability === 'OVERAGE_CAPABLE';
    const checked = status === 'ENABLED';
    const checkedAt = a.overageCheckedAt ? new Date(a.overageCheckedAt * 1000).toLocaleString() : '-';
    const statusText = status === 'ENABLED' ? t('detail.overageEnabled')
      : status === 'DISABLED' ? t('detail.overageDisabled')
      : t('detail.overageUnknown');
    const disabledAttr = capable ? '' : ' disabled';
    return '<div class="form-group flex items-center gap-2">' +
      '<label class="switch"><input type="checkbox" id="overageSwitchInput-' + idAttr + '" data-detail-action="toggleOverage" data-id="' + idAttr + '" ' + (checked ? 'checked' : '') + disabledAttr + ' /><span class="slider"></span></label>' +
      '<span id="overageSwitchLabel-' + idAttr + '">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      (capable ? '' : '<p class="help-block" style="color:#ef4444">' + escapeHtml(t('detail.overageNotCapable')) + '</p>') +
      '<div class="detail-grid">' +
      detailItem(t('detail.overageStatus'), status || '-') +
      detailItem(t('detail.overageCap'), a.overageCap ? '$' + Number(a.overageCap).toFixed(2) : '-') +
      detailItem(t('detail.overageRate'), a.overageRate ? '$' + Number(a.overageRate).toFixed(2) : '-') +
      detailItem(t('detail.overageCurrent'), a.currentOverages ? '$' + Number(a.currentOverages).toFixed(2) : '$0') +
      detailItem(t('detail.overageCheckedAt'), checkedAt) +
      '</div>';
  }
  async function toggleOverageSwitch(id, inputEl) {
    const desired = inputEl.checked;
    const labelEl = $('overageSwitchLabel-' + id);
    const oldLabel = labelEl ? labelEl.textContent : '';
    inputEl.disabled = true;
    if (labelEl) labelEl.textContent = t('detail.overageSwitching');
    try {
      const res = await api('/accounts/' + encodeURIComponent(id) + '/overage', {
        method: 'POST',
        body: JSON.stringify({ enabled: desired }),
      });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) {
        throw new Error(d.error || t('accounts.overageSwitchFailed'));
      }
      if (labelEl) {
        labelEl.textContent = d.overageStatus === 'ENABLED' ? t('detail.overageEnabled')
          : d.overageStatus === 'DISABLED' ? t('detail.overageDisabled')
          : t('detail.overageUnknown');
      }
      inputEl.checked = d.overageStatus === 'ENABLED';
      await loadAccounts();
    } catch (e) {
      inputEl.checked = !desired;
      if (labelEl) labelEl.textContent = oldLabel;
      toast(t('accounts.overageSwitchFailed') + ': ' + (e.message || e), 'warning');
    } finally {
      inputEl.disabled = false;
    }
  }
  function closeDetailModal() { closeDialog('detailModal'); }

  // Test flow
  function getTestAccount(id) {
    return accountsData.find(a => a.id === id) || null;
  }
  function getTestModelValue() {
    const choice = $('testModelChoice');
    return (choice && choice.value.trim()) || 'claude-sonnet-4';
  }
  function renderTestLog() {
    const c = $('testModalLog');
    if (!c) return;
    if (!testLogs.length) {
      c.innerHTML = '<div class="test-log-empty">' + escapeHtml(t('accounts.testLog.empty')) + '</div>';
      return;
    }
    c.innerHTML = testLogs.map(log =>
      '<div class="test-log-line ' + escapeAttr(log.type || 'info') + '">' +
      '<span class="test-log-time">' + escapeHtml(log.time) + '</span>' +
      '<span class="test-log-message">' + escapeHtml(log.msg) + '</span>' +
      '</div>'
    ).join('');
    c.scrollTop = c.scrollHeight;
  }
  function addTestLog(msg, type) {
    const time = new Date().toLocaleTimeString();
    testLogs.push({ time, msg, type });
    if (testLogs.length > 100) testLogs.shift();
    renderTestLog();
  }
  function clearTestLog() {
    testLogs = [];
    renderTestLog();
  }
  function renderTestModal() {
    const body = $('testBody');
    if (!body) return;
    const acc = getTestAccount(testModalAccountId);
    const idAttr = escapeAttr(testModalAccountId);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : testModalAccountId;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    const statusText = testModalLoadingModels
      ? t('accounts.testModelsLoading')
      : testModalModelError
        ? t('accounts.testModelsFallback')
        : t('accounts.testModelsReady', testModalModels.length);
    const modelField = testModalLoadingModels
      ? '<div class="test-model-loading">' + escapeHtml(t('accounts.testModelsLoading')) + '</div>'
      : testModalModels.length
        ? '<select id="testModelChoice">' +
        testModalModels.map(m => '<option value="' + escapeAttr(m) + '">' + escapeHtml(m) + '</option>').join('') +
        '</select>'
        : '<input type="text" id="testModelChoice" placeholder="claude-sonnet-4" value="claude-sonnet-4" />';

    body.innerHTML =
      '<div class="test-modal-account">' +
      '<div class="test-modal-account-main">' +
      '<div class="test-modal-email">' + escapeHtml(email) + '</div>' +
      '<div class="test-modal-meta">' +
      '<span>' + escapeHtml(formatAuthMethod(acc && (acc.provider || acc.authMethod))) + '</span>' +
      '<span>' + escapeHtml(proxy) + '</span>' +
      '</div>' +
      '</div>' +
      '<span class="test-modal-status">' + escapeHtml(statusText) + '</span>' +
      '</div>' +
      '<div class="test-modal-grid">' +
      '<div class="form-group test-model-field">' +
      '<label for="testModelChoice">' + escapeHtml(t('accounts.selectModel')) + '</label>' +
      modelField +
      '</div>' +
      '<div class="test-log-card">' +
      '<div class="test-log-header">' +
      '<span class="test-log-title">' + escapeHtml(t('accounts.testLog.title')) + '</span>' +
      '<button class="btn btn-xs btn-outline test-log-clear" id="testLogClear" type="button">' + escapeHtml(t('accounts.testLog.clear')) + '</button>' +
      '</div>' +
      '<div class="test-log-content" id="testModalLog"></div>' +
      '</div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="testModalCancelBtn" type="button">' + escapeHtml(t('common.close')) + '</button>' +
      '<button class="btn btn-primary" id="testRunBtn" data-id="' + idAttr + '" type="button" ' + (testModalLoadingModels ? 'disabled' : '') + '>' + escapeHtml(t('accounts.test')) + '</button>' +
      '</div>';

    if (!testModalLoadingModels) enhanceCustomSelects(body);
    renderTestLog();
  }
  async function testAccount(id) {
    testModalAccountId = id;
    testModalModels = [];
    testModalLoadingModels = true;
    testModalModelError = false;
    testModalRunning = false;
    testLogs = [];
    renderTestModal();
    openDialog('testModal');
    try {
      const res = await api('/accounts/' + id + '/models/cached');
      const d = await res.json();
      // /models/cached entries are objects ({modelId, rateMultiplier, ...});
      // the test picker only needs the ids.
      testModalModels = Array.isArray(d.models) ? d.models.map(m => m && m.modelId).filter(Boolean).sort() : [];
    } catch (e) {
      testModalModelError = true;
    } finally {
      testModalLoadingModels = false;
      renderTestModal();
    }
  }
  function closeTestModal() {
    closeAllCustomSelects();
    closeDialog('testModal');
  }
  async function runTestAccount(id, model) {
    if (testModalRunning) return;
    testModalRunning = true;
    const modalBtn = $('testRunBtn');
    if (modalBtn) modalBtn.setAttribute('aria-busy', 'true');
    const acc = accountsData.find(a => a.id === id);
    const email = acc ? getDisplayEmail(acc.email, acc.id) : id;
    const proxy = acc ? (acc.proxyURL || t('accounts.testLog.globalProxy')) : '?';
    addTestLog(t('accounts.testLog.start', email, model, proxy), 'info');
    try {
      const startTime = Date.now();
      const res = await api('/accounts/' + id + '/test', { method: 'POST', body: JSON.stringify({ model }) });
      const elapsed = ((Date.now() - startTime) / 1000).toFixed(1);
      const d = await res.json();
      if (d.success) {
        addTestLog(t('accounts.testLog.success', email, elapsed, d.reply), 'ok');
      } else {
        addTestLog(t('accounts.testLog.failed', email, elapsed, d.error || t('common.unknownError')), 'err');
      }
    } catch (e) {
      addTestLog(t('accounts.testLog.error', email, e.message), 'err');
    }
    testModalRunning = false;
    if (modalBtn) modalBtn.removeAttribute('aria-busy');
  }

  // Settings
  async function loadSettings() {
    // Global allowOverUsage was removed (overage is per-account now); just load
    // the remaining settings panels.
    await Promise.all([loadEndpointConfig(), loadProxyConfig(), loadPromptFilter(), loadApiKeys()]);
    refreshCustomSelects();
  }
  async function loadThinkingConfig() {
    const res = await api('/thinking');
    const d = await res.json();
    $('thinkingSuffix').value = d.suffix || '-thinking';
    $('openaiThinkingFormat').value = d.openaiFormat || 'reasoning_content';
    $('claudeThinkingFormat').value = d.claudeFormat || 'thinking';
  }
  async function saveThinkingConfig() {
    const res = await api('/thinking', {
      method: 'POST', body: JSON.stringify({
        suffix: $('thinkingSuffix').value || '-thinking',
        openaiFormat: $('openaiThinkingFormat').value,
        claudeFormat: $('claudeThinkingFormat').value
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.thinkingSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadEndpointConfig() {
    const res = await api('/endpoint');
    const d = await res.json();
    $('preferredEndpoint').value = d.preferredEndpoint || 'auto';
    $('endpointFallback').checked = d.endpointFallback !== false;
  }
  async function saveEndpointConfig() {
    const res = await api('/endpoint', {
      method: 'POST', body: JSON.stringify({
        preferredEndpoint: $('preferredEndpoint').value,
        endpointFallback: $('endpointFallback').checked
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.endpointSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function loadProxyConfig() {
    const res = await api('/proxy');
    const d = await res.json();
    const url = d.proxyURL || '';
    if (!url) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
      return;
    }
    try {
      const u = new URL(url);
      const scheme = u.protocol.replace(':', '');
      $('proxyType').value = scheme.startsWith('socks5') ? 'socks5' : 'http';
      $('proxyHost').value = u.hostname;
      $('proxyPort').value = u.port;
      $('proxyUsername').value = decodeURIComponent(u.username);
      $('proxyPassword').value = decodeURIComponent(u.password);
      $('proxyFields').classList.remove('hidden');
    } catch (e) {
      $('proxyType').value = 'none';
      $('proxyFields').classList.add('hidden');
    }
  }
  function onProxyTypeChange() {
    const type = $('proxyType').value;
    $('proxyFields').classList.toggle('hidden', type === 'none');
  }
  async function saveProxyConfig() {
    const type = $('proxyType').value;
    let url = '';
    if (type !== 'none') {
      const host = $('proxyHost').value.trim();
      const port = $('proxyPort').value.trim();
      if (!host || !port) { toast(t('settings.proxyHostRequired'), 'warning'); return; }
      const u = $('proxyUsername').value.trim();
      const p = $('proxyPassword').value.trim();
      const auth = u ? (p ? encodeURIComponent(u) + ':' + encodeURIComponent(p) + '@' : encodeURIComponent(u) + '@') : '';
      url = type + '://' + auth + host + ':' + port;
    }
    const res = await api('/proxy', { method: 'POST', body: JSON.stringify({ proxyURL: url }) });
    const d = await res.json();
    if (d.success) toast(t('settings.proxySaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  async function saveRequireApiKey() {
    try {
      const requireApiKey = $('requireApiKey').checked;
      if (requireApiKey) {
        const hasEnabledKey = Array.isArray(apiKeysCache) && apiKeysCache.some(k => k && k.enabled);
        if (!hasEnabledKey) {
          const ok = await confirmAction(t('apiKeys.requireWithoutEnabledKeyWarning'), { variant: 'danger' });
          if (!ok) {
            $('requireApiKey').checked = false;
            return;
          }
        }
      }
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ requireApiKey }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      toast(t('detail.saved'), 'success');
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  async function changePassword() {
    const np = $('newPassword').value;
    if (!np) return toast(t('settings.passwordRequired'), 'warning');
    try {
      const res = await api('/settings', { method: 'POST', body: JSON.stringify({ password: np }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      setActivePassword(np, localStorage.getItem('kiro_remember') === '1');
      toast(t('settings.passwordChanged'), 'success');
      $('newPassword').value = '';
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
    }
  }
  // Multi API Key / card billing management
  let apiKeysCache = [];
  let apiKeyEditingId = '';
  let apiKeyModalSubmitting = false;
  let keysFilterKeyword = '';
  let keysFilterStatus = 'all';
  let keysSortBy = 'balance';
  let topupKeyId = '';
  let keyDetailId = '';
  let keyDetailTab = 'summary';
  let keyRechargePage = 1;
  let keyUsagePage = 1;
  const KEY_PAGE_SIZE = 20;

  async function loadApiKeys(quiet) {
    // Any fetch (auto or user-triggered) restarts the slow-tab window.
    slowTabLastFetch.keys = Date.now();
    const list = $('keysList');
    try {
      const res = await api('/api-keys');
      if (!res.ok) throw new Error('http ' + res.status);
      const d = await res.json();
      apiKeysCache = Array.isArray(d.apiKeys) ? d.apiKeys : [];
      renderApiKeys(quiet);
    } catch (e) {
      if (quiet) return; // keep the current list on a transient auto-refresh error
      apiKeysCache = [];
      if (list) list.innerHTML = '<div class="empty-state">' + escapeHtml(t('keys.loadFailed')) + '</div>';
    }
  }
  // Normalize a key entry into a billing view, with graceful fallbacks for the
  // richer fields the backend may not expose yet (granted/balance/expiresAt/...).
  function normalizeKey(item) {
    item = item || {};
    const granted = item.granted != null ? item.granted
      : item.creditsGranted != null ? item.creditsGranted
      : (item.creditLimit || 0);
    const used = item.used != null ? item.used : (item.creditsUsed || 0);
    const balance = item.balance != null ? item.balance : (granted ? granted - used : 0);
    return {
      id: item.id || '',
      name: item.name || '',
      key: item.key || '',
      keyMasked: item.keyMasked || '',
      rpm: item.rpm || 0,
      enabled: !!item.enabled,
      migrated: !!item.migrated,
      granted: granted,
      used: used,
      balance: balance,
      creditLimit: item.creditLimit || 0,
      tokenLimit: item.tokenLimit || 0,
      tokensUsed: item.tokensUsed || 0,
      requestsCount: item.requestsCount || 0,
      expiresAt: item.expiresAt || 0,
      maxConcurrency: item.maxConcurrency != null ? item.maxConcurrency : 0,
      boundAccountIds: Array.isArray(item.boundAccountIds) ? item.boundAccountIds : [],
      parentKeyId: item.parentKeyId || '',
      createdAt: item.createdAt || 0,
      raw: item
    };
  }
  function keyIsExpired(k) {
    return k.expiresAt && k.expiresAt > 0 && k.expiresAt < Date.now() / 1000;
  }
  function toDatetimeLocal(ts) {
    if (!ts) return '';
    const d = new Date(ts * 1000);
    const pad = n => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + 'T' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }
  function fromDatetimeLocal(val) {
    if (!val) return 0;
    const ms = new Date(val).getTime();
    return isNaN(ms) ? 0 : Math.floor(ms / 1000);
  }

  function formatNumber(n) {
    if (n == null || isNaN(n)) return '0';
    if (Math.abs(n) >= 1 && Math.floor(n) === n) return Number(n).toLocaleString('en-US');
    return Number(n).toLocaleString('en-US', { maximumFractionDigits: 1 });
  }

  function usageBar(used, limit) {
    if (!limit || limit <= 0) return '';
    const ratio = Math.max(0, Math.min(1, used / limit));
    const pct = (ratio * 100).toFixed(1);
    let color = '#3b82f6';
    if (ratio >= 0.95) color = '#ef4444';
    else if (ratio >= 0.8) color = '#f59e0b';
    return '<div style="height:6px;background:rgba(127,127,127,0.2);border-radius:3px;overflow:hidden;margin-top:4px;">' +
      '<div style="height:100%;width:' + pct + '%;background:' + color + ';transition:width 0.3s;"></div>' +
      '</div>';
  }

  function usageLine(label, used, limit, options) {
    options = options || {};
    const fmt = options.fmt || formatNumber;
    if (!limit || limit <= 0) {
      return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(t('apiKeys.unlimited')) + '</div>';
    }
    return '<div class="text-xs muted-text">' + escapeHtml(label) + ': ' + escapeHtml(fmt(used)) + ' / ' + escapeHtml(fmt(limit)) + '</div>' + usageBar(used, limit);
  }

  function keyBillingItem(label, value, variant) {
    return '<div class="key-billing-item' + (variant ? ' key-billing-item--' + variant : '') + '">' +
      '<div class="key-billing-value">' + escapeHtml(String(value)) + '</div>' +
      '<div class="key-billing-label">' + escapeHtml(label) + '</div>' +
      '</div>';
  }
  function keyBoundLabel(k) {
    if (!k.boundAccountIds.length) return t('keys.boundNone');
    if (k.boundAccountIds.length <= 2) {
      return k.boundAccountIds.map(id => {
        const acc = accountsData.find(a => a.id === id);
        return acc ? getDisplayEmail(acc.email, acc.id) : id.slice(0, 8);
      }).join(', ');
    }
    return t('keys.boundCount', k.boundAccountIds.length);
  }
  function keyParentLabel(parentId) {
    if (!parentId) return '';
    const p = apiKeysCache.find(x => x.id === parentId);
    return p ? (p.name || t('keys.unnamed')) : parentId.slice(0, 8);
  }
  function getFilteredKeys() {
    const kw = keysFilterKeyword.trim().toLowerCase();
    const list = apiKeysCache.map(normalizeKey).filter(k => {
      if (keysFilterStatus === 'active' && (!k.enabled || keyIsExpired(k))) return false;
      if (keysFilterStatus === 'disabled' && k.enabled) return false;
      if (keysFilterStatus === 'expired' && !keyIsExpired(k)) return false;
      if (keysFilterStatus === 'sub' && !k.parentKeyId) return false;
      if (kw && !((k.name + ' ' + k.key + ' ' + k.keyMasked).toLowerCase().includes(kw))) return false;
      return true;
    });
    const balanceKey = k => (k.granted > 0 ? k.balance : Infinity);
    list.sort((a, b) => {
      switch (keysSortBy) {
        case 'rpm': return (b.rpm || 0) - (a.rpm || 0);
        case 'createdDesc': return (b.createdAt || 0) - (a.createdAt || 0);
        case 'balance':
        default: return balanceKey(b) - balanceKey(a);
      }
    });
    return list;
  }
  function renderApiKeys(quiet) {
    renderKeysStats();
    const list = $('keysList');
    if (!list) return;
    const keys = getFilteredKeys();
    // Quiet auto-refresh: skip the rebuild (keeping scroll/focus) unless the key
    // data, filter, sort or language changed.
    const sig = currentLang + '|' + keysFilterStatus + '|' + keysFilterKeyword + '|' + keysSortBy + '|' + JSON.stringify(apiKeysCache);
    if (quiet && sig === lastKeysSig && list.childElementCount) return;
    lastKeysSig = sig;
    if (!keys.length) {
      list.innerHTML = '<div class="empty-state">' + escapeHtml(t('keys.empty')) + '</div>';
      return;
    }
    // Parent/child pool math is computed over ALL keys (not just the filtered
    // view) so parent badges + pool usage stay correct even while filtered.
    const allKeys = apiKeysCache.map(normalizeKey);
    const parentIds = new Set();
    const childGrantByParent = {};
    const childCountByParent = {};
    allKeys.forEach(x => {
      if (!x.parentKeyId) return;
      parentIds.add(x.parentKeyId);
      childGrantByParent[x.parentKeyId] = (childGrantByParent[x.parentKeyId] || 0) + (x.granted > 0 ? x.granted : 0);
      childCountByParent[x.parentKeyId] = (childCountByParent[x.parentKeyId] || 0) + 1;
    });
    // Group the filtered rows: each root key is followed by its filtered
    // children (indented). A child whose parent is filtered out shows standalone.
    const inView = new Map(keys.map(x => [x.id, x]));
    const childrenOf = {};
    keys.forEach(x => { if (x.parentKeyId) (childrenOf[x.parentKeyId] = childrenOf[x.parentKeyId] || []).push(x); });
    const emitted = new Set();
    const ordered = [];
    keys.forEach(x => {
      if (x.parentKeyId && inView.has(x.parentKeyId)) return; // emitted under its parent
      if (emitted.has(x.id)) return;
      ordered.push({ k: x, isChild: false });
      emitted.add(x.id);
      (childrenOf[x.id] || []).forEach(c => { if (!emitted.has(c.id)) { ordered.push({ k: c, isChild: true }); emitted.add(c.id); } });
    });
    keys.forEach(x => { if (!emitted.has(x.id)) { ordered.push({ k: x, isChild: !!x.parentKeyId }); emitted.add(x.id); } });
    const scrollY = window.scrollY;
    list.innerHTML = ordered.map(o => renderKeyRow(o.k, {
      isParent: parentIds.has(o.k.id),
      isChild: o.isChild,
      childGrant: childGrantByParent[o.k.id] || 0,
      childCount: childCountByParent[o.k.id] || 0
    })).join('');
    applyUsageBars(list);
    if (quiet) window.scrollTo(0, scrollY);
  }
  // One dense row per card. Preserves all actions (toggle / topup / detail /
  // edit / reset / delete / copy key+url) and shows sub-card / parent lineage.
  function renderKeyRow(k, info) {
    info = info || {};
    const id = escapeAttr(k.id);
    const nameHtml = k.name ? escapeHtml(k.name) : '<span class="muted-text">' + escapeHtml(t('keys.unnamed')) + '</span>';
    const expired = keyIsExpired(k);
    const tags = [];
    if (info.isParent) tags.push('<span class="key-tag key-tag-parent"><i class="fa-solid fa-sitemap"></i>' + escapeHtml(t('keys.parentTag')) + '</span>');
    if (k.parentKeyId) tags.push('<span class="key-tag key-tag-sub"><i class="fa-solid fa-code-branch"></i>' + escapeHtml(t('keys.subKey')) + '</span>');
    if (k.migrated) tags.push('<span class="key-tag key-tag-migrated">' + escapeHtml(t('keys.migrated')) + '</span>');
    if (!k.enabled) tags.push('<span class="key-tag key-tag-disabled">' + escapeHtml(t('keys.disabled')) + '</span>');
    if (expired) tags.push('<span class="key-tag key-tag-expired">' + escapeHtml(t('keys.expired')) + '</span>');

    const grantedPct = k.granted > 0 ? Math.min(100, (k.used / k.granted) * 100) : 0;
    const usageClass = grantedPct > 90 ? 'critical' : grantedPct > 70 ? 'high' : '';
    const grantedText = k.granted > 0 ? formatNumber(k.granted) : t('keys.unlimited');
    // balance = granted - used; ∞ when unlimited; may be negative if overspent.
    const balanceText = k.granted > 0 ? formatNumber(k.balance) : '\u221e';
    const balanceCls = (k.granted <= 0 || k.balance >= 0) ? 'success-text' : '';
    const usageExtra = k.granted > 0 ? '<div class="ng-usage-mini"><div class="usage-fill ' + usageClass + '" data-usage-pct="' + escapeAttr(grantedPct) + '"></div></div>' : '';
    const rpmVal = escapeHtml(String(k.rpm));
    // Two soft-tinted data blocks: 计费 (balance / quota / used) and 流量
    // (tokens / requests / rpm), so money and traffic read as distinct groups.
    // Blocks use compact numbers (1.7B / 21.9K) so each fixed-width column fits
    // without truncation; full values remain available via title tooltip.
    const balanceFull = k.granted > 0 ? formatNumber(k.balance) : '\u221e';
    const grantedFull = k.granted > 0 ? formatNumber(k.granted) : t('keys.unlimited');
    const balanceCompact = k.granted > 0 ? formatNum(k.balance) : '\u221e';
    const grantedCompact = k.granted > 0 ? formatNum(k.granted) : t('keys.unlimited');
    const creditBlock = '<div class="ng-block ng-block--credit">' +
      ngMetric(escapeHtml(balanceCompact), t('keys.balance'), '', balanceCls, balanceFull) +
      ngMetric(escapeHtml(grantedCompact), t('keys.granted'), usageExtra, '', grantedFull) +
      ngMetric(escapeHtml(formatNum(k.used)), t('keys.used'), '', '', formatNumber(k.used)) +
      '</div>';
    const trafficBlock = '<div class="ng-block ng-block--traffic">' +
      ngMetric(escapeHtml(formatNum(k.tokensUsed)), t('keys.tokens'), '', '', formatNumber(k.tokensUsed)) +
      ngMetric(escapeHtml(formatNum(k.requestsCount)), t('keys.requests'), '', '', formatNumber(k.requestsCount)) +
      ngMetric(rpmVal, t('keys.rpmLabel')) +
      '</div>';
    const blocks = creditBlock + trafficBlock;
    const dotCls = keyDotClass(k, expired);

    const mcLabel = k.maxConcurrency === -1 ? t('keys.unlimited') : (k.maxConcurrency > 0 ? String(k.maxConcurrency) : t('keys.concurrencyDefault'));
    const fullKey = k.key || k.keyMasked || '';
    const seg = (html) => '<span class="ng-sub-seg">' + html + '</span>';
    // Line 1: the key value + copy buttons.
    const keySegs = [];
    keySegs.push('<code class="key-inline">' + escapeHtml(fullKey) + '</code>');
    keySegs.push('<button class="ng-copy-btn ng-copy-xs" type="button" data-key-action="copyKey" data-id="' + id + '"><i class="fa-regular fa-copy"></i>' + escapeHtml(t('keys.copyKey')) + '</button>');
    keySegs.push('<button class="ng-copy-btn ng-copy-xs" type="button" data-key-action="copyUrl" data-id="' + id + '"><i class="fa-solid fa-link"></i>' + escapeHtml(t('keys.copyUrl')) + '</button>');
    // Line 2: metadata grouped as one compact strip (过期 · 并发 · 绑定账号 …).
    const metaSegs = [];
    metaSegs.push(seg('<i class="fa-regular fa-clock"></i>' + escapeHtml(t('keys.expiresAt') + ': ' + (k.expiresAt ? formatDateTime(k.expiresAt) : t('keys.neverExpires')))));
    metaSegs.push(seg('<i class="fa-solid fa-layer-group"></i>' + escapeHtml(t('keys.maxConcurrency') + ': ' + mcLabel)));
    metaSegs.push(seg('<i class="fa-solid fa-users"></i>' + escapeHtml(t('keys.boundAccounts') + ': ' + keyBoundLabel(k))));
    if (k.parentKeyId) metaSegs.push(seg('<i class="fa-solid fa-code-branch"></i>' + escapeHtml(t('keys.parentKey') + ': ' + keyParentLabel(k.parentKeyId))));
    if (info.isParent) {
      const poolTxt = t('keys.childAllocated') + ' ' + formatNumber(info.childGrant) + ' / ' + (k.granted > 0 ? formatNumber(k.granted) : '\u221e') + ' \u00b7 ' + t('keys.childCount', info.childCount || 0);
      metaSegs.push(seg('<i class="fa-solid fa-sitemap"></i>' + escapeHtml(poolTxt)));
    }

    const actions =
      '<label class="switch" title="' + escapeAttr(k.enabled ? t('accounts.disable') : t('accounts.enable')) + '"><input type="checkbox" data-key-action="toggle" data-id="' + id + '"' + (k.enabled ? ' checked' : '') + ' /><span class="slider"></span></label>' +
      '<button class="btn btn-primary btn-sm" type="button" data-key-action="topup" data-id="' + id + '"><i class="fa-solid fa-wallet"></i><span>' + escapeHtml(t('keys.topup')) + '</span></button>' +
      '<button class="btn btn-outline btn-sm" type="button" data-key-action="detail" data-id="' + id + '"><i class="fa-solid fa-chart-line"></i><span>' + escapeHtml(t('keys.detail')) + '</span></button>' +
      '<button class="btn btn-outline btn-sm" type="button" data-key-action="edit" data-id="' + id + '">' + escapeHtml(t('keys.edit')) + '</button>' +
      '<button class="btn btn-outline btn-sm" type="button" data-key-action="reset" data-id="' + id + '">' + escapeHtml(t('keys.reset')) + '</button>' +
      '<button class="btn btn-danger btn-sm" type="button" data-key-action="delete" data-id="' + id + '">' + escapeHtml(t('keys.delete')) + '</button>';

    const rowCls = 'key-card' + (k.enabled ? '' : ' is-disabled') + (expired ? ' is-expired' : '') +
      (k.parentKeyId ? ' is-sub' : '') + (info.isParent ? ' is-parent' : '') + (info.isChild ? ' key-child-row' : '');
    return '<div class="' + rowCls + '" data-key-id="' + id + '">' +
      (info.isChild ? '<span class="key-child-connector" aria-hidden="true">\u21b3</span>' : '') +
      '<div class="ng-row-main">' +
        '<div class="ng-row-title"><span class="ng-status-dot ' + dotCls + '" aria-hidden="true"></span><span class="ng-row-name">' + nameHtml + '</span></div>' +
        (tags.length ? '<div class="ng-row-tags">' + tags.join('') + '</div>' : '') +
        '<div class="ng-row-sub ng-row-sub--key">' + keySegs.join('') + '</div>' +
        '<div class="ng-row-sub ng-row-sub--meta">' + metaSegs.join('') + '</div>' +
      '</div>' +
      '<div class="ng-row-blocks">' + blocks + '</div>' +
      '<div class="ng-row-actions">' + actions + '</div>' +
    '</div>';
  }

  function markKeyCopied(btn) {
    if (!btn) return;
    btn.classList.add('ng-copied');
    setTimeout(() => btn.classList.remove('ng-copied'), 1200);
  }
  async function copyKeyValue(entry, btn) {
    const val = entry && (entry.key || entry.keyMasked) || '';
    if (!val) { toastWarning(t('common.failed')); return; }
    try { await copyText(val); markKeyCopied(btn); toast(t('keys.copyKeyDone'), 'primary'); }
    catch (e) { toastError(t('common.failed')); }
  }
  async function copyKeyBaseUrl(btn) {
    try { await copyText(location.origin); markKeyCopied(btn); toast(t('keys.copyUrlDone'), 'primary'); }
    catch (e) { toastError(t('common.failed')); }
  }
  // Max-concurrency control: default(0) / custom(N) / unlimited(-1)
  function updateMaxConcurrencyField() {
    const mode = $('apiKeyForm_maxConcurrencyMode');
    const input = $('apiKeyForm_maxConcurrency');
    if (!mode || !input) return;
    input.classList.toggle('hidden', mode.value !== 'custom');
  }

  function openApiKeyModal(entry) {
    apiKeyEditingId = entry ? (entry.id || '') : '';
    const titleEl = $('apiKeyModalTitle');
    titleEl.textContent = t(apiKeyEditingId ? 'apiKeys.modalTitleEdit' : 'apiKeys.modalTitleCreate');
    $('apiKeyForm_name').value = entry ? (entry.name || '') : '';
    const keyEl = $('apiKeyForm_key');
    if (apiKeyEditingId) {
      keyEl.value = entry.keyMasked || '';
      keyEl.readOnly = true;
    } else {
      keyEl.value = '';
      keyEl.readOnly = false;
    }
    $('apiKeyForm_enabled').checked = entry ? !!entry.enabled : true;
    $('apiKeyForm_tokenLimit').value = entry ? String(entry.tokenLimit || 0) : '0';
    // "额度" is the card's total credit grant (CreditsGranted), which drives both
    // the displayed balance and quota enforcement. Pre-fill from the current grant
    // so editing sets an absolute quota (recharges are preserved when left as-is);
    // fall back to the legacy creditLimit only when no grant is present.
    $('apiKeyForm_creditLimit').value = entry ? String((entry.granted != null ? entry.granted : entry.creditLimit) || 0) : '0';
    const mc = entry && entry.maxConcurrency != null ? entry.maxConcurrency : 0;
    const mcMode = $('apiKeyForm_maxConcurrencyMode');
    const mcInput = $('apiKeyForm_maxConcurrency');
    if (mc === -1) { if (mcMode) mcMode.value = '-1'; if (mcInput) mcInput.value = '0'; }
    else if (mc > 0) { if (mcMode) mcMode.value = 'custom'; if (mcInput) mcInput.value = String(mc); }
    else { if (mcMode) mcMode.value = '0'; if (mcInput) mcInput.value = '0'; }
    updateMaxConcurrencyField();
    $('apiKeyForm_expiresAt').value = entry ? toDatetimeLocal(entry.expiresAt) : '';
    populateParentKeySelect(apiKeyEditingId, entry ? (entry.parentKeyId || '') : '');
    populateBoundAccounts(entry && Array.isArray(entry.boundAccountIds) ? entry.boundAccountIds : []);
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
    openDialog('apiKeyModal');
    refreshCustomSelects($('apiKeyModal'));
  }
  function populateParentKeySelect(currentId, selectedParentId) {
    const sel = $('apiKeyForm_parentKey');
    if (!sel) return;
    let html = '<option value="">' + escapeHtml(t('apiKeys.formParentNone')) + '</option>';
    apiKeysCache.forEach(item => {
      if (item.id === currentId) return;   // never self
      if (item.parentKeyId) return;        // only root cards can be a parent
      const label = (item.name || t('keys.unnamed')) + (item.keyMasked ? ' \u00b7 ' + item.keyMasked : '');
      html += '<option value="' + escapeAttr(item.id) + '"' + (item.id === selectedParentId ? ' selected' : '') + '>' + escapeHtml(label) + '</option>';
    });
    sel.innerHTML = html;
    sel.value = selectedParentId || '';
  }
  function populateBoundAccounts(selectedIds) {
    const box = $('apiKeyForm_boundAccounts');
    if (!box) return;
    const set = new Set(selectedIds || []);
    if (!accountsData.length) {
      box.innerHTML = '<div class="muted-text text-xs bound-accounts-empty">' + escapeHtml(t('apiKeys.formBoundAccountsEmpty')) + '</div>';
      return;
    }
    box.innerHTML = accountsData.map(a => {
      const id = escapeAttr(a.id);
      const label = getDisplayEmail(a.email, a.id);
      return '<label class="bound-account-item">' +
        '<input type="checkbox" value="' + id + '"' + (set.has(a.id) ? ' checked' : '') + ' />' +
        '<span class="bound-account-email">' + escapeHtml(label) + '</span>' +
        '</label>';
    }).join('');
  }

  function closeApiKeyModal() {
    closeDialog('apiKeyModal');
    apiKeyEditingId = '';
    apiKeyModalSubmitting = false;
    $('apiKeyModalSaveBtn').disabled = false;
  }

  async function submitApiKeyModal() {
    if (apiKeyModalSubmitting) return;
    apiKeyModalSubmitting = true;
    const saveBtn = $('apiKeyModalSaveBtn');
    saveBtn.disabled = true;
    try {
      const name = $('apiKeyForm_name').value.trim();
      const enabled = $('apiKeyForm_enabled').checked;
      const tokenLimit = parseInt($('apiKeyForm_tokenLimit').value, 10);
      // "额度" quota drives the unified ledger (CreditsGranted). Send it as both
      // creditsGranted (authoritative grant) and creditLimit (legacy mirror) so a
      // created/edited card shows and enforces exactly what the operator typed.
      const creditLimit = parseFloat($('apiKeyForm_creditLimit').value);
      const creditQuota = isNaN(creditLimit) || creditLimit < 0 ? 0 : creditLimit;
      const mcMode = ($('apiKeyForm_maxConcurrencyMode') && $('apiKeyForm_maxConcurrencyMode').value) || '0';
      let maxConcurrency;
      if (mcMode === '-1') {
        maxConcurrency = -1;
      } else if (mcMode === 'custom') {
        const n = parseInt($('apiKeyForm_maxConcurrency').value, 10);
        maxConcurrency = (isNaN(n) || n < 1) ? 0 : n;
      } else {
        maxConcurrency = 0;
      }
      const expiresAt = fromDatetimeLocal($('apiKeyForm_expiresAt').value);
      const parentKeyId = ($('apiKeyForm_parentKey').value || '').trim();
      const boundAccountIds = qsa('#apiKeyForm_boundAccounts input[type="checkbox"]:checked').map(cb => cb.value);
      const payload = {
        name: name,
        enabled: enabled,
        tokenLimit: isNaN(tokenLimit) || tokenLimit < 0 ? 0 : tokenLimit,
        creditLimit: creditQuota,
        creditsGranted: creditQuota,
        maxConcurrency: maxConcurrency,
        expiresAt: expiresAt,
        parentKeyId: parentKeyId,
        boundAccountIds: boundAccountIds
      };
      let res, d;
      if (apiKeyEditingId) {
        res = await api('/api-keys/' + encodeURIComponent(apiKeyEditingId), { method: 'PUT', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        toast(t('apiKeys.updated'), 'success');
        closeApiKeyModal();
        await loadApiKeys();
      } else {
        const keyVal = $('apiKeyForm_key').value.trim();
        if (keyVal) payload.key = keyVal;
        res = await api('/api-keys', { method: 'POST', body: JSON.stringify(payload) });
        d = await res.json().catch(() => ({}));
        if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
        closeApiKeyModal();
        await loadApiKeys();
        // Auto-copy the freshly created key to the clipboard so the operator can
        // paste it straight away. The cleartext key is only returned once, on
        // creation (d.key). Fall back to the "copy from the list" hint if the
        // response carried no key or the clipboard write failed.
        const createdKey = (d && (d.key || d.apiKey && d.apiKey.key)) || '';
        let copied = false;
        if (createdKey) {
          try { await copyText(createdKey); copied = true; } catch (e) { }
        }
        toast(t(copied ? 'apiKeys.createdCopied' : 'apiKeys.createdCopyHint'), 'success');
      }
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      apiKeyModalSubmitting = false;
      saveBtn.disabled = false;
    }
  }

  async function toggleApiKeyEntry(id, enabled) {
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'PUT', body: JSON.stringify({ enabled }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.saveFailed'));
      const item = apiKeysCache.find(x => x.id === id);
      if (item) item.enabled = enabled;
      renderApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.saveFailed'), 'error');
      await loadApiKeys();
    }
  }

  async function deleteApiKeyEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmDelete', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionDelete'),
      confirmText: t('apiKeys.actionDelete'),
      variant: 'danger'
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id), { method: 'DELETE' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.deleteSuccess'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  async function resetApiKeyUsageEntry(id, name) {
    const ok = await confirmAction(t('apiKeys.confirmReset', name || t('apiKeys.unnamed')), {
      title: t('apiKeys.actionReset'),
      confirmText: t('apiKeys.actionReset')
    });
    if (!ok) return;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(id) + '/reset-usage', { method: 'POST' });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('apiKeys.usageReset'), 'success');
      await loadApiKeys();
    } catch (e) {
      toast((e && e.message) || t('common.failed'), 'error');
    }
  }

  // ── Top-up ──
  function openTopupModal(id) {
    const entry = apiKeysCache.find(x => x.id === id);
    topupKeyId = id;
    $('topupCardName').textContent = entry ? (entry.name || t('keys.unnamed')) : id;
    $('topupAmount').value = '';
    $('topupNote').value = '';
    openDialog('topupModal');
    setTimeout(() => { const el = $('topupAmount'); if (el) el.focus(); }, 30);
  }
  function closeTopupModal() { closeDialog('topupModal'); topupKeyId = ''; }
  async function submitTopup() {
    const amount = parseFloat($('topupAmount').value);
    if (isNaN(amount) || amount <= 0) { toastWarning(t('topup.invalidAmount')); return; }
    const note = $('topupNote').value.trim();
    const btn = $('topupSubmitBtn');
    if (btn) btn.disabled = true;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(topupKeyId) + '/topup', { method: 'POST', body: JSON.stringify({ amount, note }) });
      if (res.status === 404) throw new Error(t('topup.notAvailable'));
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      toast(t('topup.success'), 'success');
      closeTopupModal();
      await loadApiKeys();
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  // ── Card detail (usage summary / recharges / usage records / sub-cards) ──
  function openKeyDetail(id) {
    keyDetailId = id;
    keyDetailTab = 'summary';
    keyRechargePage = 1;
    keyUsagePage = 1;
    const entry = apiKeysCache.find(x => x.id === id);
    const nameEl = $('keyDetailName');
    if (nameEl) nameEl.textContent = entry ? (entry.name || t('keys.unnamed')) : id;
    qsa('#keyDetailSubtabs .key-subtab').forEach(b => b.classList.toggle('active', b.dataset.subtab === 'summary'));
    openDialog('keyDetailModal');
    renderKeyDetailTab();
  }
  function switchKeyDetailTab(tab) {
    keyDetailTab = tab;
    qsa('#keyDetailSubtabs .key-subtab').forEach(b => b.classList.toggle('active', b.dataset.subtab === tab));
    renderKeyDetailTab();
  }
  function keyDetailLoading() {
    return '<div class="api-view-loading"><i class="fa-solid fa-spinner fa-spin"></i> ' + escapeHtml(t('detail.loading')) + '</div>';
  }
  function keyDetailUnavailable() {
    return '<div class="empty-state key-detail-unavailable"><i class="fa-solid fa-plug-circle-xmark"></i><span>' + escapeHtml(t('keyDetail.notAvailable')) + '</span></div>';
  }
  function renderKeyDetailTab() {
    const c = $('keyDetailContent');
    if (!c) return;
    c.innerHTML = keyDetailLoading();
    if (keyDetailTab === 'summary') renderKeySummary(c);
    else if (keyDetailTab === 'recharges') renderKeyRecharges(c);
    else if (keyDetailTab === 'usage') renderKeyUsageRecords(c);
    else if (keyDetailTab === 'children') renderKeyChildren(c);
  }
  function keySummaryCard(label, value, variant) {
    return '<div class="key-summary-item' + (variant ? ' key-summary-item--' + variant : '') + '">' +
      '<div class="key-summary-value">' + escapeHtml(String(value)) + '</div>' +
      '<div class="key-summary-label">' + escapeHtml(label) + '</div>' +
      '</div>';
  }
  async function renderKeySummary(c) {
    let d = null;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(keyDetailId) + '/usage');
      if (res.ok) d = await res.json();
    } catch (e) { /* fall back to local key data */ }
    const entry = normalizeKey(apiKeysCache.find(x => x.id === keyDetailId));
    const granted = d && d.creditsGranted != null ? d.creditsGranted : entry.granted;
    const used = d && d.creditsUsed != null ? d.creditsUsed : entry.used;
    const balance = d && d.balance != null ? d.balance : entry.balance;
    const tokensUsed = d && d.tokensUsed != null ? d.tokensUsed : entry.tokensUsed;
    const requestsCount = d && d.requestsCount != null ? d.requestsCount : entry.requestsCount;
    const byModel = d && Array.isArray(d.byModel) ? d.byModel : [];
    const cacheHitRate = d && typeof d.cacheHitRate === 'number' ? d.cacheHitRate : null;
    const chrTxt = cacheHitRate == null ? '\u2014' : (cacheHitRate * 100).toFixed(1) + '%';
    const chrColor = cacheHitRate == null ? '' : (cacheHitRate >= 0.6 ? '#16a34a' : cacheHitRate >= 0.3 ? '#ca8a04' : '#dc2626');
    let html = '<div class="key-summary-grid">';
    html += keySummaryCard(t('keyDetail.summaryBalance'), granted > 0 ? formatNumber(balance) : '\u221e', 'balance');
    html += keySummaryCard(t('keyDetail.summaryGranted'), granted > 0 ? formatNumber(granted) : t('keys.unlimited'), '');
    html += keySummaryCard(t('keyDetail.summaryUsed'), formatNumber(used), 'used');
    html += keySummaryCard(t('keyDetail.summaryTokens'), formatNumber(tokensUsed), '');
    html += keySummaryCard(t('keyDetail.summaryRequests'), formatNumber(requestsCount), '');
    html += '<div class="key-summary-item"><div class="key-summary-value"' + (chrColor ? ' style="color:' + chrColor + '"' : '') + '>' + escapeHtml(chrTxt) + '</div>' +
      '<div class="key-summary-label">' + escapeHtml(t('keyDetail.summaryCacheHit')) + '</div></div>';
    html += '</div>';
    if (byModel.length) {
      html += '<div class="key-detail-subheading">' + escapeHtml(t('keyDetail.byModel')) + '</div>';
      html += '<table class="data-table"><thead><tr>' +
        '<th>' + escapeHtml(t('usageRec.model')) + '</th>' +
        '<th class="ta-right">' + escapeHtml(t('usageRec.credits')) + '</th>' +
        '<th class="ta-right">' + escapeHtml(t('keys.requests')) + '</th>' +
        '</tr></thead><tbody>';
      byModel.forEach(m => {
        html += '<tr>' +
          '<td class="font-mono">' + escapeHtml(m.model || '-') + '</td>' +
          '<td class="ta-right">' + escapeHtml(formatNumber(m.credits || 0)) + '</td>' +
          '<td class="ta-right">' + escapeHtml(formatNumber(m.requests || 0)) + '</td>' +
          '</tr>';
      });
      html += '</tbody></table>';
    }
    c.innerHTML = html;
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
  async function renderKeyRecharges(c) {
    let d = null, unavailable = false;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(keyDetailId) + '/recharges?page=' + keyRechargePage + '&pageSize=' + KEY_PAGE_SIZE);
      if (res.status === 404) unavailable = true;
      else if (res.ok) d = await res.json();
      else unavailable = true;
    } catch (e) { unavailable = true; }
    if (unavailable) { c.innerHTML = keyDetailUnavailable(); return; }
    const records = d && Array.isArray(d.records) ? d.records : [];
    const total = d && d.total != null ? d.total : records.length;
    if (!records.length) { c.innerHTML = '<div class="empty-state">' + escapeHtml(t('keyDetail.empty')) + '</div>'; return; }
    let html = '<table class="data-table"><thead><tr>' +
      '<th>' + escapeHtml(t('recharge.time')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('recharge.amount')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('recharge.balanceAfter')) + '</th>' +
      '<th>' + escapeHtml(t('recharge.operator')) + '</th>' +
      '<th>' + escapeHtml(t('recharge.note')) + '</th>' +
      '</tr></thead><tbody>';
    records.forEach(r => {
      html += '<tr>' +
        '<td>' + escapeHtml(formatDateTime(r.createdAt)) + '</td>' +
        '<td class="ta-right data-pos">+' + escapeHtml(formatNumber(r.amount || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.balanceAfter || 0)) + '</td>' +
        '<td>' + escapeHtml(r.operator || '-') + '</td>' +
        '<td class="data-note">' + escapeHtml(r.note || '-') + '</td>' +
        '</tr>';
    });
    html += '</tbody></table>' + pagerHtml('recharge', keyRechargePage, total);
    c.innerHTML = html;
  }
  async function renderKeyUsageRecords(c) {
    let d = null, unavailable = false;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(keyDetailId) + '/usage/records?page=' + keyUsagePage + '&pageSize=' + KEY_PAGE_SIZE);
      if (res.status === 404) unavailable = true;
      else if (res.ok) d = await res.json();
      else unavailable = true;
    } catch (e) { unavailable = true; }
    if (unavailable) { c.innerHTML = keyDetailUnavailable(); return; }
    const records = d && Array.isArray(d.records) ? d.records : [];
    const total = d && d.total != null ? d.total : records.length;
    if (!records.length) { c.innerHTML = '<div class="empty-state">' + escapeHtml(t('keyDetail.empty')) + '</div>'; return; }
    let html = '<table class="data-table"><thead><tr>' +
      '<th>' + escapeHtml(t('usageRec.time')) + '</th>' +
      '<th>' + escapeHtml(t('usageRec.model')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.input')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.output')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.cacheRead')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('usageRec.credits')) + '</th>' +
      '</tr></thead><tbody>';
    records.forEach(r => {
      html += '<tr>' +
        '<td>' + escapeHtml(formatDateTime(r.createdAt)) + '</td>' +
        '<td class="font-mono">' + escapeHtml(r.model || '-') + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.inputTokens || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.outputTokens || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.cacheReadInputTokens || 0)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(r.credits || 0)) + '</td>' +
        '</tr>';
    });
    html += '</tbody></table>' + pagerHtml('usage', keyUsagePage, total);
    c.innerHTML = html;
  }
  async function renderKeyChildren(c) {
    let d = null, unavailable = false;
    try {
      const res = await api('/api-keys/' + encodeURIComponent(keyDetailId) + '/children');
      if (res.status === 404) unavailable = true;
      else if (res.ok) d = await res.json();
      else unavailable = true;
    } catch (e) { unavailable = true; }
    let children = [];
    if (d) children = Array.isArray(d.children) ? d.children : (Array.isArray(d.records) ? d.records : (Array.isArray(d) ? d : []));
    // Fall back to deriving children from the local cache via parentKeyId.
    if (!children.length) children = apiKeysCache.filter(k => k.parentKeyId === keyDetailId);
    let html = '<div class="key-children-head"><button class="btn btn-primary btn-sm" type="button" data-child-create="1"><i class="fa-solid fa-plus"></i>' + escapeHtml(t('children.create')) + '</button></div>';
    if (!children.length) {
      html += '<div class="empty-state">' + escapeHtml(t('children.empty')) + '</div>';
      c.innerHTML = html;
      return;
    }
    const parent = normalizeKey(apiKeysCache.find(x => x.id === keyDetailId));
    html += '<table class="data-table"><thead><tr>' +
      '<th>' + escapeHtml(t('children.name')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('children.granted')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('children.used')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('children.balance')) + '</th>' +
      '<th class="ta-right">' + escapeHtml(t('children.poolShare')) + '</th>' +
      '</tr></thead><tbody>';
    children.forEach(raw => {
      const k = normalizeKey(raw);
      const share = parent.granted > 0 ? ((k.granted / parent.granted) * 100).toFixed(1) + '%' : '-';
      html += '<tr>' +
        '<td>' + escapeHtml(k.name || t('keys.unnamed')) + '</td>' +
        '<td class="ta-right">' + escapeHtml(k.granted > 0 ? formatNumber(k.granted) : t('keys.unlimited')) + '</td>' +
        '<td class="ta-right">' + escapeHtml(formatNumber(k.used)) + '</td>' +
        '<td class="ta-right">' + escapeHtml(k.granted > 0 ? formatNumber(k.balance) : '\u221e') + '</td>' +
        '<td class="ta-right">' + escapeHtml(share) + '</td>' +
        '</tr>';
    });
    html += '</tbody></table>';
    c.innerHTML = html;
  }
  function onKeyDetailContentClick(e) {
    const pager = e.target.closest('[data-pager]');
    if (pager && !pager.disabled) {
      const dir = parseInt(pager.dataset.dir, 10) || 0;
      if (pager.dataset.pager === 'recharge') keyRechargePage = Math.max(1, keyRechargePage + dir);
      else if (pager.dataset.pager === 'usage') keyUsagePage = Math.max(1, keyUsagePage + dir);
      renderKeyDetailTab();
      return;
    }
    const childCreate = e.target.closest('[data-child-create]');
    if (childCreate) {
      const parentId = keyDetailId;
      closeDialog('keyDetailModal');
      openApiKeyModal(null);
      const sel = $('apiKeyForm_parentKey');
      if (sel) { sel.value = parentId; refreshCustomSelects($('apiKeyModal')); }
    }
  }

  // ── Concurrency / sticky cache board (read-only, gracefully hidden) ──
  function concurrencyItem(label, value, variant) {
    return '<div class="concurrency-item' + (variant ? ' concurrency-item--' + variant : '') + '">' +
      '<div class="concurrency-value">' + escapeHtml(String(value)) + '</div>' +
      '<div class="concurrency-label">' + escapeHtml(label) + '</div>' +
      '</div>';
  }
  function renderConcurrency(grid, d) {
    d = d || {};
    const hits = d.stickyHits || 0;
    const misses = d.stickyMisses || 0;
    const totalSticky = hits + misses;
    const rate = totalSticky > 0 ? ((hits / totalSticky) * 100).toFixed(1) + '%' : '-';
    const sumVals = obj => (obj && typeof obj === 'object') ? Object.values(obj).reduce((a, b) => a + (Number(b) || 0), 0) : 0;
    grid.innerHTML =
      concurrencyItem(t('concurrency.stickyHits'), formatNumber(hits), 'success') +
      concurrencyItem(t('concurrency.stickyMisses'), formatNumber(misses), '') +
      concurrencyItem(t('concurrency.hitRate'), rate, 'info') +
      concurrencyItem(t('concurrency.inflightAccounts'), formatNumber(sumVals(d.inflightByAccount)), '') +
      concurrencyItem(t('concurrency.inflightKeys'), formatNumber(sumVals(d.inflightByKey)), '');
  }
  async function loadConcurrency() {
    const card = $('concurrencyCard');
    const grid = $('concurrencyGrid');
    if (!card || !grid) return;
    try {
      const res = await api('/concurrency');
      if (!res.ok) throw new Error('unavailable');
      const d = await res.json();
      renderConcurrency(grid, d);
      card.classList.remove('hidden');
    } catch (e) {
      card.classList.add('hidden');
    }
  }

  function bindApiKeyEvents() {
    const list = $('keysList');
    if (list) {
      list.addEventListener('click', e => {
        const btn = e.target.closest('[data-key-action]');
        if (!btn) return;
        const action = btn.dataset.keyAction;
        const id = btn.dataset.id;
        if (!id || action === 'toggle') return;
        const entry = apiKeysCache.find(x => x.id === id);
        const name = entry ? entry.name : '';
        if (action === 'edit') openApiKeyModal(entry);
        else if (action === 'delete') deleteApiKeyEntry(id, name);
        else if (action === 'reset') resetApiKeyUsageEntry(id, name);
        else if (action === 'topup') openTopupModal(id);
        else if (action === 'detail') openKeyDetail(id);
        else if (action === 'copyKey') copyKeyValue(entry, btn);
        else if (action === 'copyUrl') copyKeyBaseUrl(btn);
      });
      list.addEventListener('change', e => {
        const cb = e.target.closest('input[data-key-action="toggle"]');
        if (!cb || !cb.dataset.id) return;
        toggleApiKeyEntry(cb.dataset.id, cb.checked);
      });
    }
    // Keys tab controls
    const addBtn = $('keysAddBtn');
    if (addBtn) addBtn.addEventListener('click', () => openApiKeyModal(null));
    const search = $('keysSearch');
    if (search) search.addEventListener('input', e => { keysFilterKeyword = e.target.value; renderApiKeys(); });
    const filterSel = $('keysFilterSelect');
    if (filterSel) filterSel.addEventListener('change', e => { keysFilterStatus = e.target.value; renderApiKeys(); });
    const sortSel = $('keysSortSelect');
    if (sortSel) sortSel.addEventListener('change', e => { keysSortBy = e.target.value; renderApiKeys(); });
    const gotoBtn = $('gotoKeysBtn');
    if (gotoBtn) gotoBtn.addEventListener('click', () => switchTab('keys'));

    // Create/edit modal
    const saveBtn = $('apiKeyModalSaveBtn');
    if (saveBtn) saveBtn.addEventListener('click', submitApiKeyModal);
    const mcModeSel = $('apiKeyForm_maxConcurrencyMode');
    if (mcModeSel) mcModeSel.addEventListener('change', updateMaxConcurrencyField);
    const cancelBtn = $('apiKeyModalCancelBtn');
    if (cancelBtn) cancelBtn.addEventListener('click', closeApiKeyModal);
    const closeBtn = $('apiKeyModalClose');
    if (closeBtn) closeBtn.addEventListener('click', closeApiKeyModal);

    // Top-up modal
    const topupClose = $('topupModalClose');
    if (topupClose) topupClose.addEventListener('click', closeTopupModal);
    const topupCancel = $('topupCancelBtn');
    if (topupCancel) topupCancel.addEventListener('click', closeTopupModal);
    const topupSubmit = $('topupSubmitBtn');
    if (topupSubmit) topupSubmit.addEventListener('click', submitTopup);

    // Card detail modal
    const kdClose = $('keyDetailModalClose');
    if (kdClose) kdClose.addEventListener('click', () => closeDialog('keyDetailModal'));
    const subtabs = $('keyDetailSubtabs');
    if (subtabs) subtabs.addEventListener('click', e => {
      const b = e.target.closest('[data-subtab]');
      if (b) switchKeyDetailTab(b.dataset.subtab);
    });
    const kdContent = $('keyDetailContent');
    if (kdContent) kdContent.addEventListener('click', onKeyDetailContentClick);

    bindDialogBackdropClose('apiKeyModal', closeApiKeyModal);
    bindDialogBackdropClose('topupModal', closeTopupModal);
    bindDialogBackdropClose('keyDetailModal', () => closeDialog('keyDetailModal'));
  }

  // Prompt filter rules
  async function loadPromptFilter() {
    const res = await api('/prompt-filter');
    const d = await res.json();
    $('filterClaudeCode').checked = !!d.filterClaudeCode;
    $('filterEnvNoise').checked = !!d.filterEnvNoise;
    $('filterStripBoundaries').checked = !!d.filterStripBoundaries;
    promptRules = d.rules || [];
    renderPromptRules();
  }
  async function savePromptFilter() {
    const res = await api('/prompt-filter', {
      method: 'POST', body: JSON.stringify({
        filterClaudeCode: $('filterClaudeCode').checked,
        filterEnvNoise: $('filterEnvNoise').checked,
        filterStripBoundaries: $('filterStripBoundaries').checked,
        rules: promptRules
      })
    });
    const d = await res.json();
    if (d.success) toast(t('settings.promptFilterSaved'), 'success');
    else toast(t('common.saveFailed') + ': ' + (d.error || ''), 'error');
  }
  function renderPromptRules() {
    const c = $('promptFilterRules');
    if (!c) return;
    if (!promptRules.length) {
      c.innerHTML = '<small class="text-xs muted-text">' + escapeHtml(t('promptFilter.noRules')) + '</small>';
      return;
    }
    c.innerHTML = promptRules.map((r, i) => {
      const isContains = r.type === 'lines-containing';
      const typeLabel = isContains ? t('promptFilter.typeContains') : t('promptFilter.typeRegex');
      const matchPh = isContains ? t('promptFilter.matchPlaceholderContains') : t('promptFilter.matchPlaceholderRegex');
      const replaceRow = !isContains
        ? '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.replace')) + '</label>' +
        '<input value="' + escapeAttr(r.replace || '') + '" data-rule-idx="' + i + '" data-rule-field="replace" placeholder="' + escapeAttr(t('promptFilter.emptyRemove')) + '" />' +
        '</div>'
        : '';
      return '<div class="rule-card' + (r.enabled ? '' : ' disabled') + '">' +
        '<div class="rule-header">' +
        '<label class="switch"><input type="checkbox" ' + (r.enabled ? 'checked' : '') + ' data-rule-toggle="' + i + '" /><span class="slider"></span></label>' +
        '<div class="rule-meta">' +
        '<input class="rule-name-input" value="' + escapeAttr(r.name || '') + '" data-rule-idx="' + i + '" data-rule-field="name" placeholder="' + escapeAttr(t('promptFilter.unnamed')) + '" />' +
        '<span class="rule-type">' + escapeHtml(typeLabel) + '</span>' +
        '</div>' +
        '<button class="rule-remove" data-rule-remove="' + i + '" type="button" aria-label="' + escapeAttr(t('common.remove')) + '">&times;</button>' +
        '</div>' +
        '<div class="rule-body">' +
        '<div class="rule-field"><label>' + escapeHtml(t('promptFilter.match')) + '</label>' +
        '<input value="' + escapeAttr(r.match || '') + '" data-rule-idx="' + i + '" data-rule-field="match" placeholder="' + escapeAttr(matchPh) + '" />' +
        '</div>' +
        replaceRow +
        '</div>' +
        '</div>';
    }).join('');
  }
  function addPromptRule(type) {
    promptRules.push({ id: 'rule-' + Date.now(), name: '', type, match: '', replace: '', enabled: true });
    renderPromptRules();
  }

  // Single "add account" modal: a method tab-bar switches the form below.
  // Social login is intentionally excluded (known-broken upstream).
  const ADD_METHODS = [
    ['credentials', 'modal.credentialsTitle'],
    ['apikey', 'modal.apiKeyTitle'],
    ['sso', 'modal.iamTitle']
  ];
  function addMethodTabs(active) {
    return '<div class="add-method-tabs">' + ADD_METHODS.map(m =>
      '<button type="button" class="add-method-tab' + (m[0] === active ? ' active' : '') + '" data-modal-goto="' + m[0] + '">' +
      escapeHtml(t(m[1])) + '</button>'
    ).join('') + '</div>';
  }
  function showModal(type) {
    const modal = $('addModal');
    const title = $('modalTitle');
    const body = $('modalBody');
    if (type === 'apikey') modalApiKeyImport(title, body);
    else if (type === 'sso') modalSso(title, body);
    else modalCredentials(title, body); // 'add' / 'credentials' / default
    if (!modal.classList.contains('active')) openDialog('addModal');
    enhanceCustomSelects(body);
  }
  function closeModal() {
    closeDialog('addModal');
  }
  function modalSso(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      addMethodTabs('sso') +
      '<p class="help-block">' + escapeHtml(t('modal.iamDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('sso.nameLabel')) + '</label>' +
      '<input type="text" id="ssoName" placeholder="' + escapeAttr(t('sso.namePlaceholder')) + '" /></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.startUrl')) + ' <small>' + escapeHtml(t('sso.startUrlHint')) + '</small></label>' +
      '<input type="text" id="ssoStartUrl" class="font-mono" placeholder="https://d-xxxxxxxxxx.awsapps.com/start" /></div>' +
      '<div id="ssoStep2" class="hidden">' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.loginUrl')) + '</label>' +
      '<div class="input-row"><input type="text" id="ssoAuthUrl" class="font-mono" readonly />' +
      '<button class="btn btn-outline" id="ssoOpenBtn" type="button">' + escapeHtml(t('builderid.open')) + '</button></div></div>' +
      '<p class="help-block">' + escapeHtml(t('iam.completeLogin')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('iam.callbackUrl')) + '</label>' +
      '<textarea id="ssoCallback" class="font-mono" placeholder="http://127.0.0.1/oauth/callback?code=..."></textarea></div>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-primary" id="ssoStartBtn" type="button">' + escapeHtml(t('builderid.startLogin')) + '</button>' +
      '<button class="btn btn-primary hidden" id="ssoCompleteBtn" type="button">' + escapeHtml(t('iam.complete')) + '</button>' +
      '</div>';
    const ssoState = { sessionId: null, authUrl: null };
    $('ssoStartBtn').addEventListener('click', () => startIamSso(ssoState));
    $('ssoCompleteBtn').addEventListener('click', () => completeIamSso(ssoState));
    $('ssoOpenBtn').addEventListener('click', () => { if (ssoState.authUrl) window.open(ssoState.authUrl, '_blank', 'noopener'); });
  }

  function modalCredentials(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      addMethodTabs('credentials') +
      '<p class="help-block">' + escapeHtml(t('credentials.jsonHint')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('credentials.label')) + ' <small>' + escapeHtml(t('credentials.dropHint')) + '</small></label>' +
      '<textarea id="credJson" class="font-mono" placeholder=\'{"refreshToken":"...","clientId":"...","clientSecret":"...","region":"us-east-1"}\'></textarea>' +
      '</div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-primary" id="importCredBtn" type="button">' + escapeHtml(t('common.add')) + '</button>' +
      '</div>';
    $('importCredBtn').addEventListener('click', importCredentials);
    enableJsonFileDrop($('credJson'));
  }
  function modalApiKeyImport(title, body) {
    title.textContent = t('modal.addAccount');
    body.innerHTML =
      addMethodTabs('apikey') +
      '<p class="help-block">' + escapeHtml(t('modal.apiKeyDesc')) + '</p>' +
      '<div class="form-group"><label>' + escapeHtml(t('apiKeyImport.keys')) + ' <small>' + escapeHtml(t('apiKeyImport.keysHint')) + '</small></label>' +
      '<textarea id="apiKeyImportKeys" class="font-mono" placeholder="ksk_xxxxxxxx&#10;ksk_yyyyyyyy"></textarea></div>' +
      '<div class="form-group"><label>' + escapeHtml(t('apiKeyImport.region')) + '</label><input type="text" id="apiKeyImportRegion" value="us-east-1" /></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" data-close-add="1" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-primary" id="importApiKeyBtn" type="button">' + escapeHtml(t('apiKeyImport.submit')) + '</button>' +
      '</div>';
    $('importApiKeyBtn').addEventListener('click', importKiroApiKey);
  }
  // Let users drag a .json credentials file (e.g. Kiro Account Manager export)
  // straight onto a textarea instead of copy-pasting. The file text is read into
  // the field; importCredentials already parses both kam ({accounts:[...]}) and
  // line/array formats, so no extra format handling is needed here.
  function enableJsonFileDrop(el) {
    if (!el) return;
    const stop = (e) => { e.preventDefault(); e.stopPropagation(); };
    ['dragenter', 'dragover'].forEach(ev => el.addEventListener(ev, (e) => {
      stop(e);
      el.classList.add('is-dragover');
    }));
    ['dragleave', 'dragend'].forEach(ev => el.addEventListener(ev, (e) => {
      stop(e);
      el.classList.remove('is-dragover');
    }));
    el.addEventListener('drop', (e) => {
      stop(e);
      el.classList.remove('is-dragover');
      const file = e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0];
      if (!file) return;
      const r = new FileReader();
      r.onload = (ev) => { el.value = ev.target.result; el.focus(); };
      r.readAsText(file);
    });
  }
  async function importCredentials() {
    const raw = $('credJson').value.trim();
    if (!raw) { toastWarning(t('credentials.jsonError')); return; }
    let items;
    try {
      const json = JSON.parse(raw);
      if (json.accounts && Array.isArray(json.accounts)) {
        items = json.accounts.map(a => {
          const c = a.credentials || {};
          return {
            refreshToken: c.refreshToken || a.refreshToken,
            clientId: c.clientId || a.clientId,
            clientSecret: c.clientSecret || a.clientSecret,
            region: c.region || a.region,
            authMethod: c.authMethod || a.authMethod,
            provider: c.provider || a.provider || a.idp
          };
        });
      } else {
        items = Array.isArray(json) ? json : [json];
      }
    } catch {
      toastWarning(t('credentials.jsonError'));
      return;
    }
    let ok = 0, fail = 0;
    for (const item of items) {
      if (!item.refreshToken) { fail++; continue; }
      let authMethod = item.authMethod || '';
      if (item.clientId && item.clientSecret) authMethod = 'idc';
      else if (!authMethod || authMethod === 'social') authMethod = 'social';
      else authMethod = authMethod.toLowerCase() === 'idc' ? 'idc' : 'social';
      let provider = item.provider || '';
      if (!provider && authMethod === 'social') provider = 'Google';
      if (!provider && authMethod === 'idc') provider = 'BuilderId';
      const payload = {
        refreshToken: item.refreshToken,
        accessToken: item.accessToken || '',
        clientId: item.clientId || '',
        clientSecret: item.clientSecret || '',
        authMethod, provider,
        region: item.region || 'us-east-1'
      };
      try {
        const res = await api('/auth/credentials', { method: 'POST', body: JSON.stringify(payload) });
        const d = await res.json();
        if (d.success) { ok++; }
        else fail++;
      } catch { fail++; }
    }
    closeModal(); loadAccounts(); loadStats();
    let msg = t('sso.importSuccess', ok);
    if (fail > 0) msg += t('sso.importPartial', fail);
    toastPrimary(msg, { duration: 5200 });
  }
  async function importKiroApiKey() {
    const apiKey = $('apiKeyImportKeys').value.trim();
    if (!apiKey) return toastWarning(t('apiKeyImport.keysMissing'));
    const region = $('apiKeyImportRegion').value.trim() || 'us-east-1';
    const btn = $('importApiKeyBtn');
    if (btn) { btn.disabled = true; btn.setAttribute('aria-busy', 'true'); }
    try {
      const res = await api('/auth/api-key', { method: 'POST', body: JSON.stringify({ apiKey, region }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || d.success === false) throw new Error(d.error || t('common.failed'));
      closeModal(); loadAccounts(); loadStats();
      const okCount = (d.accounts && d.accounts.length) || 0;
      const failCount = (d.errors && d.errors.length) || 0;
      let msg = t('apiKeyImport.success', okCount);
      if (failCount > 0) msg += t('apiKeyImport.partial', failCount);
      toastPrimary(msg, { duration: 5200 });
    } catch (e) {
      if (btn) { btn.disabled = false; btn.removeAttribute('aria-busy'); }
      toastError((e && e.message) || t('common.failed'));
    }
  }
  // Manual IAM Identity Center (企业 SSO) OOB flow: fill 备注(用户名) + Start URL →
  // backend auto-detects the portal region and returns an AWS authorize URL → user
  // logs in and pastes the 127.0.0.1 callback URL → backend exchanges it for tokens
  // and hydrates quota/subscription server-side.
  async function startIamSso(state) {
    const name = $('ssoName').value.trim();
    const startUrl = $('ssoStartUrl').value.trim();
    if (!name) return toastWarning(t('sso.nameMissing'));
    if (!startUrl) return toastWarning(t('sso.startUrlMissing'));
    const btn = $('ssoStartBtn');
    if (btn) { btn.disabled = true; btn.setAttribute('aria-busy', 'true'); }
    try {
      const res = await api('/auth/iam-sso/start', { method: 'POST', body: JSON.stringify({ startUrl, name }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || !d.sessionId) throw new Error(d.error || t('common.failed'));
      state.sessionId = d.sessionId;
      state.authUrl = d.authorizeUrl || '';
      const urlField = $('ssoAuthUrl');
      if (urlField) urlField.value = state.authUrl;
      const step2 = $('ssoStep2');
      if (step2) step2.classList.remove('hidden');
      if (btn) btn.classList.add('hidden');
      const cbtn = $('ssoCompleteBtn');
      if (cbtn) cbtn.classList.remove('hidden');
      if (state.authUrl) window.open(state.authUrl, '_blank', 'noopener');
      toastPrimary(t('iam.completeLogin'), { duration: 6000 });
    } catch (e) {
      toastError((e && e.message) || t('common.failed'));
    } finally {
      if (btn) { btn.disabled = false; btn.removeAttribute('aria-busy'); }
    }
  }
  async function completeIamSso(state) {
    const callbackUrl = $('ssoCallback').value.trim();
    if (!callbackUrl) return toastWarning(t('sso.callbackMissing'));
    if (!state.sessionId) return toastWarning(t('sso.startUrlMissing'));
    const btn = $('ssoCompleteBtn');
    if (btn) { btn.disabled = true; btn.setAttribute('aria-busy', 'true'); }
    try {
      const res = await api('/auth/iam-sso/complete', { method: 'POST', body: JSON.stringify({ sessionId: state.sessionId, callbackUrl }) });
      const d = await res.json().catch(() => ({}));
      if (!res.ok || !d.success) throw new Error(d.error || t('common.failed'));
      closeModal(); loadAccounts(); loadStats();
      toastPrimary(t('sso.importSuccess', 1), { duration: 5200 });
    } catch (e) {
      if (btn) { btn.disabled = false; btn.removeAttribute('aria-busy'); }
      toastError((e && e.message) || t('common.failed'));
    }
  }

  // Export modal
  function showExportModal() {
    if (!accountsData.length) return toastWarning(t('accounts.empty'));
    exportSelectedIds = new Set(accountsData.map(a => a.id));
    renderExportModal();
    openDialog('exportModal');
  }
  function closeExportModal() { closeDialog('exportModal'); }
  function renderExportModal() {
    const body = $('exportBody');
    const all = exportSelectedIds.size === accountsData.length;
    body.innerHTML =
      '<div class="flex items-center justify-between mb-3">' +
      '<span class="text-sm muted-text">' + escapeHtml(t('export.selected', exportSelectedIds.size)) + '</span>' +
      '<button class="btn btn-sm btn-outline" id="exportToggleAllBtn" type="button">' + escapeHtml(all ? t('export.deselectAll') : t('export.selectAll')) + '</button>' +
      '</div>' +
      '<div class="export-list">' +
      accountsData.map(a => {
        const checked = exportSelectedIds.has(a.id);
        return '<label class="export-row' + (checked ? ' selected' : '') + '">' +
          '<input type="checkbox" ' + (checked ? 'checked' : '') + ' data-export-toggle="' + escapeAttr(a.id) + '" />' +
          '<div class="export-row-text">' +
          '<div class="export-row-email">' + escapeHtml(getDisplayEmail(a.email, a.id)) + '</div>' +
          '<div class="export-row-meta">' + escapeHtml(formatAuthMethod(a.provider || a.authMethod)) + ' · ' + escapeHtml(formatSubscriptionLabel(a.subscriptionType)) + '</div>' +
          '</div>' +
          '</label>';
      }).join('') +
      '</div>' +
      '<div id="exportJsonPreview" class="hidden mb-3"><textarea id="exportJsonText" readonly class="font-mono"></textarea></div>' +
      '<div class="modal-footer">' +
      '<button class="btn btn-secondary" id="exportCloseBtn" type="button">' + escapeHtml(t('common.cancel')) + '</button>' +
      '<button class="btn btn-outline" id="exportShowJsonBtn" type="button">' + escapeHtml(t('export.showJson')) + '</button>' +
      '<button class="btn btn-outline" id="exportCopyJsonBtn" type="button">' + escapeHtml(t('export.copyJson')) + '</button>' +
      '<button class="btn btn-primary" id="exportDownloadBtn" type="button">' + escapeHtml(t('export.downloadJson')) + '</button>' +
      '</div>';
    $('exportToggleAllBtn').addEventListener('click', () => {
      if (exportSelectedIds.size === accountsData.length) exportSelectedIds.clear();
      else exportSelectedIds = new Set(accountsData.map(a => a.id));
      renderExportModal();
    });
    $('exportCloseBtn').addEventListener('click', closeExportModal);
    $('exportShowJsonBtn').addEventListener('click', exportShowJson);
    $('exportCopyJsonBtn').addEventListener('click', exportCopyJson);
    $('exportDownloadBtn').addEventListener('click', exportDownloadJson);
    qsa('[data-export-toggle]', body).forEach(cb => cb.addEventListener('change', e => {
      const id = e.target.dataset.exportToggle;
      if (exportSelectedIds.has(id)) exportSelectedIds.delete(id);
      else exportSelectedIds.add(id);
      renderExportModal();
    }));
  }
  async function getExportData() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return null; }
    const res = await api('/export', { method: 'POST', body: JSON.stringify({ ids: Array.from(exportSelectedIds) }) });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      toastError(t('common.failed') + ': ' + (err.error || t('common.unknownError')));
      return null;
    }
    return res.json();
  }
  async function exportShowJson() {
    const data = await getExportData();
    if (!data) return;
    $('exportJsonPreview').classList.remove('hidden');
    $('exportJsonText').value = JSON.stringify(data, null, 2);
  }
  async function exportCopyJson() {
    if (exportSelectedIds.size === 0) { toastWarning(t('export.noSelection')); return; }
    // Copy the complete KAM structure (same as Download / Show), not a stripped
    // 4-field subset — so region / authMethod / startUrl are preserved for IdC
    // and enterprise accounts.
    const jsonPromise = getExportData().then(data => {
      if (!data) throw new Error('no-data');
      return JSON.stringify(data, null, 2);
    });
    try {
      await copyText(jsonPromise);
      toast(t('export.copied'), 'primary');
    } catch (e) {
      if (e && e.message !== 'no-data') toastError(t('common.failed'));
    }
  }
  async function exportDownloadJson() {
    const data = await getExportData();
    if (!data) return;
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'kiro-accounts-' + new Date().toISOString().slice(0, 10) + '.json';
    a.click();
    URL.revokeObjectURL(url);
  }

  // Version and update
  function renderVersionBadge() {
    const badge = $('versionBadge');
    if (badge && currentVersion) badge.textContent = currentVersion.replace(/^v/i, '');
  }
  async function loadVersion() {
    try {
      const res = await api('/version');
      const d = await res.json();
      currentVersion = d.version || '';
      renderVersionBadge();
    } catch (e) { }
  }
  // Tabs
  const KNOWN_TABS = ['overview', 'accounts', 'keys', 'settings', 'logs'];
  function switchTab(tab) {
    if (KNOWN_TABS.indexOf(tab) === -1) tab = 'overview';
    qsa('.tab').forEach(el => el.classList.toggle('active', el.dataset.tab === tab));
    qsa('.tab-content').forEach(c => { c.classList.add('hidden'); c.classList.remove('ng-fade-in'); });
    const panel = $('tab' + tab.charAt(0).toUpperCase() + tab.slice(1));
    if (panel) {
      panel.classList.remove('hidden');
      void panel.offsetWidth; // reflow so the fade-in animation retriggers
      panel.classList.add('ng-fade-in');
    }
    try { localStorage.setItem('kiro_tab', tab); } catch (e) { }
    currentTab = tab;
    // Load the freshly-shown tab immediately; the 1s auto-refresh loop keeps it
    // live afterwards (overview also samples the RPM/token trend each tick).
    if (tab === 'overview') { ovAnimated = false; loadOverview(); }
    if (tab === 'accounts') loadAccounts();
    if (tab === 'logs') loadLogs();
    if (tab === 'keys') { loadApiKeys(); loadConcurrency(); }
  }

  // Event wiring
  function bindLoginEvents() {
    $('loginBtn').addEventListener('click', login);
    $('pwdField').addEventListener('keypress', e => { if (e.key === 'Enter') login(); });

    const pwdToggle = $('pwdToggle');
    if (pwdToggle) {
      pwdToggle.addEventListener('click', () => {
        const f = $('pwdField');
        const willShow = f.type === 'password';
        f.type = willShow ? 'text' : 'password';
        pwdToggle.dataset.shown = String(willShow);
        pwdToggle.setAttribute('aria-label', willShow ? t('login.hidePassword') : t('login.showPassword'));
        pwdToggle.innerHTML = willShow
          ? '<i class="fa-solid fa-eye-slash"></i>'
          : '<i class="fa-solid fa-eye"></i>';
      });
    }
  }

  function bindShellEvents() {
    document.body.addEventListener('click', e => {
      if (!e.target.closest('.custom-select')) closeAllCustomSelects();
      const lb = e.target.closest('.lang-btn');
      if (lb) setLang(lb.dataset.lang);
      const lt = e.target.closest('.lang-toggle');
      if (lt) toggleLang();
    });
    window.addEventListener('resize', positionOpenCustomSelects);
    window.addEventListener('scroll', positionOpenCustomSelects, true);

    $('loginThemeToggle').addEventListener('click', toggleTheme);
    $('mainThemeToggle').addEventListener('click', toggleTheme);
    $('logoutBtn').addEventListener('click', logout);

    qsa('#tabBar .tab').forEach(tab => tab.addEventListener('click', () => switchTab(tab.dataset.tab)));

    qsa('[data-copy]').forEach(btn => btn.addEventListener('click', async () => {
      const id = btn.dataset.copy;
      const target = $(id);
      if (!target) return;
      try {
        await copyText(target.dataset.rawValue || target.textContent);
        toast(t('common.copied'), 'primary');
      } catch (e) {
        toast(t('common.failed'), 'error');
      }
    }));

    // Auto-refresh: run one immediate tick when the page regains focus so
    // coming back feels instant (the 1s loop handles the steady state). The
    // slow-tab (accounts/keys) 10s gate is reset so that tick fetches at once.
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) { resetSlowTabGate(); autoRefreshTick(); }
    });

    // API View modal (guarded: its triggers were removed with the API tab)
    const vmb = $('viewModelsBtn'); if (vmb) vmb.addEventListener('click', showModelsView);
    const vsb = $('viewStatsBtn'); if (vsb) vsb.addEventListener('click', showStatsView);
    const avc = $('apiViewModalClose'); if (avc) avc.addEventListener('click', closeApiViewModal);
    if ($('apiViewModal')) bindDialogBackdropClose('apiViewModal', closeApiViewModal);

    // Logs tab (manual refresh + auto-refresh checkbox removed; always live)
    const logsClearBtn = $('logsClearBtn');
    if (logsClearBtn) logsClearBtn.addEventListener('click', clearLogs);
    const logsFilterSel = $('logsFilterSelect');
    if (logsFilterSel) logsFilterSel.addEventListener('change', e => {
      logsFilter = e.target.value;
      loadLogs();
    });
  }

  function bindAccountEvents() {
    $('exportBtn').addEventListener('click', showExportModal);
    $('addAccountBtn').addEventListener('click', () => showModal('add'));

    $('selectAllCheckbox').addEventListener('change', e => toggleSelectAll(e.target.checked));
    qsa('[data-batch]').forEach(b => b.addEventListener('click', () => {
      const a = b.dataset.batch;
      if (a === 'delete') batchDelete();
      else batchAction(a);
    }));

    $('filterSearch').addEventListener('input', onFilterChange);
    $('filterStatusSelect').addEventListener('change', onFilterChange);
    const accountsSortSel = $('accountsSortSelect');
    if (accountsSortSel) accountsSortSel.addEventListener('change', e => { accountsSortBy = e.target.value; renderAccounts(); });

    $('accountsList').addEventListener('click', e => {
      const cb = e.target.closest('.account-checkbox');
      if (cb) {
        toggleSelectAccount(cb.dataset.id);
        const card = cb.closest('.account-card');
        if (card) card.classList.toggle('selected', cb.checked);
        return;
      }
      const btn = e.target.closest('button[data-action]');
      if (!btn) return;
      const id = btn.dataset.id;
      const action = btn.dataset.action;
      if (action === 'detail') showDetail(id);
      else if (action === 'copyJSON') copyAccountJSON(id, btn);
      else if (action === 'toggle') toggleAccount(id, btn.dataset.enabled === 'true');
      else if (action === 'test') testAccount(id);
      else if (action === 'delete') deleteAccount(id);
    });
  }

  function bindSettingsEvents() {
    $('saveEndpointBtn').addEventListener('click', saveEndpointConfig);
    $('changePasswordBtn').addEventListener('click', changePassword);
    $('proxyType').addEventListener('change', onProxyTypeChange);
    $('saveProxyBtn').addEventListener('click', saveProxyConfig);
    bindApiKeyEvents();
  }

  function bindPromptFilterEvents() {
    $('savePromptFilterBtn').addEventListener('click', savePromptFilter);
    $('addRuleRegexBtn').addEventListener('click', () => addPromptRule('regex'));
    $('addRuleContainsBtn').addEventListener('click', () => addPromptRule('lines-containing'));

    $('promptFilterRules').addEventListener('input', e => {
      const idx = e.target.dataset.ruleIdx;
      const field = e.target.dataset.ruleField;
      if (idx != null && field) promptRules[idx][field] = e.target.value;
    });
    $('promptFilterRules').addEventListener('change', e => {
      if (e.target.dataset.ruleToggle != null) {
        promptRules[e.target.dataset.ruleToggle].enabled = e.target.checked;
        renderPromptRules();
      }
    });
    $('promptFilterRules').addEventListener('click', e => {
      const rm = e.target.closest('[data-rule-remove]');
      if (rm) { promptRules.splice(parseInt(rm.dataset.ruleRemove, 10), 1); renderPromptRules(); }
    });
  }

  function bindModalEvents() {
    $('addModalClose').addEventListener('click', closeModal);
    $('detailModalClose').addEventListener('click', closeDetailModal);
    $('exportModalClose').addEventListener('click', closeExportModal);
    $('testModalClose').addEventListener('click', closeTestModal);
    [
      ['addModal', closeModal],
      ['detailModal', closeDetailModal],
      ['exportModal', closeExportModal],
      ['testModal', closeTestModal],
      ['confirmModal', () => closeConfirm(false)],
    ].forEach(([id, fn]) => bindDialogBackdropClose(id, fn));

    $('modalBody').addEventListener('click', e => {
      const g = e.target.closest('[data-modal-goto]');
      if (g) { showModal(g.dataset.modalGoto); return; }
      if (e.target.dataset.closeAdd) closeModal();
    });
  }

  function bindDetailEvents() {
    const onDetailClick = e => {
      if (e.target.id === 'generateMachineIdBtn') { generateMachineId(); return; }
      const b = e.target.closest('[data-detail-action]');
      if (!b) return;
      const id = b.dataset.id;
      const a = b.dataset.detailAction;
      if (a === 'saveDetail') saveAccountDetail(id);
      else if (a === 'toggleOverage') toggleOverageSwitch(id, b);
    };
    $('detailBody').addEventListener('click', onDetailClick);
    $('detailFooter').addEventListener('click', onDetailClick);
  }

  function bindTestEvents() {
    $('testBody').addEventListener('click', e => {
      if (e.target.id === 'testLogClear') { clearTestLog(); return; }
      if (e.target.id === 'testModalCancelBtn') { closeTestModal(); return; }
      const run = e.target.closest('#testRunBtn');
      if (run) runTestAccount(run.dataset.id, getTestModelValue());
    });
    $('testBody').addEventListener('keydown', e => {
      if (e.key !== 'Enter') return;
      if (!e.target.closest('#testModelChoice')) return;
      const run = $('testRunBtn');
      if (!run || run.disabled) return;
      e.preventDefault();
      runTestAccount(run.dataset.id, getTestModelValue());
    });
  }

  // ── API View Modal ──
  function closeApiViewModal() {
    closeDialog('apiViewModal');
  }

  function formatUptime(seconds) {
    const d = Math.floor(seconds / 86400);
    const h = Math.floor((seconds % 86400) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = seconds % 60;
    const parts = [];
    if (d > 0) parts.push(d + (currentLang === 'zh' ? '天' : 'd'));
    if (h > 0) parts.push(h + (currentLang === 'zh' ? '时' : 'h'));
    if (m > 0) parts.push(m + (currentLang === 'zh' ? '分' : 'm'));
    parts.push(s + (currentLang === 'zh' ? '秒' : 's'));
    return parts.join(' ');
  }

  async function showModelsView() {
    const title = $('apiViewTitle');
    const body = $('apiViewBody');
    title.textContent = t('api.viewModelsTitle');
    body.innerHTML = '<div class="api-view-loading"><i class="fa-solid fa-spinner fa-spin"></i> ' + escapeHtml(t('api.loading')) + '</div>';
    openDialog('apiViewModal');

    try {
      const res = await fetch(baseUrl + '/v1/models');
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const data = await res.json();
      const models = data.data || [];
      renderModelsView(body, models);
    } catch (e) {
      body.innerHTML = '<div class="api-view-error"><i class="fa-solid fa-circle-exclamation"></i> ' + escapeHtml(t('api.fetchError') + ': ' + e.message) + '</div>';
    }
  }

  function renderModelsView(container, models) {
    const thinkingSuffix = '-thinking';
    let html = '<div class="api-view-toolbar">';
    html += '<span class="api-view-count">' + escapeHtml(t('api.totalModels').replace('{count}', models.length)) + '</span>';
    html += '<input type="text" class="api-view-search" id="modelsSearchInput" placeholder="' + escapeAttr(t('api.searchModels')) + '" />';
    html += '</div>';
    html += '<div id="modelsGridContainer">';
    html += buildModelsGroupedHtml(models, thinkingSuffix);
    html += '</div>';
    container.innerHTML = html;

    const searchInput = $('modelsSearchInput');
    if (searchInput) {
      searchInput.addEventListener('input', () => {
        const kw = searchInput.value.toLowerCase().trim();
        const filtered = kw ? models.filter(m => (m.id || '').toLowerCase().includes(kw) || (m.owned_by || '').toLowerCase().includes(kw)) : models;
        $('modelsGridContainer').innerHTML = buildModelsGroupedHtml(filtered, thinkingSuffix);
      });
    }
  }

  // SVG icons for model providers (inline style forces size over Tailwind preflight)
  const _svgStyle = 'style="width:1.375rem;height:1.375rem;max-width:1.375rem;max-height:1.375rem;flex:none;display:block"';
  const MODEL_SVGS = {
    claude: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M4.709 15.955l4.72-2.647.08-.23-.08-.128H9.2l-.79-.048-2.698-.073-2.339-.097-2.266-.122-.571-.121L0 11.784l.055-.352.48-.321.686.06 1.52.103 2.278.158 1.652.097 2.449.255h.389l.055-.157-.134-.098-.103-.097-2.358-1.596-2.552-1.688-1.336-.972-.724-.491-.364-.462-.158-1.008.656-.722.881.06.225.061.893.686 1.908 1.476 2.491 1.833.365.304.145-.103.019-.073-.164-.274-1.355-2.446-1.446-2.49-.644-1.032-.17-.619a2.97 2.97 0 01-.104-.729L6.283.134 6.696 0l.996.134.42.364.62 1.414 1.002 2.229 1.555 3.03.456.898.243.832.091.255h.158V9.01l.128-1.706.237-2.095.23-2.695.08-.76.376-.91.747-.492.584.28.48.685-.067.444-.286 1.851-.559 2.903-.364 1.942h.212l.243-.242.985-1.306 1.652-2.064.73-.82.85-.904.547-.431h1.033l.76 1.129-.34 1.166-1.064 1.347-.881 1.142-1.264 1.7-.79 1.36.073.11.188-.02 2.856-.606 1.543-.28 1.841-.315.833.388.091.395-.328.807-1.969.486-2.309.462-3.439.813-.042.03.049.061 1.549.146.662.036h1.622l3.02.225.79.522.474.638-.079.485-1.215.62-1.64-.389-3.829-.91-1.312-.329h-.182v.11l1.093 1.068 2.006 1.81 2.509 2.33.127.578-.322.455-.34-.049-2.205-1.657-.851-.747-1.926-1.62h-.128v.17l.444.649 2.345 3.521.122 1.08-.17.353-.608.213-.668-.122-1.374-1.925-1.415-2.167-1.143-1.943-.14.08-.674 7.254-.316.37-.729.28-.607-.461-.322-.747.322-1.476.389-1.924.315-1.53.286-1.9.17-.632-.012-.042-.14.018-1.434 1.967-2.18 2.945-1.726 1.845-.414.164-.717-.37.067-.662.401-.589 2.388-3.036 1.44-1.882.93-1.086-.006-.158h-.055L4.132 18.56l-1.13.146-.487-.456.061-.746.231-.243 1.908-1.312-.006.006z"/></svg>',
    openai: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M9.205 8.658v-2.26c0-.19.072-.333.238-.428l4.543-2.616c.619-.357 1.356-.523 2.117-.523 2.854 0 4.662 2.212 4.662 4.566 0 .167 0 .357-.024.547l-4.71-2.759a.797.797 0 00-.856 0l-5.97 3.473zm10.609 8.8V12.06c0-.333-.143-.57-.429-.737l-5.97-3.473 1.95-1.118a.433.433 0 01.476 0l4.543 2.617c1.309.76 2.189 2.378 2.189 3.948 0 1.808-1.07 3.473-2.76 4.163zM7.802 12.703l-1.95-1.142c-.167-.095-.239-.238-.239-.428V5.899c0-2.545 1.95-4.472 4.591-4.472 1 0 1.927.333 2.712.928L8.23 5.067c-.285.166-.428.404-.428.737v6.898zM12 15.128l-2.795-1.57v-3.33L12 8.658l2.795 1.57v3.33L12 15.128zm1.796 7.23c-1 0-1.927-.332-2.712-.927l4.686-2.712c.285-.166.428-.404.428-.737v-6.898l1.974 1.142c.167.095.238.238.238.428v5.233c0 2.545-1.974 4.472-4.614 4.472zm-5.637-5.303l-4.544-2.617c-1.308-.761-2.188-2.378-2.188-3.948A4.482 4.482 0 014.21 6.327v5.423c0 .333.143.571.428.738l5.947 3.449-1.95 1.118a.432.432 0 01-.476 0zm-.262 3.9c-2.688 0-4.662-2.021-4.662-4.519 0-.19.024-.38.047-.57l4.686 2.71c.286.167.571.167.856 0l5.97-3.448v2.26c0 .19-.07.333-.237.428l-4.543 2.616c-.619.357-1.356.523-2.117.523zm5.899 2.83a5.947 5.947 0 005.827-4.756C22.287 18.339 24 15.84 24 13.296c0-1.665-.713-3.282-1.998-4.448.119-.5.19-.999.19-1.498 0-3.401-2.759-5.947-5.946-5.947-.642 0-1.26.095-1.88.31A5.962 5.962 0 0010.205 0a5.947 5.947 0 00-5.827 4.757C1.713 5.447 0 7.945 0 10.49c0 1.666.713 3.283 1.998 4.448-.119.5-.19 1-.19 1.499 0 3.401 2.759 5.946 5.946 5.946.642 0 1.26-.095 1.88-.309a5.96 5.96 0 004.162 1.713z"/></svg>',
    deepseek: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M23.748 4.482c-.254-.124-.364.113-.512.234-.051.039-.094.09-.137.136-.372.397-.806.657-1.373.626-.829-.046-1.537.214-2.163.848-.133-.782-.575-1.248-1.247-1.548-.352-.156-.708-.311-.955-.65-.172-.241-.219-.51-.305-.774-.055-.16-.11-.323-.293-.35-.2-.031-.278.136-.356.276-.313.572-.434 1.202-.422 1.84.027 1.436.633 2.58 1.838 3.393.137.093.172.187.129.323-.082.28-.18.552-.266.833-.055.179-.137.217-.329.14a5.526 5.526 0 01-1.736-1.18c-.857-.828-1.631-1.742-2.597-2.458a11.365 11.365 0 00-.689-.471c-.985-.957.13-1.743.388-1.836.27-.098.093-.432-.779-.428-.872.004-1.67.295-2.687.684a3.055 3.055 0 01-.465.137 9.597 9.597 0 00-2.883-.102c-1.885.21-3.39 1.102-4.497 2.623C.082 8.606-.231 10.684.152 12.85c.403 2.284 1.569 4.175 3.36 5.653 1.858 1.533 3.997 2.284 6.438 2.14 1.482-.085 3.133-.284 4.994-1.86.47.234.962.327 1.78.397.63.059 1.236-.03 1.705-.128.735-.156.684-.837.419-.961-2.155-1.004-1.682-.595-2.113-.926 1.096-1.296 2.746-2.642 3.392-7.003.05-.347.007-.565 0-.845-.004-.17.035-.237.23-.256a4.173 4.173 0 001.545-.475c1.396-.763 1.96-2.015 2.093-3.517.02-.23-.004-.467-.247-.588zM11.581 18c-2.089-1.642-3.102-2.183-3.52-2.16-.392.024-.321.471-.235.763.09.288.207.486.371.739.114.167.192.416-.113.603-.673.416-1.842-.14-1.897-.167-1.361-.802-2.5-1.86-3.301-3.307-.774-1.393-1.224-2.887-1.298-4.482-.02-.386.093-.522.477-.592a4.696 4.696 0 011.529-.039c2.132.312 3.946 1.265 5.468 2.774.868.86 1.525 1.887 2.202 2.891.72 1.066 1.494 2.082 2.48 2.914.348.292.625.514.891.677-.802.09-2.14.11-3.054-.614zm1-6.44a.306.306 0 01.415-.287.302.302 0 01.2.288.306.306 0 01-.31.307.303.303 0 01-.304-.308zm3.11 1.596c-.2.081-.399.151-.59.16a1.245 1.245 0 01-.798-.254c-.274-.23-.47-.358-.552-.758a1.73 1.73 0 01.016-.588c.07-.327-.008-.537-.239-.727-.187-.156-.426-.199-.688-.199a.559.559 0 01-.254-.078c-.11-.054-.2-.19-.114-.358.028-.054.16-.186.192-.21.356-.202.767-.136 1.146.016.352.144.618.408 1.001.782.391.451.462.576.685.914.176.265.336.537.445.848.067.195-.019.354-.25.452z"/></svg>',
    qwen: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M12.604 1.34c.393.69.784 1.382 1.174 2.075a.18.18 0 00.157.091h5.552c.174 0 .322.11.446.327l1.454 2.57c.19.337.24.478.024.837-.26.43-.513.864-.76 1.3l-.367.658c-.106.196-.223.28-.04.512l2.652 4.637c.172.301.111.494-.043.77-.437.785-.882 1.564-1.335 2.34-.159.272-.352.375-.68.37-.777-.016-1.552-.01-2.327.016a.099.099 0 00-.081.05 575.097 575.097 0 01-2.705 4.74c-.169.293-.38.363-.725.364-.997.003-2.002.004-3.017.002a.537.537 0 01-.465-.271l-1.335-2.323a.09.09 0 00-.083-.049H4.982c-.285.03-.553-.001-.805-.092l-1.603-2.77a.543.543 0 01-.002-.54l1.207-2.12a.198.198 0 000-.197 550.951 550.951 0 01-1.875-3.272l-.79-1.395c-.16-.31-.173-.496.095-.965.465-.813.927-1.625 1.387-2.436.132-.234.304-.334.584-.335a338.3 338.3 0 012.589-.001.124.124 0 00.107-.063l2.806-4.895a.488.488 0 01.422-.246c.524-.001 1.053 0 1.583-.006L11.704 1c.341-.003.724.032.9.34zm-3.432.403a.06.06 0 00-.052.03L6.254 6.788a.157.157 0 01-.135.078H3.253c-.056 0-.07.025-.041.074l5.81 10.156c.025.042.013.062-.034.063l-2.795.015a.218.218 0 00-.2.116l-1.32 2.31c-.044.078-.021.118.068.118l5.716.008c.046 0 .08.02.104.061l1.403 2.454c.046.081.092.082.139 0l5.006-8.76.783-1.382a.055.055 0 01.096 0l1.424 2.53a.122.122 0 00.107.062l2.763-.02a.04.04 0 00.035-.02.041.041 0 000-.04l-2.9-5.086a.108.108 0 010-.113l.293-.507 1.12-1.977c.024-.041.012-.062-.035-.062H9.2c-.059 0-.073-.026-.043-.077l1.434-2.505a.107.107 0 000-.114L9.225 1.774a.06.06 0 00-.053-.031zm6.29 8.02c.046 0 .058.02.034.06l-.832 1.465-2.613 4.585a.056.056 0 01-.05.029.058.058 0 01-.05-.029L8.498 9.841c-.02-.034-.01-.052.028-.054l.216-.012 6.722-.012z"/></svg>',
    mistral: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path clip-rule="evenodd" fill="currentColor" d="M3.428 3.4h3.429v3.428h3.429v3.429h-.002 3.431V6.828h3.427V3.4h3.43v13.714H24v3.429H13.714v-3.428h-3.428v-3.429h-3.43v3.428h3.43v3.429H0v-3.429h3.428V3.4zm10.286 13.715h3.428v-3.429h-3.427v3.429z"/></svg>',
    gemini: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M20.616 10.835a14.147 14.147 0 01-4.45-3.001 14.111 14.111 0 01-3.678-6.452.503.503 0 00-.975 0 14.134 14.134 0 01-3.679 6.452 14.155 14.155 0 01-4.45 3.001c-.65.28-1.318.505-2.002.678a.502.502 0 000 .975c.684.172 1.35.397 2.002.677a14.147 14.147 0 014.45 3.001 14.112 14.112 0 013.679 6.453.502.502 0 00.975 0c.172-.685.397-1.351.677-2.003a14.145 14.145 0 013.001-4.45 14.113 14.113 0 016.453-3.678.503.503 0 000-.975 13.245 13.245 0 01-2.003-.678z"/></svg>',
    meta: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M6.897 4c1.915 0 3.516.932 5.43 3.376l.282-.373c.19-.246.383-.484.58-.71l.313-.35C14.588 4.788 15.792 4 17.225 4c1.273 0 2.469.557 3.491 1.516l.218.213c1.73 1.765 2.917 4.71 3.053 8.026l.011.392.002.25c0 1.501-.28 2.759-.818 3.7l-.14.23-.108.153c-.301.42-.664.758-1.086 1.009l-.265.142-.087.04a3.493 3.493 0 01-.302.118 4.117 4.117 0 01-1.33.208c-.524 0-.996-.067-1.438-.215-.614-.204-1.163-.56-1.726-1.116l-.227-.235c-.753-.812-1.534-1.976-2.493-3.586l-1.43-2.41-.544-.895-1.766 3.13-.343.592C7.597 19.156 6.227 20 4.356 20c-1.21 0-2.205-.42-2.936-1.182l-.168-.184c-.484-.573-.837-1.311-1.043-2.189l-.067-.32a8.69 8.69 0 01-.136-1.288L0 14.468c.002-.745.06-1.49.174-2.23l.1-.573c.298-1.53.828-2.958 1.536-4.157l.209-.34c1.177-1.83 2.789-3.053 4.615-3.16L6.897 4zm-.033 2.615l-.201.01c-.83.083-1.606.673-2.252 1.577l-.138.199-.01.018c-.67 1.017-1.185 2.378-1.456 3.845l-.004.022a12.591 12.591 0 00-.207 2.254l.002.188c.004.18.017.36.04.54l.043.291c.092.503.257.908.486 1.208l.117.137c.303.323.698.492 1.17.492 1.1 0 1.796-.676 3.696-3.641l2.175-3.4.454-.701-.139-.198C9.11 7.3 8.084 6.616 6.864 6.616zm10.196-.552l-.176.007c-.635.048-1.223.359-1.82.933l-.196.198c-.439.462-.887 1.064-1.367 1.807l.266.398c.18.274.362.56.55.858l.293.475 1.396 2.335.695 1.114c.583.926 1.03 1.6 1.408 2.082l.213.262c.282.326.529.54.777.673l.102.05c.227.1.457.138.718.138.176.002.35-.023.518-.073.338-.104.61-.32.813-.637l.095-.163.077-.162c.194-.459.29-1.06.29-1.785l-.006-.449c-.08-2.871-.938-5.372-2.2-6.798l-.176-.189c-.67-.683-1.444-1.074-2.27-1.074z"/></svg>',
    zhipu: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M11.991 23.503a.24.24 0 00-.244.248.24.24 0 00.244.249.24.24 0 00.245-.249.24.24 0 00-.22-.247l-.025-.001zM9.671 5.365a1.697 1.697 0 011.099 2.132l-.071.172-.016.04-.018.054c-.07.16-.104.32-.104.498-.035.71.47 1.279 1.186 1.314h.366c1.309.053 2.338 1.173 2.286 2.523-.052 1.332-1.152 2.38-2.478 2.327h-.174c-.715.018-1.274.64-1.239 1.368 0 .124.018.23.053.337.209.373.54.658.96.8.75.23 1.517-.125 1.9-.782l.018-.035c.402-.64 1.17-.96 1.92-.711.854.284 1.378 1.226 1.099 2.167a1.661 1.661 0 01-2.077 1.102 1.711 1.711 0 01-.907-.711l-.017-.035c-.2-.323-.463-.58-.851-.711l-.056-.018a1.646 1.646 0 00-1.954.746 1.66 1.66 0 01-1.065.764 1.677 1.677 0 01-1.989-1.279c-.209-.906.332-1.83 1.257-2.043a1.51 1.51 0 01.296-.035h.018c.68-.071 1.151-.622 1.116-1.333a1.307 1.307 0 00-.227-.693 2.515 2.515 0 01-.366-1.403 2.39 2.39 0 01.366-1.208c.14-.195.21-.444.227-.693.018-.71-.506-1.261-1.186-1.332l-.07-.018a1.43 1.43 0 01-.299-.07l-.05-.019a1.7 1.7 0 01-1.047-2.114 1.68 1.68 0 012.094-1.101zm-5.575 10.11c.26-.264.639-.367.994-.27.355.096.633.379.728.74.095.362-.007.748-.267 1.013-.402.41-1.053.41-1.455 0a1.062 1.062 0 010-1.482zm14.845-.294c.359-.09.738.024.992.297.254.274.344.665.237 1.025-.107.36-.396.634-.756.718-.551.128-1.1-.22-1.23-.781a1.05 1.05 0 01.757-1.26zm-.064-4.39c.314.32.49.753.49 1.206 0 .452-.176.886-.49 1.206-.315.32-.74.5-1.185.5-.444 0-.87-.18-1.184-.5a1.727 1.727 0 010-2.412 1.654 1.654 0 012.369 0zm-11.243.163c.364.484.447 1.128.218 1.691a1.665 1.665 0 01-2.188.923c-.855-.36-1.26-1.358-.907-2.228a1.68 1.68 0 011.33-1.038c.593-.08 1.183.169 1.547.652zm11.545-4.221c.368 0 .708.2.892.524.184.324.184.724 0 1.048a1.026 1.026 0 01-.892.524c-.568 0-1.03-.47-1.03-1.048 0-.579.462-1.048 1.03-1.048zm-14.358 0c.368 0 .707.2.891.524.184.324.184.724 0 1.048a1.026 1.026 0 01-.891.524c-.569 0-1.03-.47-1.03-1.048 0-.579.461-1.048 1.03-1.048zm10.031-1.475c.925 0 1.675.764 1.675 1.706s-.75 1.705-1.675 1.705-1.674-.763-1.674-1.705c0-.942.75-1.706 1.674-1.706zm-2.626-.684c.362-.082.653-.356.761-.718a1.062 1.062 0 00-.238-1.028 1.017 1.017 0 00-.996-.294c-.547.14-.881.7-.752 1.257.13.558.675.907 1.225.783zm0 16.876c.359-.087.644-.36.75-.72a1.062 1.062 0 00-.237-1.019 1.018 1.018 0 00-.985-.301 1.037 1.037 0 00-.762.717c-.108.361-.017.754.239 1.028.245.263.606.377.953.305l.043-.01zM17.19 3.5a.631.631 0 00.628-.64c0-.355-.279-.64-.628-.64a.631.631 0 00-.628.64c0 .355.28.64.628.64zm-10.38 0a.631.631 0 00.628-.64c0-.355-.28-.64-.628-.64a.631.631 0 00-.628.64c0 .355.279.64.628.64zm-5.182 7.852a.631.631 0 00-.628.64c0 .354.28.639.628.639a.63.63 0 00.627-.606l.001-.034a.62.62 0 00-.628-.64zm5.182 9.13a.631.631 0 00-.628.64c0 .355.279.64.628.64a.631.631 0 00.628-.64c0-.355-.28-.64-.628-.64zm10.38.018a.631.631 0 00-.628.64c0 .355.28.64.628.64a.631.631 0 00.628-.64c0-.355-.279-.64-.628-.64zm5.182-9.148a.631.631 0 00-.628.64c0 .354.279.639.628.639a.631.631 0 00.628-.64c0-.355-.28-.64-.628-.64zm-.384-4.992a.24.24 0 00.244-.249.24.24 0 00-.244-.249.24.24 0 00-.244.249c0 .142.122.249.244.249zM11.991.497a.24.24 0 00.245-.248A.24.24 0 0011.99 0a.24.24 0 00-.244.249c0 .133.108.236.223.247l.021.001zM2.011 6.36a.24.24 0 00.245-.249.24.24 0 00-.244-.249.24.24 0 00-.244.249.24.24 0 00.244.249zm0 11.263a.24.24 0 00-.243.248.24.24 0 00.244.249.24.24 0 00.244-.249.252.252 0 00-.244-.248zm19.995-.018a.24.24 0 00-.245.248.24.24 0 00.245.25.24.24 0 00.244-.25.252.252 0 00-.244-.248z"/></svg>',
    minimax: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M16.278 2c1.156 0 2.093.927 2.093 2.07v12.501a.74.74 0 00.744.709.74.74 0 00.743-.709V9.099a2.06 2.06 0 012.071-2.049A2.06 2.06 0 0124 9.1v6.561a.649.649 0 01-.652.645.649.649 0 01-.653-.645V9.1a.762.762 0 00-.766-.758.762.762 0 00-.766.758v7.472a2.037 2.037 0 01-2.048 2.026 2.037 2.037 0 01-2.048-2.026v-12.5a.785.785 0 00-.788-.753.785.785 0 00-.789.752l-.001 15.904A2.037 2.037 0 0113.441 22a2.037 2.037 0 01-2.048-2.026V18.04c0-.356.292-.645.652-.645.36 0 .652.289.652.645v1.934c0 .263.142.506.372.638.23.131.514.131.744 0a.734.734 0 00.372-.638V4.07c0-1.143.937-2.07 2.093-2.07zm-5.674 0c1.156 0 2.093.927 2.093 2.07v11.523a.648.648 0 01-.652.645.648.648 0 01-.652-.645V4.07a.785.785 0 00-.789-.78.785.785 0 00-.789.78v14.013a2.06 2.06 0 01-2.07 2.048 2.06 2.06 0 01-2.071-2.048V9.1a.762.762 0 00-.766-.758.762.762 0 00-.766.758v3.8a2.06 2.06 0 01-2.071 2.049A2.06 2.06 0 010 12.9v-1.378c0-.357.292-.646.652-.646.36 0 .653.29.653.646V12.9c0 .418.343.757.766.757s.766-.339.766-.757V9.099a2.06 2.06 0 012.07-2.048 2.06 2.06 0 012.071 2.048v8.984c0 .419.343.758.767.758.423 0 .766-.339.766-.758V4.07c0-1.143.937-2.07 2.093-2.07z"/></svg>',
    proxy: '<svg ' + _svgStyle + ' viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg"><path fill="currentColor" d="M12 2l3.09 6.26L22 9.27l-5 4.87 1.18 6.88L12 17.77l-6.18 3.25L7 14.14 2 9.27l6.91-1.01L12 2z"/></svg>'
  };

  function getModelFamily(id) {
    const lower = id.toLowerCase();
    if (lower.startsWith('claude-') || lower.startsWith('anthropic')) return 'claude';
    if (lower.startsWith('gpt-') || lower === 'o1' || lower.startsWith('o1-') || lower.startsWith('o3-') || lower.startsWith('o4-')) return 'openai';
    if (lower.startsWith('deepseek')) return 'deepseek';
    if (lower.startsWith('qwen') || lower.startsWith('qwq') || lower.startsWith('qvq')) return 'qwen';
    if (lower.startsWith('glm') || lower.startsWith('chatglm') || lower.startsWith('zhipu') || lower.startsWith('codegeex')) return 'zhipu';
    if (lower.startsWith('minimax') || lower.startsWith('abab')) return 'minimax';
    if (lower.startsWith('mistral') || lower.startsWith('mixtral') || lower.startsWith('codestral')) return 'mistral';
    if (lower.startsWith('gemini') || lower.startsWith('gemma')) return 'gemini';
    if (lower.startsWith('llama') || lower.startsWith('meta-') || lower.startsWith('codellama')) return 'meta';
    if (lower === 'auto' || lower.startsWith('auto')) return 'proxy';
    return 'other';
  }

  function getModelFamilyLabel(family) {
    const labels = {
      claude: 'Claude (Anthropic)',
      openai: 'OpenAI',
      deepseek: 'DeepSeek',
      qwen: 'Qwen (Alibaba)',
      zhipu: 'GLM (Zhipu)',
      minimax: 'MiniMax',
      mistral: 'Mistral AI',
      gemini: 'Gemini (Google)',
      meta: 'LLaMA (Meta)',
      proxy: 'Proxy Aliases',
      other: currentLang === 'zh' ? '其他模型' : 'Other'
    };
    return labels[family] || family;
  }

  function getModelFamilyColor(family) {
    const colors = {
      claude: '#d97757',
      openai: '#10a37f',
      deepseek: '#4d6bfe',
      qwen: '#615ced',
      zhipu: '#3859ff',
      minimax: '#e1474f',
      mistral: '#ff7000',
      gemini: '#4285f4',
      meta: '#0668e1',
      proxy: '#888888',
      other: '#6b7280'
    };
    return colors[family] || '#6b7280';
  }

  function buildModelsGroupedHtml(models, thinkingSuffix) {
    if (models.length === 0) {
      return '<div class="api-view-loading">' + escapeHtml(t('api.noModels')) + '</div>';
    }

    // Group models by family
    const groups = {};
    const familyOrder = ['claude', 'openai', 'deepseek', 'qwen', 'zhipu', 'minimax', 'mistral', 'gemini', 'meta', 'proxy', 'other'];
    for (const m of models) {
      const family = getModelFamily(m.id || '');
      if (!groups[family]) groups[family] = [];
      groups[family].push(m);
    }

    let html = '';
    for (const family of familyOrder) {
      if (!groups[family] || groups[family].length === 0) continue;
      const familyModels = groups[family];
      const color = getModelFamilyColor(family);
      const svg = MODEL_SVGS[family] || MODEL_SVGS.proxy;
      const label = getModelFamilyLabel(family);

      html += '<div class="model-group">';
      html += '<div class="model-group-header">';
      html += '<span class="model-group-icon" style="color:' + color + '">' + svg + '</span>';
      html += '<span class="model-group-title">' + escapeHtml(label) + '</span>';
      html += '<span class="model-group-count">' + familyModels.length + '</span>';
      html += '</div>';
      html += '<div class="model-group-grid">';

      for (const m of familyModels) {
        const id = m.id || '';
        const isThinking = id.endsWith(thinkingSuffix);
        const supportsImage = m.supports_image || false;

        html += '<div class="model-item">';
        html += '<div class="model-info">';
        html += '<div class="model-name">' + escapeHtml(id) + '</div>';
        html += '<div class="model-badges">';
        if (isThinking) html += '<span class="model-badge model-badge--thinking"><i class="fa-solid fa-brain"></i> thinking</span>';
        if (supportsImage) html += '<span class="model-badge model-badge--image"><i class="fa-solid fa-image"></i> vision</span>';
        html += '</div>';
        html += '</div>';
        html += '</div>';
      }

      html += '</div></div>';
    }
    return html;
  }

  async function showStatsView() {
    const title = $('apiViewTitle');
    const body = $('apiViewBody');
    title.textContent = t('api.viewStatsTitle');
    body.innerHTML = '<div class="api-view-loading"><i class="fa-solid fa-spinner fa-spin"></i> ' + escapeHtml(t('api.loading')) + '</div>';
    openDialog('apiViewModal');

    try {
      const res = await api('/status');
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const d = await res.json();
      renderStatsView(body, d);
    } catch (e) {
      body.innerHTML = '<div class="api-view-error"><i class="fa-solid fa-circle-exclamation"></i> ' + escapeHtml(t('api.fetchError') + ': ' + e.message) + '</div>';
    }
  }

  function renderStatsView(container, d) {
    const version = String(d.version || currentVersion || '-').replace(/^v/i, '');
    let html = '<div class="stats-view-grid">';
    html += statsCard(t('api.statsVersion'), version, '');
    html += statsCard(t('api.statsAccounts'), d.accounts || 0, '');
    html += statsCard(t('api.statsAvailable'), d.available || 0, 'success');
    html += statsCard(t('api.statsTotalReqs'), formatNum(d.totalRequests || 0), 'info');
    html += statsCard(t('api.statsSuccessReqs'), formatNum(d.successRequests || 0), 'success');
    html += statsCard(t('api.statsFailedReqs'), formatNum(d.failedRequests || 0), 'danger');
    html += statsCard(t('api.statsTotalTokens'), formatNum(d.totalTokens || 0), '');
    html += statsCard(t('api.statsTotalCredits'), (d.totalCredits || 0).toFixed(2), 'info');
    html += '</div>';
    if (d.uptime !== undefined) {
      html += '<div class="stats-view-uptime"><i class="fa-solid fa-clock"></i> ' + escapeHtml(t('api.statsUptime')) + ': <strong>' + escapeHtml(formatUptime(d.uptime)) + '</strong></div>';
    }
    container.innerHTML = html;
  }

  function statsCard(label, value, variant) {
    const cls = variant ? ' stats-view-item--' + variant : '';
    return '<div class="stats-view-item' + cls + '"><div class="stats-view-value">' + escapeHtml(String(value)) + '</div><div class="stats-view-label">' + escapeHtml(label) + '</div></div>';
  }

  function wireEvents() {
    bindLoginEvents();
    bindShellEvents();
    bindAccountEvents();
    bindSettingsEvents();
    bindPromptFilterEvents();
    bindModalEvents();
    bindDetailEvents();
    bindTestEvents();
  }

  // Init
  async function init() {
    initTheme();
    await loadLocale(currentLang);
    if (currentLang !== 'zh') await loadLocale('zh');
    applyTranslations();
    initCustomSelectObserver();
    initPrivacyMode();
    initRememberMe();
    const yr = $('footerYear');
    if (yr) yr.textContent = new Date().getFullYear();
    wireEvents();
    if (password) tryAutoLogin();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();

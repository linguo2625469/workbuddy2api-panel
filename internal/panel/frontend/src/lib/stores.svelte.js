// 全局 UI 状态（Svelte 5 runes，模块级 $state；须用 .svelte.js 后缀）。
export const TITLES = {
  accounts: '账号池',
  usage: '用量',
  packages: '积分构成',
  taskscenter: '任务中心',
  models: '模型与档位',
  config: '配置',
  logs: '运行日志'
};

export function routeFromHash() {
  const h = (location.hash || '#accounts').slice(1);
  return h in TITLES ? h : 'accounts';
}

export const ui = $state({
  route: 'accounts',
  taskUid: null,
  addOpen: false,
  vouchersOpen: false,
  keyOpen: false
});

let toastSeq = 0;
export const toasts = $state([]);

export function toast(msg, cls = '') {
  const id = ++toastSeq;
  toasts.push({ id, msg, cls });
  setTimeout(() => {
    const i = toasts.findIndex((t) => t.id === id);
    if (i >= 0) toasts.splice(i, 1);
  }, 3600);
}

// 401 时弹密钥门（各视图 catch 后调此函数，保持行为一致）。
export function authFailed() {
  ui.keyOpen = true;
}

// 账号池总览（App 轮询写入，各视图只读）。
export const overview = $state({ data: null });

const LS_THEME = 'wb2api.theme';
export const theme = $state({
  mode: (() => {
    try {
      return localStorage.getItem(LS_THEME) || 'auto';
    } catch {
      return 'auto';
    }
  })()
});

export function effTheme() {
  const m = theme.mode;
  if (m === 'auto') {
    try {
      return matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
    } catch {
      return 'dark';
    }
  }
  return m;
}

export function applyTheme() {
  document.documentElement.dataset.theme = effTheme();
}

export function toggleTheme() {
  const next = effTheme() === 'light' ? 'dark' : 'light';
  theme.mode = next;
  try {
    localStorage.setItem(LS_THEME, next);
  } catch {
    /* 忽略 */
  }
  applyTheme();
}

// 首屏前套上明暗主题（与 HeroUI useTheme 同一个存储键）。
// 旧版面板把偏好存在 wb2api.theme（auto/light/dark），第一次打开新版时迁移过来。
(function () {
  var KEY = 'heroui-theme';
  try {
    var t = localStorage.getItem(KEY);
    if (!t) {
      var old = localStorage.getItem('wb2api.theme');
      t = old === 'light' || old === 'dark' ? old : 'system';
      localStorage.setItem(KEY, t);
    }
    if (t === 'system') t = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    document.documentElement.classList.add(t);
    document.documentElement.setAttribute('data-theme', t);
  } catch (e) { /* 无痕模式等拿不到存储：交给 React 里的 useTheme */ }
})();

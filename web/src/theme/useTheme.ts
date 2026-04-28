import { useEffect, useState, useCallback } from 'react';

export type Theme = 'system' | 'light' | 'dark';

const STORAGE_KEY = 'bloodhound:theme';

function isValid(t: string | null): t is Theme {
  return t === 'system' || t === 'light' || t === 'dark';
}

function applyTheme(theme: Theme) {
  const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
  const dark = theme === 'dark' || (theme === 'system' && prefersDark);
  document.documentElement.classList.toggle('dark', dark);
}

/**
 * useTheme returns the current Theme (system | light | dark) and a setter
 * that persists to localStorage. The hook also subscribes to OS theme
 * changes when in 'system' mode so the UI flips automatically.
 *
 * Theme is applied synchronously on first paint by an inline script in
 * index.html; this hook is responsible only for changes after that.
 */
export function useTheme() {
  const [theme, setThemeState] = useState<Theme>(() => {
    const stored = localStorage.getItem(STORAGE_KEY);
    return isValid(stored) ? stored : 'system';
  });

  useEffect(() => {
    applyTheme(theme);
    localStorage.setItem(STORAGE_KEY, theme);

    if (theme !== 'system') return;

    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = () => applyTheme('system');
    mq.addEventListener('change', handler);
    return () => mq.removeEventListener('change', handler);
  }, [theme]);

  const setTheme = useCallback((t: Theme) => setThemeState(t), []);
  return { theme, setTheme };
}

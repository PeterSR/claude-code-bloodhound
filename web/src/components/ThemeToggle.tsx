import { Sun, Moon, Monitor } from 'lucide-react';
import { useTheme, type Theme } from '../theme/useTheme';

const OPTIONS: { value: Theme; label: string; Icon: typeof Sun }[] = [
  { value: 'system', label: 'System', Icon: Monitor },
  { value: 'light', label: 'Light', Icon: Sun },
  { value: 'dark', label: 'Dark', Icon: Moon },
];

export default function ThemeToggle() {
  const { theme, setTheme } = useTheme();
  return (
    <div className="border-t border-zinc-200 dark:border-zinc-800 pt-3 mt-3">
      <div className="text-[10px] uppercase tracking-wider text-zinc-500 mb-2 px-1">
        Theme
      </div>
      <div className="flex gap-1">
        {OPTIONS.map(({ value, label, Icon }) => {
          const active = theme === value;
          return (
            <button
              key={value}
              onClick={() => setTheme(value)}
              title={label}
              aria-pressed={active}
              className={[
                'flex-1 flex items-center justify-center py-1.5 rounded-md transition',
                active
                  ? 'bg-zinc-200 dark:bg-zinc-800 text-zinc-900 dark:text-zinc-50'
                  : 'text-zinc-500 hover:bg-zinc-100 dark:hover:bg-zinc-900',
              ].join(' ')}
            >
              <Icon className="size-3.5" />
            </button>
          );
        })}
      </div>
    </div>
  );
}

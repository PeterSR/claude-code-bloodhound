import { Outlet, NavLink } from 'react-router-dom';
import {
  Activity,
  History as HistoryIcon,
  Files,
  Combine,
  Droplet,
  Bug,
  Settings as SettingsIcon,
  Dog,
} from 'lucide-react';
import ThemeToggle from './ThemeToggle';

type NavItem = {
  to: string;
  label: string;
  icon: typeof Activity;
  end?: boolean;
};

const NAV: NavItem[] = [
  { to: '/', label: 'Now', icon: Activity, end: true },
  { to: '/history', label: 'History', icon: HistoryIcon },
  { to: '/sessions', label: 'Sessions', icon: Files },
  { to: '/compactions', label: 'Compactions', icon: Combine },
  { to: '/leaks', label: 'Leaks', icon: Droplet },
  { to: '/debug', label: 'Debug', icon: Bug },
  { to: '/settings', label: 'Settings', icon: SettingsIcon },
];

export default function Layout() {
  return (
    <div className="flex h-screen bg-zinc-50 text-zinc-900 dark:bg-zinc-950 dark:text-zinc-100">
      <aside className="w-56 shrink-0 border-r border-zinc-200 dark:border-zinc-800 p-4 flex flex-col">
        <div className="flex items-center gap-2 mb-8 shrink-0">
          <Dog className="size-5 text-rose-500" />
          <span className="font-semibold tracking-tight">Bloodhound</span>
        </div>
        <nav className="flex-1 min-h-0 overflow-y-auto space-y-0.5">
          {NAV.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                [
                  'flex items-center gap-2 px-3 py-2 rounded-md text-sm transition',
                  isActive
                    ? 'bg-zinc-200 dark:bg-zinc-800 text-zinc-900 dark:text-zinc-50'
                    : 'text-zinc-600 dark:text-zinc-400 hover:bg-zinc-100 dark:hover:bg-zinc-900',
                ].join(' ')
              }
            >
              <Icon className="size-4" />
              <span>{label}</span>
            </NavLink>
          ))}
        </nav>
        <ThemeToggle />
      </aside>
      <main className="flex-1 p-8 overflow-auto">
        <Outlet />
      </main>
    </div>
  );
}

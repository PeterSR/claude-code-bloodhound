import { RefreshCw } from 'lucide-react';

type Props = {
  refreshing: boolean;
  onClick: () => void;
  label?: string;
};

export default function ReloadButton({ refreshing, onClick, label = 'Refresh' }: Props) {
  return (
    <button
      onClick={onClick}
      className="flex items-center gap-1.5 text-xs text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 transition"
      aria-label={label}
      title={label}
    >
      <RefreshCw className={`size-3.5 ${refreshing ? 'animate-spin' : ''}`} />
      {label}
    </button>
  );
}

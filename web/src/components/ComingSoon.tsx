import { Construction } from 'lucide-react';

type Props = {
  title: string;
  description: string;
};

export default function ComingSoon({ title, description }: Props) {
  return (
    <div>
      <h1 className="text-2xl font-semibold tracking-tight mb-2">{title}</h1>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl">{description}</p>
      <div className="mt-8 flex items-center gap-3 rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 px-4 py-3 max-w-md">
        <Construction className="size-4 text-amber-500 shrink-0" />
        <span className="text-sm text-zinc-600 dark:text-zinc-400">
          Coming in a future version.
        </span>
      </div>
    </div>
  );
}

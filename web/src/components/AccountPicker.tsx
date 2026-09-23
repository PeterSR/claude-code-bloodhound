import { useEffect } from 'react';
import { useApi } from '../hooks/useApi';
import { setAccount, useAccount, type AccountsResponse } from '../api/account';

/**
 * Sidebar dropdown choosing which Claude account the rest of the app shows.
 * Hidden while there is only one account, so a single-account install looks
 * exactly as it did before accounts existed.
 */
export default function AccountPicker() {
  const { data } = useApi<AccountsResponse>('/accounts', 60_000);
  const selected = useAccount();

  const accounts = data?.accounts ?? [];

  // A remembered account that no longer exists (merged away, database
  // reset) falls back to the daemon's default rather than 400ing every page.
  useEffect(() => {
    if (data && selected !== null && !accounts.some((a) => a.id === selected)) {
      setAccount(null);
    }
  }, [data, selected, accounts]);

  if (accounts.length < 2) return null;

  const value = selected ?? data?.primary_id ?? accounts[0].id;
  return (
    <div className="mb-6 shrink-0">
      <label
        htmlFor="account-picker"
        className="block text-[10px] uppercase tracking-wider text-zinc-500 mb-1.5 px-1"
      >
        Account
      </label>
      <select
        id="account-picker"
        value={value}
        onChange={(e) => {
          const id = Number(e.target.value);
          setAccount(id === data?.primary_id ? null : id);
        }}
        className="w-full rounded-md border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 px-2 py-1.5 text-sm truncate"
        title={accounts.find((a) => a.id === value)?.config_dirs?.join(', ')}
      >
        {accounts.map((a) => (
          <option key={a.id} value={a.id}>
            {a.name}
            {a.metered ? '' : ' (no meter)'}
          </option>
        ))}
      </select>
    </div>
  );
}

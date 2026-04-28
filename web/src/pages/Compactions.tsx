import ComingSoon from '../components/ComingSoon';

export default function Compactions() {
  return (
    <ComingSoon
      title="Compactions"
      description="Cost-by-cache-state scatter and a 'what would warm-only have cost?' callout. Compacting often on a warm cache is essentially free; a cold compaction can cost 10–20× as much in cost-weighted terms."
    />
  );
}

// Model naming and colour, shared by any page that breaks usage down by
// model.

/** Drop the vendor prefix: "claude-opus-4-8" → "Opus 4.8". */
export function modelLabel(model: string): string {
  const m = model.replace(/^claude-/, '');
  const parts = m.split('-');
  const family = parts[0];
  const version = parts.slice(1).join('.');
  const nice = family.charAt(0).toUpperCase() + family.slice(1);
  return version ? `${nice} ${version}` : nice;
}

/** The tier a model belongs to, which is what drives its price. */
export function modelTier(model: string): string {
  const m = model.replace(/^claude-/, '');
  return m.split('-')[0];
}

// Colour carries meaning here rather than just separating series: hue is
// the price tier, so the expensive models read as a single visual block no
// matter how many versions of them are in the mix. Shade separates
// versions within a tier, newest first.
const TIER_SHADES: Record<string, string[]> = {
  fable: ['#8b5cf6', '#a78bfa', '#c4b5fd'],
  mythos: ['#7c3aed', '#9333ea', '#a855f7'],
  opus: ['#f43f5e', '#fb7185', '#fda4af'],
  sonnet: ['#0ea5e9', '#38bdf8', '#7dd3fc'],
  haiku: ['#10b981', '#34d399', '#6ee7b7'],
};
const UNKNOWN_SHADES = ['#71717a', '#a1a1aa', '#d4d4d8'];

/**
 * Assign every model a stable colour. Stable is the point: colours are
 * derived from the model's own name and its position within its tier, not
 * from its rank in the data, so a model doesn't change colour when its
 * spend does or when the window changes.
 */
export function modelColors(models: string[]): Record<string, string> {
  const byTier: Record<string, string[]> = {};
  for (const m of models) {
    const t = modelTier(m);
    (byTier[t] ??= []).push(m);
  }
  const out: Record<string, string> = {};
  for (const [tier, list] of Object.entries(byTier)) {
    // Descending name puts the newest version of a tier first, so it takes
    // the strongest shade.
    const sorted = [...list].sort((a, b) => b.localeCompare(a));
    const shades = TIER_SHADES[tier] ?? UNKNOWN_SHADES;
    sorted.forEach((m, i) => {
      out[m] = shades[Math.min(i, shades.length - 1)];
    });
  }
  return out;
}

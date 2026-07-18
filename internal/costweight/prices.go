package costweight

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/PeterSR/claude-code-bloodhound/internal/config"
)

// The model price table is data, not code.
//
// A bundled default ships in the binary; an on-disk override in the state
// directory can add or correct models without a rebuild, so a model
// Anthropic launches after this binary was built can be priced by editing a
// file (or, later, by a self-heal that writes one). Nothing in the weighting
// path hardcodes a model name.
//
// Prices are stored per million tokens, input and output separately, rather
// than as a single multiplier. The multiplier every downstream surface uses
// is derived: a model's input price over the baseline's input price. Storing
// both prices is deliberate — it keeps the raw source of the number, and it
// lets the code notice when a model breaks the assumption the whole
// single-scalar scheme rests on (that output costs exactly 5x input on every
// model). See Table.BreaksRatioAssumption.

//go:embed prices_default.json
var defaultPricesJSON []byte

// PriceTableVersion is the schema version the code understands. A file on
// disk at a different version is ignored in favor of the bundled default,
// exactly like the extractor — a binary rollback must not choke on a
// newer-shaped file, and a stale file must not outrank fresher code.
const PriceTableVersion = 1

const pricesFileName = "prices.json"

// outputInputRatio is the multiple every current model prices output at
// relative to its own input ($10/$50, $5/$25, $3/$15, $1/$5). The composition
// of a per-token-type weighting with a single per-model scalar is only valid
// while this holds; a model that violates it needs a per-model token-type
// table instead, and BreaksRatioAssumption flags that case loudly.
const outputInputRatio = 5.0

// PriceOrigin says where the active table came from.
type PriceOrigin string

const (
	PriceOriginUser    PriceOrigin = "user"
	PriceOriginDefault PriceOrigin = "default"
)

// PriceStatusUnverified marks a price the daemon found on its own (a web
// search on discovering an unknown model) that has not yet been
// corroborated by local calibration. Such a price is used, but flagged: a
// price has no on-machine ground truth to validate against, unlike the
// extractor's regex-versus-live-screen, so an auto-found one is a prior,
// not a measurement. An empty status is a trusted price (bundled default or
// user-curated).
const PriceStatusUnverified = "unverified"

// ModelPrice is one model's list price per million tokens.
type ModelPrice struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	// Source is where the price came from — a URL for an auto-found price,
	// empty on the bundled defaults. Surfaced so an unverified price is
	// never mistaken for a known one.
	Source string `json:"source,omitempty"`
	// Status is "" (trusted) or PriceStatusUnverified (auto-found, awaiting
	// corroboration).
	Status string `json:"status,omitempty"`
}

// Table is a full price table: the baseline model everything is measured
// against, and each model's prices.
type Table struct {
	Version     int                   `json:"version"`
	Baseline    string                `json:"baseline"`
	GeneratedAt string                `json:"generated_at,omitempty"`
	GeneratedBy string                `json:"generated_by,omitempty"`
	Note        string                `json:"note,omitempty"`
	Models      map[string]ModelPrice `json:"models"`

	origin PriceOrigin
}

// Origin reports whether this table is the user override or the bundled
// default.
func (t *Table) Origin() PriceOrigin { return t.origin }

// Validate checks a table is usable: a positive-priced baseline that is
// itself in the table. Everything is measured against the baseline, so a
// missing or free baseline makes every multiplier meaningless.
func (t *Table) Validate() error {
	if t.Version != PriceTableVersion {
		return fmt.Errorf("price table: version %d, code expects %d", t.Version, PriceTableVersion)
	}
	if t.Baseline == "" {
		return errors.New("price table: no baseline model")
	}
	b, ok := t.Models[t.Baseline]
	if !ok {
		return fmt.Errorf("price table: baseline %q not in table", t.Baseline)
	}
	if b.Input <= 0 {
		return fmt.Errorf("price table: baseline %q has non-positive input price", t.Baseline)
	}
	return nil
}

// Multiplier returns a model's cost weight relative to the baseline, and
// whether the model was actually in the table. An untabulated model reports
// (DefaultMultiplier, false) so callers can both weight it conservatively
// and flag that the weight is a guess.
func (t *Table) Multiplier(model string) (weight float64, known bool) {
	base, ok := t.Models[t.Baseline]
	if !ok || base.Input <= 0 {
		return DefaultMultiplier, false
	}
	m, ok := t.Models[model]
	if !ok || m.Input < 0 {
		return DefaultMultiplier, false
	}
	return m.Input / base.Input, true
}

// Known reports whether the model is priced in the table.
func (t *Table) Known(model string) bool {
	_, ok := t.Models[model]
	return ok
}

// Price returns a model's list prices and whether it is in the table.
func (t *Table) Price(model string) (ModelPrice, bool) {
	p, ok := t.Models[model]
	return p, ok
}

// ModelNames lists every priced model, sorted.
func (t *Table) ModelNames() []string {
	out := make([]string, 0, len(t.Models))
	for m := range t.Models {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// BreaksRatioAssumption reports whether a model prices output at something
// other than outputInputRatio times its input. When true, the single scalar
// this package applies is only an approximation for that model, and the UI
// should say so. Unknown models report false — we make no claim about them.
func (t *Table) BreaksRatioAssumption(model string) bool {
	m, ok := t.Models[model]
	if !ok || m.Input <= 0 || m.Output <= 0 {
		return false
	}
	return math.Abs(m.Output/m.Input-outputInputRatio) > 0.01
}

// Clone returns a deep copy, so a caller can add or change a model without
// mutating the shared cached table under other readers.
func (t *Table) Clone() *Table {
	c := *t
	c.Models = make(map[string]ModelPrice, len(t.Models))
	for k, v := range t.Models {
		c.Models[k] = v
	}
	return &c
}

// UnpricedModels returns the subset of seen that the table does not price,
// sorted. This is the work-list for on-discovery price healing.
func (t *Table) UnpricedModels(seen []string) []string {
	var out []string
	for _, m := range seen {
		if _, ok := t.Models[m]; !ok {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// AddUnverifiedPrice writes a newly-found price into the on-disk override.
// It materializes the current table (bundled or user) to disk with the new
// model added, marked unverified with its source, and reloads the cache so
// the price takes effect immediately.
//
// generatedAt is passed in rather than read from the clock because the
// workflow layer forbids wall-clock reads mid-run; callers stamp it.
func AddUnverifiedPrice(model string, input, output float64, source, generatedAt string) error {
	if model == "" || input <= 0 || output <= 0 {
		return fmt.Errorf("price table: refusing to add %q at input=%g output=%g", model, input, output)
	}
	base, err := LoadTable()
	if err != nil {
		return err
	}
	next := base.Clone()
	next.Models[model] = ModelPrice{
		Input:  input,
		Output: output,
		Source: source,
		Status: PriceStatusUnverified,
	}
	next.GeneratedAt = generatedAt
	next.GeneratedBy = "priceheal"
	return SaveTable(next)
}

// parseTable unmarshals and validates a table from JSON.
func parseTable(data []byte, origin PriceOrigin) (*Table, error) {
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("price table: %w", err)
	}
	t.origin = origin
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// bundledTable parses the embedded default. A failure here is a build error
// (the embedded JSON is wrong), so it panics — there is no safe fallback
// below the bundled default.
func bundledTable() *Table {
	t, err := parseTable(defaultPricesJSON, PriceOriginDefault)
	if err != nil {
		panic("costweight: bundled price table invalid: " + err.Error())
	}
	return t
}

// LoadTable returns the active table and where it came from.
//
// Ladder, mirroring the extractor but softer on corruption: prefer the user
// file at $XDG_STATE_HOME/bloodhound/prices.json; fall back to the bundled
// default if it is missing, the wrong version, or unreadable. Unlike the
// extractor, a corrupt user file does NOT hard-error — cost weighting
// underlies every page, so it must always resolve to something usable. The
// bundled default is that floor.
func LoadTable() (*Table, error) {
	dir, err := config.StateDir()
	if err != nil {
		return bundledTable(), nil
	}
	data, err := os.ReadFile(filepath.Join(dir, pricesFileName))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// Unreadable for some reason other than absence; the bundled
			// default keeps the app working.
			return bundledTable(), nil
		}
		return bundledTable(), nil
	}
	t, err := parseTable(data, PriceOriginUser)
	if err != nil {
		// Corrupt or version-mismatched user file: fall back rather than
		// break every weighted number in the product.
		return bundledTable(), nil
	}
	return t, nil
}

// PricesPath returns the on-disk override path.
func PricesPath() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, pricesFileName), nil
}

// SaveTable writes a table to the override path atomically.
func SaveTable(t *Table) error {
	if err := t.Validate(); err != nil {
		return err
	}
	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	if err := config.EnsureDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, pricesFileName)
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	Reload()
	return nil
}

// The active table is cached: it is read on every cost-weighted SQL query
// and every CW() call, so re-reading the file each time would be wasteful.
// The cache is reloadable so a self-heal or manual edit takes effect without
// a restart.
var (
	tableMu    sync.RWMutex
	cachedTbl  *Table
	tableReady bool
)

// Current returns the active table, loading it once on first use.
func Current() *Table {
	tableMu.RLock()
	if tableReady {
		t := cachedTbl
		tableMu.RUnlock()
		return t
	}
	tableMu.RUnlock()

	tableMu.Lock()
	defer tableMu.Unlock()
	if !tableReady {
		t, _ := LoadTable()
		cachedTbl, tableReady = t, true
	}
	return cachedTbl
}

// Reload drops the cache so the next Current re-reads from disk. Called
// after SaveTable and available to the daemon after a price self-heal.
func Reload() {
	tableMu.Lock()
	cachedTbl, tableReady = nil, false
	tableMu.Unlock()
}

// SetTableForTest swaps the active table in tests without touching disk.
func SetTableForTest(t *Table) {
	tableMu.Lock()
	cachedTbl, tableReady = t, true
	tableMu.Unlock()
}

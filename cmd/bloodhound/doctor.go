package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PeterSR/claude-code-bloodhound/internal/api/routes"
	"github.com/PeterSR/claude-code-bloodhound/internal/config"
	"github.com/PeterSR/claude-code-bloodhound/internal/projectconfig"
	"github.com/PeterSR/claude-code-bloodhound/internal/store"
	"github.com/PeterSR/claude-code-bloodhound/internal/usage"
	"github.com/PeterSR/claude-code-bloodhound/internal/version"
)

var (
	doctorJSON        bool
	doctorProjectsDir string
)

// doctorReport is the --json shape of `bloodhound doctor`. Built alongside
// the human-readable sections in RunE below (same values, same order) so
// the two can't drift into reporting different things; see the io.Discard
// trick at the top of RunE for how that's kept to one code path.
type doctorReport struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoOS      string `json:"go_os"`
	GoArch    string `json:"go_arch"`
	GoVersion string `json:"go_version"`

	Paths struct {
		DataDir             string `json:"data_dir"`
		StateDir            string `json:"state_dir"`
		ConfigDir           string `json:"config_dir"`
		ConfigFile          string `json:"config_file"`
		ClaudeProjectsDir   string `json:"claude_projects_dir"`
		ClaudeProjectsFound bool   `json:"claude_projects_found"`
	} `json:"paths"`

	Config    *config.Config `json:"config,omitempty"`
	APISocket string         `json:"api_socket,omitempty"`

	// ProjectConfig is what the working directory doctor was run from says
	// about bloodhound's behaviour toward its sessions. A separate schema
	// from Config rather than an overlay on it — see internal/projectconfig.
	ProjectConfig *projectConfigReport `json:"project_config,omitempty"`

	ClaudeBinaryPath  string `json:"claude_binary_path,omitempty"`
	ClaudeBinaryState string `json:"claude_binary_state,omitempty"`

	Database struct {
		Path          string `json:"path"`
		SchemaVersion int    `json:"schema_version"`
		DeviceID      string `json:"device_id"`
	} `json:"database"`

	Extractor          *usage.Extractor `json:"extractor,omitempty"`
	ExtractorOrigin    string           `json:"extractor_origin,omitempty"`
	ExtractorStatePath string           `json:"extractor_state_path,omitempty"`
	ExtractorSnapshot  string           `json:"extractor_snapshot_path,omitempty"`

	// Coverage is the on-disk-vs-ingested tally per transcript shape (see
	// internal/store/coverage_ops.go). Always populated when the database
	// opened successfully; doctor.go only calls CheckCoverage and prints
	// the result, all the counting logic lives in store.
	Coverage store.CoverageReport `json:"coverage"`

	// HistoryRepair is a dry run of `repair --dedupe-history` at the
	// default window and default (not --all) scope. Nil only when the
	// dry run itself failed. Coverage answers "is anything missing";
	// this answers the other half, "is anything counted twice" — the
	// state an upgrader lands in when their history predates the
	// ingester fix.
	HistoryRepair *store.RepairStats `json:"history_repair,omitempty"`

	// Errors collects anything that went wrong gathering the above without
	// aborting the rest of the report (mirrors the human output's inline
	// "ERROR" annotations, just collected in one place for --json).
	Errors []string `json:"errors,omitempty"`
}

// projectConfigReport is the doctor view of the nearest .bloodhound file:
// where it was found, what it resolved to, and anything in it this build did
// not recognise.
type projectConfigReport struct {
	Cwd         string               `json:"cwd"`
	Path        string               `json:"path,omitempty"`
	Config      projectconfig.Config `json:"config"`
	UnknownKeys []string             `json:"unknown_keys,omitempty"`
}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Print build info, paths, config status, DB schema version, and transcript coverage",
	RunE: func(cmd *cobra.Command, args []string) error {
		realOut := cmd.OutOrStdout()
		// Human text is printed unconditionally below via w, same as
		// before this command had a --json mode; when --json is set, w
		// writes to io.Discard so none of it reaches the terminal, and
		// the doctorReport gathered alongside is marshalled instead. This
		// keeps one code path building the data (no separate JSON-only
		// branch to fall out of sync with the printed one).
		w := realOut
		if doctorJSON {
			w = io.Discard
		}

		var rep doctorReport
		rep.Version, rep.Commit, rep.BuildDate = version.Version, version.Commit, version.Date
		rep.GoOS, rep.GoArch, rep.GoVersion = runtime.GOOS, runtime.GOARCH, runtime.Version()

		fmt.Fprintf(w, "bloodhound %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		fmt.Fprintf(w, "go runtime: %s/%s (%s)\n", runtime.GOOS, runtime.GOARCH, runtime.Version())

		fmt.Fprintln(w, "\nPaths:")
		dataDir, err := config.DataDir()
		rep.Paths.DataDir = dataDir
		fmt.Fprintf(w, "  data dir:    %s%s\n", dataDir, errSuffix(err))
		stateDir, err := config.StateDir()
		rep.Paths.StateDir = stateDir
		fmt.Fprintf(w, "  state dir:   %s%s\n", stateDir, errSuffix(err))
		cfgDir, err := config.ConfigDir()
		rep.Paths.ConfigDir = cfgDir
		fmt.Fprintf(w, "  config dir:  %s%s\n", cfgDir, errSuffix(err))
		cfgPath, err := config.Path()
		rep.Paths.ConfigFile = cfgPath
		fmt.Fprintf(w, "  config file: %s%s\n", cfgPath, errSuffix(err))
		projects, err := config.ClaudeProjectsDir()
		exists := dirExists(projects)
		rep.Paths.ClaudeProjectsDir = projects
		rep.Paths.ClaudeProjectsFound = exists
		state := "missing"
		if exists {
			state = "found"
		}
		fmt.Fprintf(w, "  claude projects: %s (%s)%s\n", projects, state, errSuffix(err))

		fmt.Fprintln(w, "\nConfig:")
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintf(w, "  load: ERROR: %v\n", err)
			rep.Errors = append(rep.Errors, fmt.Sprintf("config load: %v", err))
		} else {
			rep.Config = &cfg
			sockPath, sockErr := routes.SocketPath()
			rep.APISocket = sockPath
			fmt.Fprintf(w, "  api socket:       %s%s\n", sockPath, errSuffix(sockErr))
			fmt.Fprintf(w, "  poll interval:    %ds\n", cfg.PollIntervalS)
			fmt.Fprintf(w, "  ingest interval:  %ds\n", cfg.IngestIntervalS)
			fmt.Fprintf(w, "  aggregate intvl:  %ds\n", cfg.AggregateIntervalS)
			fmt.Fprintf(w, "  plan tier:        %s (cosmetic)\n", cfg.PlanTier)
			fmt.Fprintf(w, "  join the pack:    %v\n", cfg.JoinThePack)
			binPath, binState := claudeBinaryStatus(cfg.ClaudeBinary)
			rep.ClaudeBinaryPath, rep.ClaudeBinaryState = binPath, binState
			fmt.Fprintf(w, "  claude binary:    %s (%s)\n", binPath, binState)
		}

		fmt.Fprintln(w, "\nProject config (for the current directory):")
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(w, "  cwd: ERROR: %v\n", err)
			rep.Errors = append(rep.Errors, fmt.Sprintf("cwd: %v", err))
		} else {
			pcfg, found, perr := projectconfig.Load(cwd)
			pr := &projectConfigReport{Cwd: cwd, Path: found.Path, Config: pcfg, UnknownKeys: found.UnknownKeys}
			rep.ProjectConfig = pr
			fmt.Fprintf(w, "  cwd:              %s\n", cwd)
			switch {
			case perr != nil:
				fmt.Fprintf(w, "  file: ERROR: %v\n", perr)
				rep.Errors = append(rep.Errors, fmt.Sprintf("project config: %v", perr))
			case found.Path == "":
				fmt.Fprintf(w, "  file:             none (defaults apply)\n")
			default:
				fmt.Fprintf(w, "  file:             %s\n", found.Path)
			}
			fmt.Fprintf(w, "  writeup nudge:    %v\n", pcfg.WriteupNudge)
			fmt.Fprintf(w, "  wakeup nudge:     %v\n", pcfg.WakeupNudge)
			if len(found.UnknownKeys) > 0 {
				// Named rather than ignored: the project schema is disjoint
				// from the global one, so a global key put in here parses,
				// does nothing, and would otherwise say nothing either.
				fmt.Fprintf(w, "  unknown keys:     %s (ignored)\n", strings.Join(found.UnknownKeys, ", "))
			}
		}

		fmt.Fprintln(w, "\nDatabase:")
		ctx := context.Background()
		s, err := store.Open(ctx)
		if err != nil {
			fmt.Fprintf(w, "  open: ERROR: %v\n", err)
			rep.Errors = append(rep.Errors, fmt.Sprintf("db open: %v", err))
			if doctorJSON {
				printDoctorJSON(realOut, rep)
			}
			return err
		}
		defer s.Close()
		rep.Database.Path = s.Path
		fmt.Fprintf(w, "  path: %s\n", s.Path)
		v, err := s.SchemaVersion(ctx)
		if err != nil {
			fmt.Fprintf(w, "  schema version: ERROR: %v\n", err)
			rep.Errors = append(rep.Errors, fmt.Sprintf("schema version: %v", err))
		} else {
			rep.Database.SchemaVersion = v
			fmt.Fprintf(w, "  schema version: %d\n", v)
		}
		id, err := s.DeviceID(ctx)
		if err != nil {
			fmt.Fprintf(w, "  device id: ERROR: %v\n", err)
			rep.Errors = append(rep.Errors, fmt.Sprintf("device id: %v", err))
		} else {
			rep.Database.DeviceID = id
			fmt.Fprintf(w, "  device id: %s\n", id)
		}

		fmt.Fprintln(w, "\nExtractor:")
		ext, origin, exErr := usage.LoadExtractor()
		if exErr != nil {
			fmt.Fprintf(w, "  load: ERROR: %v\n", exErr)
			rep.Errors = append(rep.Errors, fmt.Sprintf("extractor load: %v", exErr))
		} else {
			extractorPath, snapshotPath, _ := usage.ExtractorPaths()
			rep.Extractor = ext
			rep.ExtractorOrigin = string(origin)
			rep.ExtractorStatePath = extractorPath
			rep.ExtractorSnapshot = snapshotPath
			fmt.Fprintf(w, "  origin:    %s\n", origin)
			fmt.Fprintf(w, "  version:   %d\n", ext.Version)
			if ext.GeneratedAt != "" {
				fmt.Fprintf(w, "  generated: %s by %s\n", ext.GeneratedAt, ext.GeneratedBy)
			}
			fmt.Fprintf(w, "  fields:    %d\n", len(ext.Fields))
			fmt.Fprintf(w, "  state:     %s\n", extractorPath)
			fmt.Fprintf(w, "  snapshot:  %s\n", snapshotPath)
		}

		fmt.Fprintln(w, "\nTranscript coverage:")
		cov, covErr := s.CheckCoverage(ctx, store.CoverageOptions{ProjectsDir: doctorProjectsDir})
		if covErr != nil {
			fmt.Fprintf(w, "  ERROR: %v\n", covErr)
			rep.Errors = append(rep.Errors, fmt.Sprintf("coverage: %v", covErr))
		} else {
			rep.Coverage = cov
			printCoverageHuman(w, cov)
		}

		fmt.Fprintln(w, "\nHistory repair:")
		repairStats, repairErr := s.DedupeHistory(ctx, store.RepairOptions{
			WindowS: store.DefaultRepairWindowS,
		})
		if repairErr != nil {
			fmt.Fprintf(w, "  ERROR: %v\n", repairErr)
			rep.Errors = append(rep.Errors, fmt.Sprintf("history repair: %v", repairErr))
		} else {
			rep.HistoryRepair = &repairStats
			// covErr != nil means we don't know whether anything is
			// missing, so we can't claim ingest is done either; treat it
			// the same as "something is missing" and send them to ingest
			// first rather than risk recommending repair out of order.
			printHistoryRepairHuman(w, repairStats, covErr != nil || cov.AnyMissing())
		}

		if doctorJSON {
			printDoctorJSON(realOut, rep)
		}
		return nil
	},
}

// printCoverageHuman renders one line per shape plus a loud summary
// whenever any shape has a nonzero Missing count. Presentation only; all
// the counting (what's on disk, what's ingested, what's a legitimate
// skip) happens in store.CheckCoverage.
func printCoverageHuman(w io.Writer, cov store.CoverageReport) {
	for _, row := range cov.Rows {
		flag := ""
		if row.Missing > 0 {
			flag = "  <-- MISSING"
		}
		fmt.Fprintf(w, "  %-15s on_disk=%-5d ingested=%-5d skipped=%-5d missing=%-5d%s\n",
			row.Shape, row.OnDisk, row.Ingested, row.Skipped, row.Missing, flag)
	}
	if cov.AnyMissing() {
		fmt.Fprintf(w, "  !! %d transcript file(s) on disk are not represented in the database.\n", cov.TotalMissing())
		fmt.Fprintln(w, "     run `bloodhound ingest --force` and re-check with `bloodhound doctor`.")
	}
}

// printHistoryRepairHuman reports the dedupe-history dry run and, when
// there is something to collapse, names the next command. ingestFirst
// makes that recommendation ordered rather than a menu: `ingest --force`
// dedupes on the real message.id/requestId, so any session whose JSONL
// still exists is fixed exactly by ingest and only approximately by this
// heuristic. Sending someone to repair while transcripts are still
// un-ingested bakes a timing guess into rows that could have been
// reconstructed precisely.
func printHistoryRepairHuman(w io.Writer, st store.RepairStats, ingestFirst bool) {
	fmt.Fprintf(w, "  %-15s in_scope=%-5s runs=%-5s turns=%-5s\n", "dedupe-history",
		fmtCount(int64(st.SessionsInScope)), fmtCount(int64(st.RunsCollapsed)), fmtCount(int64(st.TurnsRemoved)))
	if st.RunsCollapsed == 0 {
		fmt.Fprintln(w, "  no duplicate turns from the pre-fix ingester.")
		return
	}
	fmt.Fprintf(w, "  !! %s turn(s) across %s session(s) are counted twice, inflating history by %s tokens.\n",
		fmtCount(int64(st.TurnsRemoved)), fmtCount(int64(st.SessionsChanged)),
		fmtTokens(float64(st.RawTokensBefore-st.RawTokensAfter)))
	if ingestFirst {
		fmt.Fprintln(w, "     run `bloodhound ingest --force` first, then re-check with `bloodhound doctor`:")
		fmt.Fprintln(w, "     ingest repairs on-disk sessions exactly, this pass only estimates.")
		return
	}
	fmt.Fprintln(w, "     preview with `bloodhound repair --dedupe-history`,")
	fmt.Fprintln(w, "     then write it with `bloodhound repair --dedupe-history --apply`.")
}

func printDoctorJSON(w io.Writer, rep doctorReport) {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintf(w, `{"error": %q}`+"\n", err.Error())
		return
	}
	fmt.Fprintln(w, string(b))
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "emit JSON instead of human text")
	doctorCmd.Flags().StringVar(&doctorProjectsDir, "projects-dir", "",
		"override Claude Code projects dir for the coverage check (default ~/.claude/projects)")
	rootCmd.AddCommand(doctorCmd)
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf(" (ERROR: %v)", err)
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func claudeBinaryStatus(override string) (string, string) {
	target := override
	if target == "" {
		target = "claude"
	}
	resolved, err := exec.LookPath(target)
	if err != nil {
		return target, "not found on PATH"
	}
	return resolved, "ok"
}

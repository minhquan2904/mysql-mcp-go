// Command benchreport renders the benchmark JSON into docs/benchmarks.md and a
// summary table in the README between <!-- BENCH:START --> and
// <!-- BENCH:END -->.
//
// Inputs, all written by scripts/bench.sh:
//
//	-rows   load rows from cmd/bench: a JSON array, or one JSON object per line
//	        {"scenario","concurrency","n","rps","p50_ms","p95_ms","p99_ms","max_ms","errors"}
//	-meta   a flat JSON object of environment strings: cpu_model, vcpu, ram_gb,
//	        os, docker_version, go_version, mysql_version, gosdk_version,
//	        commit, timestamp
//	-extra  named measurement groups, rendered as they come: an array of objects
//	        becomes a table (columns in order of first appearance), an array of
//	        scalars a one-column table, an object of scalars a key/value table,
//	        a scalar a paragraph line
//
// The README summary fills what the inputs can: image size from image_size,
// cold start from startup_cold_stats (else from the startup_cold_ms samples),
// idle and peak RSS from resources, the execute_sql line from the load rows,
// and the RunReadOnly and guard lines from micro_benchmarks entries named
// DirectQuery/<x>, RunReadOnly/<x> and GuardCheck/1KB.
//
// A value that looks like an IP address other than 127.x is refused: this
// report is published, so it must not say where it was measured. Nothing here
// invents a host name or an address of its own.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// loadRow is one benchmark cell as cmd/bench emits it.
type loadRow struct {
	Scenario    string  `json:"scenario"`
	Concurrency int     `json:"concurrency"`
	N           int     `json:"n"`
	RPS         float64 `json:"rps"`
	P50MS       float64 `json:"p50_ms"`
	P95MS       float64 `json:"p95_ms"`
	P99MS       float64 `json:"p99_ms"`
	MaxMS       float64 `json:"max_ms"`
	Errors      int     `json:"errors"`
}

// member is one member of a JSON object, kept in the order it was written.
type member struct {
	key string
	raw json.RawMessage
}

// pair is one rendered key/value line: an environment entry, a row of a
// key/value table, or one metric of the README summary.
type pair struct {
	key   string
	value string
}

type options struct {
	rows   string
	meta   string
	extra  string
	out    string
	readme string
}

// metaOrder is the order of the known environment keys in the report.
var metaOrder = []string{
	"cpu_model", "vcpu", "ram_gb", "os", "docker_version",
	"go_version", "mysql_version", "gosdk_version", "commit", "timestamp",
}

// scenarioNames labels the scenario IDs of the load table.
var scenarioNames = map[string]string{
	"S1": "list_sources",
	"S2": "execute_sql SELECT 1",
	"S3": "execute_sql SELECT * FROM orders",
	"S4": "execute_sql 1000 rows (order_items cross join)",
	"S5": "list_tables detailed",
	"S6": "execute_sql DELETE (guard rejection)",
}

// sectionOrder is the order of the sections of the report. Measurement groups
// join the first section whose name they contain; the load rows always fill the
// latency section.
var sectionOrder = []string{
	"Sizes", "Startup", "Resources", latencySection,
	"Micro-benchmarks", "Run-to-run variance", "Timeout accuracy", otherSection,
}

// sectionMatches decides which section a measurement group belongs to.
var sectionMatches = []struct {
	title string
	part  string
}{
	{"Sizes", "size"},
	{"Startup", "startup"},
	{"Resources", "resource"},
	{"Resources", "rss"},
	{"Micro-benchmarks", "micro"},
	{"Run-to-run variance", "variance"},
	{"Timeout accuracy", "timeout"},
}

const (
	otherSection      = "Other measurements"
	latencySection    = "Latency and throughput"
	benchStart        = "<!-- BENCH:START -->"
	benchEnd          = "<!-- BENCH:END -->"
	interpretationHdr = "## Interpretation"
	fullReportRef     = "docs/benchmarks.md"
)

// ipv4 matches dotted-quad addresses; only loopback ones may reach the report.
var ipv4 = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

func main() {
	var opts options
	flag.StringVar(&opts.rows, "rows", "", "load rows from cmd/bench (JSON array or JSON lines); \"-\" reads stdin")
	flag.StringVar(&opts.meta, "meta", "", "environment strings (flat JSON object); \"-\" reads stdin")
	flag.StringVar(&opts.extra, "extra", "", "named measurement groups (JSON object); \"-\" reads stdin")
	flag.StringVar(&opts.out, "out", fullReportRef, "report to write; \"-\" writes stdout")
	flag.StringVar(&opts.readme, "readme", "README.md", "file whose BENCH markers are filled in; \"\" skips it")
	flag.Parse()

	if err := generate(opts, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "benchreport:", err)
		os.Exit(1)
	}
}

// generate writes the full report and, unless disabled, the README snippet. The
// README markers are checked before anything is written, so a missing marker
// leaves both files untouched.
func generate(opts options, stdout, stderr io.Writer) error {
	stdin := false
	rowsRaw, err := readInput("rows", opts.rows, &stdin)
	if err != nil {
		return err
	}
	metaRaw, err := readInput("meta", opts.meta, &stdin)
	if err != nil {
		return err
	}
	extraRaw, err := readInput("extra", opts.extra, &stdin)
	if err != nil {
		return err
	}
	rows, err := loadRows(rowsRaw)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errors.New("-rows holds no benchmark row")
	}
	meta, err := loadMeta(metaRaw)
	if err != nil {
		return err
	}
	extras, err := loadExtras(extraRaw)
	if err != nil {
		return err
	}
	report, err := renderReport(rows, meta, extras, existingInterpretation(opts.out))
	if err != nil {
		return err
	}
	summary := renderSummary(rows, meta, extras)
	var readmeNew []byte
	if opts.readme != "" {
		if readmeNew, err = spliceReadme(opts.readme, summary); err != nil {
			return err
		}
	}
	for _, text := range []string{report, summary} {
		if err := checkNoHost(text); err != nil {
			return err
		}
	}
	if opts.out == "-" {
		if _, err := io.WriteString(stdout, report); err != nil {
			return err
		}
	} else {
		if dir := filepath.Dir(opts.out); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := os.WriteFile(opts.out, []byte(report), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "benchreport: wrote %s (%d load rows, %d groups)\n", opts.out, len(rows), len(extras))
	}
	if opts.readme != "" {
		if err := os.WriteFile(opts.readme, readmeNew, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "benchreport: filled the BENCH markers in %s\n", opts.readme)
	}
	return nil
}

// readInput reads one input file; "-" reads stdin and may be used only once.
func readInput(flagName, path string, stdin *bool) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	if path == "-" {
		if *stdin {
			return nil, errors.New("only one of -rows, -meta and -extra can read stdin")
		}
		*stdin = true
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("-%s: %w", flagName, err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-%s: %w", flagName, err)
	}
	return data, nil
}

// loadRows reads the load rows: a JSON array, or JSON values one after another
// (one per line, a trailing newline is fine).
func loadRows(data []byte) ([]loadRow, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var rows []loadRow
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("-rows: %w", err)
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		for {
			var row loadRow
			err := dec.Decode(&row)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("-rows: %w", err)
			}
			rows = append(rows, row)
		}
	}
	for i, row := range rows {
		if row.Scenario == "" {
			return nil, fmt.Errorf("-rows: row %d is missing its scenario", i+1)
		}
		if row.Concurrency <= 0 || row.N < 0 {
			return nil, fmt.Errorf("-rows: scenario %s has concurrency %d and n %d",
				row.Scenario, row.Concurrency, row.N)
		}
	}
	return rows, nil
}

// loadMeta reads the environment strings, known keys first and the rest sorted.
func loadMeta(data []byte) ([]pair, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	members, err := orderedObject(data)
	if err != nil {
		return nil, fmt.Errorf("-meta: %w", err)
	}
	values := make(map[string]string, len(members))
	for _, m := range members {
		values[m.key] = rawCell(m.raw)
	}
	out := make([]pair, 0, len(values))
	for _, key := range metaOrder {
		if value, ok := values[key]; ok {
			out = append(out, pair{key: key, value: value})
			delete(values, key)
		}
	}
	rest := make([]string, 0, len(values))
	for key := range values {
		rest = append(rest, key)
	}
	sort.Strings(rest)
	for _, key := range rest {
		out = append(out, pair{key: key, value: values[key]})
	}
	return out, nil
}

// loadExtras reads the named measurement groups, keeping their order.
func loadExtras(data []byte) ([]member, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	members, err := orderedObject(data)
	if err != nil {
		return nil, fmt.Errorf("-extra: %w", err)
	}
	return members, nil
}

// orderedObject decodes a JSON object into its members in source order, so the
// report keeps the order the measurements were taken in.
func orderedObject(data []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(data)))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("not a JSON object")
	}
	var members []member
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("object key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("key %q: %w", key, err)
		}
		members = append(members, member{key: key, raw: raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return members, nil
}

// renderReport builds the full markdown report.
func renderReport(rows []loadRow, meta []pair, extras []member, interpretation string) (string, error) {
	bySection := make(map[string][]string, len(sectionOrder))
	for _, m := range extras {
		text, err := groupMarkdown(m)
		if err != nil {
			return "", err
		}
		if text == "" {
			continue
		}
		section := sectionOf(m.key)
		bySection[section] = append(bySection[section], "### "+m.key+"\n\n"+text)
	}

	var b strings.Builder
	b.WriteString("# Benchmarks\n\n")
	b.WriteString("Generated by `cmd/benchreport` from the JSON written by `cmd/bench` and `scripts/bench.sh`.\n")
	b.WriteString("Every number below comes from a run; nothing is typed by hand. The section at the end is the\n")
	b.WriteString("only hand-written part, and it is kept as is when this file is generated again.\n")

	b.WriteString("\n## Environment\n\n")
	if len(meta) == 0 {
		b.WriteString("(no -meta input)\n")
	} else {
		b.WriteString(kvTable(meta))
	}
	for _, section := range sectionOrder {
		chunks := bySection[section]
		if section == latencySection {
			chunks = append(chunks, latencyTables(rows))
		}
		if len(chunks) == 0 {
			continue
		}
		b.WriteString("\n## " + section + "\n\n")
		b.WriteString(strings.Join(chunks, "\n"))
	}
	b.WriteString("\n" + interpretationHdr + "\n")
	if interpretation != "" {
		b.WriteString("\n" + interpretation + "\n")
	}
	return b.String(), nil
}

// latencyTables renders one table per scenario, in the order the scenarios
// appear in the input and by ascending concurrency inside a scenario.
func latencyTables(rows []loadRow) string {
	var order []string
	byScenario := map[string][]loadRow{}
	for _, row := range rows {
		if _, ok := byScenario[row.Scenario]; !ok {
			order = append(order, row.Scenario)
		}
		byScenario[row.Scenario] = append(byScenario[row.Scenario], row)
	}
	var b strings.Builder
	b.WriteString("End-to-end MCP tool calls over Streamable HTTP with a bearer token; p50/p95/p99 by nearest\n")
	b.WriteString("rank over every completed call of the cell.\n")
	for _, id := range order {
		scen := byScenario[id]
		sort.Slice(scen, func(i, j int) bool { return scen[i].Concurrency < scen[j].Concurrency })
		b.WriteString("\n### " + scenarioTitle(id) + "\n\n")
		b.WriteString("| Concurrency | n | RPS | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | Errors |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|\n")
		for _, row := range scen {
			fmt.Fprintf(&b, "| %d | %d | %.1f | %.1f | %.1f | %.1f | %.1f | %d |\n",
				row.Concurrency, row.N, row.RPS, row.P50MS, row.P95MS, row.P99MS, row.MaxMS, row.Errors)
		}
	}
	return b.String()
}

func scenarioTitle(id string) string {
	if name, ok := scenarioNames[id]; ok {
		return id + ": " + name
	}
	return id
}

// sectionOf maps a measurement group name to its section.
func sectionOf(name string) string {
	lower := strings.ToLower(name)
	for _, m := range sectionMatches {
		if strings.Contains(lower, m.part) {
			return m.title
		}
	}
	return otherSection
}

// groupMarkdown renders one named measurement group: tables for arrays and
// objects, a paragraph line for a scalar.
func groupMarkdown(m member) (string, error) {
	raw := bytes.TrimSpace(m.raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	switch raw[0] {
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return "", fmt.Errorf("-extra: group %q: %w", m.key, err)
		}
		return arrayMarkdown(elems)
	case '{':
		members, err := orderedObject(raw)
		if err != nil {
			return "", fmt.Errorf("-extra: group %q: %w", m.key, err)
		}
		rows := make([]pair, 0, len(members))
		for _, sub := range members {
			rows = append(rows, pair{key: sub.key, value: rawCell(sub.raw)})
		}
		return kvTable(rows), nil
	default:
		return cellOf(rawAny(raw)) + "\n", nil
	}
}

// arrayMarkdown renders an array of objects as a table with the union of their
// keys, and an array of scalars as a one-column table.
func arrayMarkdown(elems []json.RawMessage) (string, error) {
	if len(elems) == 0 {
		return "(empty)\n", nil
	}
	if first := bytes.TrimSpace(elems[0]); len(first) == 0 || first[0] != '{' {
		var b strings.Builder
		b.WriteString("| value |\n|---|\n")
		for _, el := range elems {
			fmt.Fprintf(&b, "| %s |\n", cellOf(rawAny(el)))
		}
		return b.String(), nil
	}
	var cols []string
	seen := map[string]bool{}
	rows := make([]map[string]string, 0, len(elems))
	for _, el := range elems {
		members, err := orderedObject(el)
		if err != nil {
			return "", err
		}
		row := make(map[string]string, len(members))
		for _, sub := range members {
			if !seen[sub.key] {
				seen[sub.key] = true
				cols = append(cols, sub.key)
			}
			row[sub.key] = rawCell(sub.raw)
		}
		rows = append(rows, row)
	}
	var b strings.Builder
	b.WriteString("| " + strings.Join(cols, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat("---|", len(cols)) + "\n")
	for _, row := range rows {
		cells := make([]string, len(cols))
		for i, col := range cols {
			cells[i] = row[col]
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return b.String(), nil
}

// kvTable renders key/value rows as a two-column table.
func kvTable(rows []pair) string {
	var b strings.Builder
	b.WriteString("| Key | Value |\n|---|---|\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "| %s | %s |\n", cell(row.key), cell(row.value))
	}
	return b.String()
}

// renderSummary builds the README snippet: the metrics the inputs can fill,
// the full load table, then a line naming the machine and the commit they came
// from.
func renderSummary(rows []loadRow, meta []pair, extras []member) string {
	lines := make([]pair, 0, 6)
	add := func(label, value string) {
		lines = append(lines, pair{key: label, value: value})
	}
	if v, ok := imageSize(extras); ok {
		add("Image size", v)
	}
	if v, ok := coldStart(extras); ok {
		add("Cold start → healthy (p50 / p95)", v)
	}
	if v, ok := resourcesLine(extras); ok {
		add("Idle RSS / peak RSS", v)
	}
	if len(rows) > 0 {
		lines = append(lines, selectOne(rows))
	}
	if v, ok := overheadLine(extras); ok {
		add("RunReadOnly overhead vs direct query", v)
	}
	if v, ok := guardLine(extras); ok {
		add("Guard check (1KB SQL)", v)
	}

	var b strings.Builder
	b.WriteString("| Metric | Value |\n|---|---|\n")
	for _, line := range lines {
		fmt.Fprintf(&b, "| %s | %s |\n", cell(line.key), cell(line.value))
	}
	if table := loadTable(rows); table != "" {
		b.WriteString("\n" + table)
	}
	if footer := metaLine(meta); footer != "" {
		b.WriteString("\n" + footer + "\n")
	}
	return b.String()
}

// loadTable renders every measured cell as one row per scenario, so the README
// carries the results themselves and not only the single sample above. Cells
// read "RPS / p50 / errors"; the errors are what makes a fast cell a bad one.
func loadTable(rows []loadRow) string {
	if len(rows) == 0 {
		return ""
	}
	type cellKey struct {
		scenario    string
		concurrency int
	}
	byCell := make(map[cellKey]loadRow, len(rows))
	order := make([]string, 0, len(rows))
	seenScenario := map[string]bool{}
	concs := make([]int, 0, 3)
	seenConc := map[int]bool{}
	for _, row := range rows {
		byCell[cellKey{row.Scenario, row.Concurrency}] = row
		if !seenScenario[row.Scenario] {
			seenScenario[row.Scenario] = true
			order = append(order, row.Scenario)
		}
		if !seenConc[row.Concurrency] {
			seenConc[row.Concurrency] = true
			concs = append(concs, row.Concurrency)
		}
	}
	sort.Strings(order)
	sort.Ints(concs)

	var b strings.Builder
	b.WriteString("Load, every cell (RPS / p50 / errors):\n\n")
	b.WriteString("| Scenario |")
	for _, c := range concs {
		fmt.Fprintf(&b, " c=%d |", c)
	}
	b.WriteString("\n|---|")
	for range concs {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, scenario := range order {
		fmt.Fprintf(&b, "| %s %s |", scenario, cell(scenarioLabel(scenario)))
		for _, c := range concs {
			row, ok := byCell[cellKey{scenario, c}]
			if !ok {
				b.WriteString(" — |")
				continue
			}
			b.WriteString(" " + cell(loadCellText(row)) + " |")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// loadCellText is one load cell: throughput, median, and how many calls failed.
func loadCellText(row loadRow) string {
	text := fmt.Sprintf("%.1f rps / %.1f ms / %d err", row.RPS, row.P50MS, row.Errors)
	if row.Errors > 0 && row.N > 0 {
		text += fmt.Sprintf(" (%.1f%%)", 100*float64(row.Errors)/float64(row.N))
	}
	return text
}

// selectOne picks the load row for the README line: the execute_sql row at
// concurrency 8 when it exists, else the execute_sql row with the highest
// concurrency, else the row with the highest concurrency of any scenario.
func selectOne(rows []loadRow) pair {
	best := rows[0]
	for _, row := range rows {
		if row.Concurrency == 8 && row.Scenario == "S2" {
			return oneLine(row)
		}
		if row.Scenario == "S2" && (best.Scenario != "S2" || row.Concurrency > best.Concurrency) {
			best = row
		} else if best.Scenario != "S2" && row.Concurrency > best.Concurrency {
			best = row
		}
	}
	return oneLine(best)
}

// oneLine renders the README's load line. A failing cell says so: a throughput
// number that hides half its calls failing would misread as a healthy result.
func oneLine(row loadRow) pair {
	label := fmt.Sprintf("%s @ c=%d (p50 / p99 / RPS)", scenarioLabel(row.Scenario), row.Concurrency)
	value := fmt.Sprintf("%.1f ms / %.1f ms / %.1f rps", row.P50MS, row.P99MS, row.RPS)
	if row.Errors > 0 && row.N > 0 {
		value += fmt.Sprintf(" (%.1f%% of calls failed)", 100*float64(row.Errors)/float64(row.N))
	}
	return pair{key: label, value: value}
}

func scenarioLabel(id string) string {
	if name, ok := scenarioNames[id]; ok {
		return name
	}
	return id
}

// imageSize renders the image (and binary) size group.
func imageSize(extras []member) (string, bool) {
	raw, ok := findGroup(extras, "image_size")
	if !ok {
		return "", false
	}
	if members, err := orderedObject(raw); err == nil {
		values := map[string]string{}
		for _, m := range members {
			values[m.key] = rawCell(m.raw)
		}
		image, hasImage := values["image_mb"]
		binary, hasBinary := values["binary_bytes"]
		switch {
		case hasImage && hasBinary:
			if f, err := strconv.ParseFloat(binary, 64); err == nil {
				return fmt.Sprintf("%s MB (binary %.1f MB)", image, f/(1<<20)), true
			}
			return image + " MB (binary " + binary + " bytes)", true
		case hasImage:
			return image + " MB", true
		case hasBinary:
			return binary + " bytes", true
		default:
			var parts []string
			for _, m := range members {
				parts = append(parts, m.key+" "+values[m.key])
			}
			return strings.Join(parts, ", "), len(parts) > 0
		}
	}
	if f, ok := number(rawAny(raw)); ok {
		return fmt.Sprintf("%.1f MB", f), true
	}
	return "", false
}

// coldStart renders the p50 and p95 of the cold start. It reads the stats the
// bench script already computed when they are there, and otherwise applies the
// same nearest-rank rule to the raw samples: a number, an array of numbers, or
// an array of objects carrying their sample in "ms".
func coldStart(extras []member) (string, bool) {
	if raw, ok := findGroup(extras, "startup_cold_stats"); ok {
		if members, err := orderedObject(raw); err == nil {
			values := map[string]float64{}
			for _, m := range members {
				if f, ok := number(rawAny(m.raw)); ok {
					values[m.key] = f
				}
			}
			p50, hasP50 := values["p50_ms"]
			p95, hasP95 := values["p95_ms"]
			if hasP50 && hasP95 {
				return fmt.Sprintf("%.0f ms / %.0f ms", p50, p95), true
			}
		}
	}
	raw, ok := findGroup(extras, "startup_cold_ms")
	if !ok {
		return "", false
	}
	vals := numbers(raw)
	if len(vals) == 0 {
		return "", false
	}
	return fmt.Sprintf("%.0f ms / %.0f ms", percentile(vals, 0.5), percentile(vals, 0.95)), true
}

// resourcesLine renders the idle and peak RSS of the resources group.
func resourcesLine(extras []member) (string, bool) {
	raw, ok := findGroup(extras, "resources")
	if !ok {
		return "", false
	}
	members, err := orderedObject(raw)
	if err != nil {
		return "", false
	}
	values := map[string]string{}
	for _, m := range members {
		values[m.key] = rawCell(m.raw)
	}
	idle, hasIdle := values["idle_rss_mb"]
	peak, hasPeak := values["peak_rss_mb"]
	switch {
	case hasIdle && hasPeak:
		return idle + " MB / " + peak + " MB", true
	case hasPeak:
		return "peak " + peak + " MB", true
	case hasIdle:
		return "idle " + idle + " MB", true
	default:
		return "", false
	}
}

// overheadLine compares RunReadOnly with the direct query of the same
// micro-benchmark: the name after the slash must match. The SELECT 1 pair is
// preferred, since that is the minimal query.
func overheadLine(extras []member) (string, bool) {
	entries := microEntries(extras)
	if run, ok := entries["RunReadOnly/select1"]; ok {
		if direct, ok := entries["DirectQuery/select1"]; ok {
			return overhead(run, direct), true
		}
	}
	for _, name := range sortedKeys(entries) {
		suffix, ok := strings.CutPrefix(name, "DirectQuery/")
		if !ok {
			continue
		}
		if run, ok := entries["RunReadOnly/"+suffix]; ok {
			return overhead(run, entries[name]), true
		}
	}
	return "", false
}

func overhead(run, direct float64) string {
	return fmt.Sprintf("%+.1f ms (single-use conn)", (run-direct)/1e6)
}

// guardLine renders the guard check of a 1KB statement.
func guardLine(extras []member) (string, bool) {
	entries := microEntries(extras)
	for _, name := range sortedKeys(entries) {
		if strings.Contains(name, "Guard") && strings.Contains(name, "1KB") {
			ns := entries[name]
			if ns < 1e6 {
				return fmt.Sprintf("%.1f µs", ns/1e3), true
			}
			return fmt.Sprintf("%.2f ms", ns/1e6), true
		}
	}
	return "", false
}

// microEntries maps micro-benchmark names to their ns_per_op, whatever order the
// name and the value come in.
func microEntries(extras []member) map[string]float64 {
	raw, ok := findGroup(extras, "micro_benchmarks")
	if !ok {
		return nil
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil {
		return nil
	}
	out := make(map[string]float64, len(elems))
	for _, el := range elems {
		members, err := orderedObject(el)
		if err != nil {
			continue
		}
		var (
			name   string
			ns     float64
			haveNS bool
		)
		for _, m := range members {
			switch m.key {
			case "name":
				name = rawCell(m.raw)
			case "ns_per_op", "ns_op":
				if f, ok := number(rawAny(m.raw)); ok {
					ns, haveNS = f, true
				}
			}
		}
		if name != "" && haveNS {
			out[name] = ns
		}
	}
	return out
}

// metaLine names the machine and the commit the numbers came from.
func metaLine(meta []pair) string {
	values := make(map[string]string, len(meta))
	for _, row := range meta {
		values[row.key] = row.value
	}
	var parts []string
	if vcpu, model := values["vcpu"], values["cpu_model"]; vcpu != "" && model != "" {
		parts = append(parts, vcpu+" vCPU "+model)
	} else if model != "" {
		parts = append(parts, model)
	}
	if ram := values["ram_gb"]; ram != "" {
		parts = append(parts, ram+" GB")
	}
	if mysql := values["mysql_version"]; mysql != "" {
		parts = append(parts, "MySQL "+mysql+" (same host)")
	}
	if commit := values["commit"]; commit != "" {
		parts = append(parts, "commit "+commit)
	}
	if ts := values["timestamp"]; ts != "" {
		parts = append(parts, ts)
	}
	if len(parts) == 0 {
		return ""
	}
	return "Measured on: " + strings.Join(parts, ", ") + ". Full report: " + fullReportRef
}

// spliceReadme replaces what sits between the BENCH markers of path. The
// markers themselves are kept; the README is not touched unless each marker is
// there exactly once and in the right order.
func spliceReadme(path, snippet string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if n := strings.Count(text, benchStart); n != 1 {
		return nil, fmt.Errorf("%s: want exactly one %s, found %d", path, benchStart, n)
	}
	if n := strings.Count(text, benchEnd); n != 1 {
		return nil, fmt.Errorf("%s: want exactly one %s, found %d", path, benchEnd, n)
	}
	start := strings.Index(text, benchStart) + len(benchStart)
	end := strings.Index(text, benchEnd)
	if end < start {
		return nil, fmt.Errorf("%s: %s comes before %s", path, benchEnd, benchStart)
	}
	return []byte(text[:start] + "\n" + snippet + "\n" + text[end:]), nil
}

// existingInterpretation keeps the hand-written part of an earlier report, so
// regenerating does not throw it away.
func existingInterpretation(path string) string {
	if path == "" || path == "-" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(data)
	i := strings.Index(text, interpretationHdr)
	if i < 0 {
		return ""
	}
	return strings.Trim(text[i+len(interpretationHdr):], "\n")
}

// checkNoHost refuses text that names an address; 127.x is allowed, it is the
// loopback the dev loop binds to.
func checkNoHost(text string) error {
	for _, addr := range ipv4.FindAllString(text, -1) {
		if !strings.HasPrefix(addr, "127.") {
			return fmt.Errorf("input names the address %s: the report is published, so remove it", addr)
		}
	}
	return nil
}

func findGroup(extras []member, name string) (json.RawMessage, bool) {
	for _, m := range extras {
		if m.key == name {
			return m.raw, true
		}
	}
	return nil, false
}

// numbers reads a group of samples: a number, an array of numbers, or an array
// of objects carrying their sample in "ms".
func numbers(raw json.RawMessage) []float64 {
	switch v := rawAny(raw).(type) {
	case float64:
		return []float64{v}
	case []any:
		var out []float64
		for _, el := range v {
			switch sample := el.(type) {
			case float64:
				out = append(out, sample)
			case map[string]any:
				if f, ok := number(sample["ms"]); ok {
					out = append(out, f)
				}
			}
		}
		return out
	default:
		return nil
	}
}

// percentile is the nearest-rank percentile: the value at index ceil(p*N)-1 of
// the sorted samples, the same rule cmd/bench uses.
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p * float64(len(sorted))))
	rank = min(max(rank, 1), len(sorted))
	return sorted[rank-1]
}

// rawAny decodes a raw JSON value.
func rawAny(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// rawCell renders a raw JSON value as a table cell.
func rawCell(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return cell(s)
		}
	}
	return cellOf(rawAny(trimmed))
}

// cellOf renders a decoded JSON value as a table cell.
func cellOf(v any) string { return cell(scalar(v)) }

// scalar renders a decoded JSON value as text.
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// number reports the numeric value of a JSON number.
func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// cell makes text safe inside a markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

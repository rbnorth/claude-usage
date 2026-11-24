// claude-usage: a quick morning check on Claude.
//
// It shows two things:
//   1. Anthropic's public status page (is something slow or down right now?)
//   2. Your organization's token usage by model (Opus, Sonnet, Haiku...) for
//      the last N hours, from the Admin Usage API.
//
// Usage needs an Admin API key (sk-ant-admin01-...) in ANTHROPIC_ADMIN_KEY.
// Regular workspace API keys are rejected by this endpoint. Without a key, the
// tool still shows the status section.
//
// Examples:
//   claude-usage                 # status + last 24h of usage
//   claude-usage -hours 4        # last 4 hours, hourly buckets
//   claude-usage -hours 168      # last 7 days
//   claude-usage -status-only    # just "is it Claude or is it me?"
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

var usageURL = usageURLConst

const (
	usageURLConst    = "https://api.anthropic.com/v1/organizations/usage_report/messages"
	defaultStatusURL = "https://status.anthropic.com/api/v2/summary.json"
	apiVersion       = "2023-06-01"
	userAgent        = "claude-usage/1.0"
)

// ---------- status page ----------

type statusSummary struct {
	Status struct {
		Indicator   string `json:"indicator"`
		Description string `json:"description"`
	} `json:"status"`
	Components []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Group  bool   `json:"group"`
	} `json:"components"`
	Incidents []struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		Impact    string `json:"impact"`
		Shortlink string `json:"shortlink"`
		UpdatedAt string `json:"updated_at"`
	} `json:"incidents"`
}

func showStatus(client *http.Client, statusURL string) error {
	req, err := http.NewRequest("GET", statusURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("status page unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status page returned %s", resp.Status)
	}
	var s statusSummary
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return fmt.Errorf("could not read status page: %w", err)
	}

	fmt.Println("== Anthropic status ==")
	fmt.Printf("%s %s\n", indicatorIcon(s.Status.Indicator), s.Status.Description)

	var degraded []string
	for _, c := range s.Components {
		if c.Group || c.Status == "operational" {
			continue
		}
		degraded = append(degraded, fmt.Sprintf("  - %s: %s", c.Name, strings.ReplaceAll(c.Status, "_", " ")))
	}
	if len(degraded) > 0 {
		fmt.Println("Components not fully operational:")
		for _, d := range degraded {
			fmt.Println(d)
		}
	}
	if len(s.Incidents) > 0 {
		fmt.Println("Open incidents:")
		for _, inc := range s.Incidents {
			fmt.Printf("  - [%s, %s impact] %s\n    %s\n", inc.Status, inc.Impact, inc.Name, inc.Shortlink)
		}
	}

	// Per-model view: Anthropic doesn't publish load numbers, but incidents and
	// components usually name the model family they affect.
	fmt.Println("By model family:")
	for _, fam := range []string{"Fable", "Mythos", "Opus", "Sonnet", "Haiku"} {
		var hits []string
		for _, inc := range s.Incidents {
			if strings.Contains(strings.ToLower(inc.Name), strings.ToLower(fam)) {
				hits = append(hits, fmt.Sprintf("%s (%s impact)", inc.Name, inc.Impact))
			}
		}
		for _, c := range s.Components {
			if c.Status != "operational" && strings.Contains(strings.ToLower(c.Name), strings.ToLower(fam)) {
				hits = append(hits, fmt.Sprintf("%s: %s", c.Name, strings.ReplaceAll(c.Status, "_", " ")))
			}
		}
		if len(hits) == 0 {
			fmt.Printf("  %-7s no reported issues\n", fam)
			continue
		}
		fmt.Printf("  %-7s ISSUE: %s\n", fam, strings.Join(hits, "; "))
	}
	if len(s.Incidents) > 0 {
		fmt.Println("  (An incident that names no model may affect all of them.)")
	}
	fmt.Println()
	return nil
}

func indicatorIcon(ind string) string {
	switch ind {
	case "none":
		return "[OK]  "
	case "minor":
		return "[MINOR]"
	case "major":
		return "[MAJOR]"
	case "critical":
		return "[CRITICAL]"
	default:
		return "[?]   "
	}
}

// ---------- usage report ----------

type usageResult struct {
	Model                string `json:"model"`
	UncachedInputTokens  int64  `json:"uncached_input_tokens"`
	CacheReadInputTokens int64  `json:"cache_read_input_tokens"`
	OutputTokens         int64  `json:"output_tokens"`
	CacheCreation        struct {
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
	ServerToolUse struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

type usageBucket struct {
	StartingAt string        `json:"starting_at"`
	EndingAt   string        `json:"ending_at"`
	Results    []usageResult `json:"results"`
}

type usagePage struct {
	Data     []usageBucket `json:"data"`
	HasMore  bool          `json:"has_more"`
	NextPage string        `json:"next_page"`
}

type modelTotals struct {
	Input, CacheWrite, CacheRead, Output, WebSearch int64
}

func (t modelTotals) total() int64 { return t.Input + t.CacheWrite + t.CacheRead + t.Output }

func fetchUsage(client *http.Client, key string, start, end time.Time, bucket string) (map[string]*modelTotals, map[string]*modelTotals, error) {
	totals := map[string]*modelTotals{}
	lastHour := map[string]*modelTotals{}
	hourAgo := end.Add(-1 * time.Hour)
	page := ""

	for {
		q := url.Values{}
		q.Set("starting_at", start.UTC().Format(time.RFC3339))
		q.Set("ending_at", end.UTC().Format(time.RFC3339))
		q.Set("bucket_width", bucket)
		q.Add("group_by[]", "model")
		if bucket == "1h" {
			q.Set("limit", "168")
		} else {
			q.Set("limit", "31")
		}
		if page != "" {
			q.Set("page", page)
		}

		req, err := http.NewRequest("GET", usageURL+"?"+q.Encode(), nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("anthropic-version", apiVersion)
		req.Header.Set("x-api-key", key)
		req.Header.Set("User-Agent", userAgent)

		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("usage API unreachable: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			hint := ""
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				hint = "\n  (This endpoint needs an Admin API key, sk-ant-admin01-..., not a workspace key.)"
			}
			return nil, nil, fmt.Errorf("usage API returned %s: %s%s", resp.Status, strings.TrimSpace(string(body)), hint)
		}

		var p usagePage
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, nil, fmt.Errorf("could not read usage response: %w", err)
		}
		for _, b := range p.Data {
			bStart, _ := time.Parse(time.RFC3339, b.StartingAt)
			recent := bucket == "1h" && !bStart.Before(hourAgo.Truncate(time.Hour))
			for _, r := range b.Results {
				m := r.Model
				if m == "" {
					m = "(unknown)"
				}
				add(totals, m, r)
				if recent {
					add(lastHour, m, r)
				}
			}
		}
		if !p.HasMore || p.NextPage == "" {
			break
		}
		page = p.NextPage
	}
	return totals, lastHour, nil
}

func add(dst map[string]*modelTotals, model string, r usageResult) {
	t, ok := dst[model]
	if !ok {
		t = &modelTotals{}
		dst[model] = t
	}
	t.Input += r.UncachedInputTokens
	t.CacheWrite += r.CacheCreation.Ephemeral1h + r.CacheCreation.Ephemeral5m
	t.CacheRead += r.CacheReadInputTokens
	t.Output += r.OutputTokens
	t.WebSearch += r.ServerToolUse.WebSearchRequests
}

func printTable(title string, totals map[string]*modelTotals) {
	fmt.Println(title)
	if len(totals) == 0 {
		fmt.Println("  No usage in this window.")
		fmt.Println()
		return
	}
	models := make([]string, 0, len(totals))
	var grand modelTotals
	for m, t := range totals {
		models = append(models, m)
		grand.Input += t.Input
		grand.CacheWrite += t.CacheWrite
		grand.CacheRead += t.CacheRead
		grand.Output += t.Output
		grand.WebSearch += t.WebSearch
	}
	sort.Slice(models, func(i, j int) bool { return totals[models[i]].total() > totals[models[j]].total() })

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "MODEL\tINPUT\tCACHE WRITE\tCACHE READ\tOUTPUT\tTOTAL\tSHARE\t")
	for _, m := range models {
		t := totals[m]
		share := 0.0
		if grand.total() > 0 {
			share = 100 * float64(t.total()) / float64(grand.total())
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%.1f%%\t\n", m, human(t.Input), human(t.CacheWrite), human(t.CacheRead), human(t.Output), human(t.total()), share)
	}
	fmt.Fprintf(w, "ALL MODELS\t%s\t%s\t%s\t%s\t%s\t\t\n", human(grand.Input), human(grand.CacheWrite), human(grand.CacheRead), human(grand.Output), human(grand.total()))
	w.Flush()
	if grand.WebSearch > 0 {
		fmt.Printf("Web search requests: %d\n", grand.WebSearch)
	}
	fmt.Println()
}

// human formats token counts as 1.2K / 3.4M / 5.6B.
func human(n int64) string {
	f := float64(n)
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", f/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", f/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", f/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ---------- main ----------

func main() {
	hours := flag.Int("hours", 24, "how many hours back to report (1-744)")
	statusOnly := flag.Bool("status-only", false, "only show the Anthropic status page")
	noStatus := flag.Bool("no-status", false, "skip the status page")
	statusURL := flag.String("status-url", defaultStatusURL, "Statuspage summary.json URL")
	flag.Parse()

	client := &http.Client{Timeout: 20 * time.Second}
	failed := false

	if !*noStatus {
		if err := showStatus(client, *statusURL); err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
			fmt.Println()
			failed = true
		}
	}
	if *statusOnly {
		exit(failed)
	}

	key := os.Getenv("ANTHROPIC_ADMIN_KEY")
	if key == "" {
		fmt.Println("== Usage ==")
		fmt.Println("ANTHROPIC_ADMIN_KEY is not set, so usage is skipped.")
		fmt.Println("Create an Admin key in the Claude Console (org admins only) and export it.")
		exit(failed)
	}

	if *hours < 1 || *hours > 744 {
		fmt.Fprintln(os.Stderr, "-hours must be between 1 and 744 (31 days)")
		os.Exit(2)
	}
	end := time.Now().UTC().Truncate(time.Minute)
	start := end.Add(-time.Duration(*hours) * time.Hour)
	bucket := "1h"
	if *hours > 168 {
		bucket = "1d"
		start = start.Truncate(24 * time.Hour)
	}

	totals, lastHour, err := fetchUsage(client, key, start, end, bucket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage:", err)
		os.Exit(1)
	}

	loc := time.Local
	printTable(fmt.Sprintf("== Usage by model, last %dh (%s to %s) ==", *hours,
		start.In(loc).Format("Jan 2 15:04"), end.In(loc).Format("Jan 2 15:04 MST")), totals)
	if bucket == "1h" && *hours > 1 {
		printTable("== Last hour ==", lastHour)
	}
	fmt.Println("Note: the usage API can lag a few minutes behind real time.")
	exit(failed)
}

func exit(failed bool) {
	if failed {
		os.Exit(1)
	}
	os.Exit(0)
}

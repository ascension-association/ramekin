// Package scan generates SBOMs with syft and matches them against
// vulnerability databases with grype, then applies the project's policy and
// sends alerts.
package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"
)

var severities = []string{"negligible", "low", "medium", "high", "critical"}

// Rank orders severities; unknown severities rank lowest, "none" highest.
func Rank(sev string) int {
	sev = strings.ToLower(sev)
	if sev == "none" {
		return len(severities)
	}
	return slices.Index(severities, sev)
}

type Finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Package  string `json:"package"`
	Version  string `json:"version"`
	FixedIn  string `json:"fixed_in,omitempty"`
}

type Report struct {
	Findings []Finding
}

func (r *Report) Counts() map[string]int {
	c := map[string]int{}
	for _, f := range r.Findings {
		c[strings.ToLower(f.Severity)]++
	}
	return c
}

// AtLeast returns findings at or above threshold that are not ignored.
func (r *Report) AtLeast(threshold string, ignore []string) []Finding {
	min := Rank(threshold)
	var out []Finding
	for _, f := range r.Findings {
		if Rank(f.Severity) >= min && min >= 0 && !slices.Contains(ignore, f.ID) {
			out = append(out, f)
		}
	}
	return out
}

// SBOM writes a syft JSON SBOM of an unpacked root filesystem.
func SBOM(ctx context.Context, rootfs, out string, stderr io.Writer) error {
	if _, err := exec.LookPath("syft"); err != nil {
		return fmt.Errorf("syft is required for SBOM generation (https://github.com/anchore/syft)")
	}
	cmd := exec.CommandContext(ctx, "syft", "scan", "dir:"+rootfs, "-q", "-o", "syft-json="+out)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("syft: %w", err)
	}
	return nil
}

// Grype matches an SBOM against the current vulnerability database.
func Grype(ctx context.Context, sbom string, stderr io.Writer) (*Report, error) {
	if _, err := exec.LookPath("grype"); err != nil {
		return nil, fmt.Errorf("grype is required for CVE scanning (https://github.com/anchore/grype)")
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "grype", "sbom:"+sbom, "-q", "-o", "json")
	cmd.Stdout, cmd.Stderr = &out, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("grype: %w", err)
	}
	return ParseGrype(out.Bytes())
}

func ParseGrype(b []byte) (*Report, error) {
	var doc struct {
		Matches []struct {
			Vulnerability struct {
				ID       string `json:"id"`
				Severity string `json:"severity"`
				Fix      struct {
					Versions []string `json:"versions"`
				} `json:"fix"`
			} `json:"vulnerability"`
			Artifact struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"artifact"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse grype output: %w", err)
	}
	r := &Report{}
	for _, m := range doc.Matches {
		r.Findings = append(r.Findings, Finding{
			ID:       m.Vulnerability.ID,
			Severity: strings.ToLower(m.Vulnerability.Severity),
			Package:  m.Artifact.Name,
			Version:  m.Artifact.Version,
			FixedIn:  strings.Join(m.Vulnerability.Fix.Versions, ", "),
		})
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		return Rank(r.Findings[i].Severity) > Rank(r.Findings[j].Severity)
	})
	return r, nil
}

// Print writes a human-readable summary.
func (r *Report) Print(w io.Writer, limit int) {
	c := r.Counts()
	fmt.Fprintf(w, "vulnerabilities: critical=%d high=%d medium=%d low=%d negligible=%d\n",
		c["critical"], c["high"], c["medium"], c["low"], c["negligible"])
	for i, f := range r.Findings {
		if i == limit {
			fmt.Fprintf(w, "  ... and %d more\n", len(r.Findings)-limit)
			break
		}
		fix := f.FixedIn
		if fix == "" {
			fix = "no fix yet"
		}
		fmt.Fprintf(w, "  %-9s %-20s %s %s (fixed in: %s)\n", f.Severity, f.ID, f.Package, f.Version, fix)
	}
}

// Alert posts findings to a webhook. The body carries both "text" (Slack,
// Mattermost, Google Chat) and "content" (Discord) plus structured details.
func Alert(ctx context.Context, webhook, subject string, findings []Finding) error {
	if webhook == "" {
		return nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %d vulnerabilities", subject, len(findings))
	for i, f := range findings {
		if i == 10 {
			fmt.Fprintf(&sb, "\n… and %d more", len(findings)-10)
			break
		}
		fmt.Fprintf(&sb, "\n• %s %s in %s %s", f.Severity, f.ID, f.Package, f.Version)
	}
	body, _ := json.Marshal(map[string]any{
		"text":     sb.String(),
		"content":  sb.String(),
		"subject":  subject,
		"findings": findings,
		"time":     time.Now().UTC(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("alert webhook: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode > 299 {
		return fmt.Errorf("alert webhook returned %s", resp.Status)
	}
	return nil
}

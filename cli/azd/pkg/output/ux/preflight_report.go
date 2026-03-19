// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package ux

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/output"
)

// PreflightReportSeverity mirrors the internal check severity for the UX report layer.
type PreflightReportSeverity int

const (
	// PreflightReportSuccess indicates the check passed — rendered with a green prefix.
	PreflightReportSuccess PreflightReportSeverity = iota
	// PreflightReportWarning indicates a non-blocking issue — rendered with a yellow prefix.
	PreflightReportWarning
	// PreflightReportError indicates a blocking issue — rendered with a red prefix.
	PreflightReportError
)

// PreflightReportItem represents a single finding from preflight validation.
type PreflightReportItem struct {
	// Severity indicates whether this is a success, warning, or error.
	Severity PreflightReportSeverity
	// Message describes the finding.
	Message string
}

// PreflightReport displays the results of local preflight validation.
// Warnings are shown first, followed by errors. Each entry is separated by a blank line.
type PreflightReport struct {
	Items []PreflightReportItem
}

func (r *PreflightReport) ToString(currentIndentation string) string {
	successes, warnings, errors := r.partition()
	if len(successes) == 0 && len(warnings) == 0 && len(errors) == 0 {
		return ""
	}

	var sb strings.Builder

	for i, s := range successes {
		if i > 0 {
			sb.WriteString("\n")
		}
		prefix := fmt.Sprintf("%s%s ", currentIndentation, passedPrefix)
		sb.WriteString(prefix + indentContinuationLines(s.Message, len(currentIndentation)+len("(✓) Passed: ")))
	}

	if len(successes) > 0 && (len(warnings) > 0 || len(errors) > 0) {
		sb.WriteString("\n")
	}

	for i, w := range warnings {
		if i > 0 {
			sb.WriteString("\n")
		}
		prefix := fmt.Sprintf("%s%s ", currentIndentation, warningPrefix)
		sb.WriteString(prefix + indentContinuationLines(w.Message, len(currentIndentation)+len("(!) Warning: ")))
	}

	if len(warnings) > 0 && len(errors) > 0 {
		sb.WriteString("\n")
	}

	for i, e := range errors {
		if i > 0 {
			sb.WriteString("\n")
		}
		prefix := fmt.Sprintf("%s%s ", currentIndentation, failedPrefix)
		sb.WriteString(prefix + indentContinuationLines(e.Message, len(currentIndentation)+len("(x) Failed: ")))
	}

	return sb.String()
}

func (r *PreflightReport) MarshalJSON() ([]byte, error) {
	successes, warnings, errors := r.partition()

	return json.Marshal(output.EventForMessage(
		fmt.Sprintf("preflight: %d passed, %d warning(s), %d error(s)",
			len(successes), len(warnings), len(errors))))
}

// HasErrors returns true if the report contains at least one error-level item.
func (r *PreflightReport) HasErrors() bool {
	for _, item := range r.Items {
		if item.Severity == PreflightReportError {
			return true
		}
	}
	return false
}

// HasWarnings returns true if the report contains at least one warning-level item.
func (r *PreflightReport) HasWarnings() bool {
	for _, item := range r.Items {
		if item.Severity == PreflightReportWarning {
			return true
		}
	}
	return false
}

// indentContinuationLines pads any lines after the first so they align beneath the
// opening prefix (e.g. "(x) Failed: ").  prefixLen is the visible width of that prefix.
func indentContinuationLines(msg string, prefixLen int) string {
	if !strings.Contains(msg, "\n") {
		return msg
	}
	pad := strings.Repeat(" ", prefixLen)
	return strings.ReplaceAll(msg, "\n", "\n"+pad)
}

// partition splits items into successes, warnings, and errors, preserving order within each group.
func (r *PreflightReport) partition() (successes, warnings, errors []PreflightReportItem) {
	for _, item := range r.Items {
		switch item.Severity {
		case PreflightReportSuccess:
			successes = append(successes, item)
		case PreflightReportError:
			errors = append(errors, item)
		default:
			warnings = append(warnings, item)
		}
	}
	return successes, warnings, errors
}

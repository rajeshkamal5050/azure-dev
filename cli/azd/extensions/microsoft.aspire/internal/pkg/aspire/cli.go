// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

// Package aspire provides utilities for interacting with the Aspire CLI
// and detecting Aspire projects.
package aspire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"microsoft.aspire/internal/exterrors"
)

// ansiRegex strips ANSI escape codes from terminal output.
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripAnsi removes ANSI escape codes from the given string.
func StripAnsi(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// CheckCli verifies the aspire CLI is installed and available on PATH.
// Returns the resolved path or a structured error with install instructions.
func CheckCli() (string, error) {
	// Check standard PATH first
	path, err := exec.LookPath("aspire")
	if err == nil {
		return path, nil
	}

	// Check common install location
	home, _ := os.UserHomeDir()
	if home != "" {
		aspirePath := filepath.Join(home, ".aspire", "bin", "aspire")
		if _, err := os.Stat(aspirePath); err == nil {
			return aspirePath, nil
		}
	}

	return "", exterrors.Dependency(
		exterrors.CodeAspireCliNotFound,
		"Aspire CLI not found on PATH or in ~/.aspire/bin/",
		"Install with: dotnet tool install -g Microsoft.Aspire.Cli\n"+
			"Or visit: https://learn.microsoft.com/dotnet/aspire/fundamentals/dotnet-aspire-cli",
	)
}

// RunPublishManifest executes `aspire do publish-manifest` to produce the
// manifest JSON and flat Bicep modules. Output is captured quietly and
// shown only on failure with structured error context.
func RunPublishManifest(ctx context.Context, appHostDir string, outputPath string) error {
	aspirePath, err := CheckCli()
	if err != nil {
		return err
	}

	args := []string{"do", "publish-manifest", "--project", appHostDir, "--output-path", outputPath}
	fmt.Printf("  $ aspire %s\n", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, aspirePath, args...)
	cmd.Dir = appHostDir

	var captured bytes.Buffer
	cmd.Stdout = &captured
	cmd.Stderr = &captured

	if err := cmd.Run(); err != nil {
		output := StripAnsi(captured.String())
		// Show concise failure summary — not the entire build log
		printFailureSummary(output)
		return exterrors.FromAspireCommand(err, "do publish-manifest", output)
	}

	return nil
}

// RunPublish executes `aspire publish` to produce the structured Bicep
// output (main.bicep + module directories). Output is captured quietly and
// shown only on failure with structured error context.
func RunPublish(ctx context.Context, appHostDir string, outputPath string) error {
	aspirePath, err := CheckCli()
	if err != nil {
		return err
	}

	args := []string{"publish", "--project", appHostDir, "--output-path", outputPath, "--publisher", "azure"}
	fmt.Printf("  $ aspire %s\n", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, aspirePath, args...)
	cmd.Dir = appHostDir

	var captured bytes.Buffer
	cmd.Stdout = &captured
	cmd.Stderr = &captured

	if err := cmd.Run(); err != nil {
		output := StripAnsi(captured.String())
		// Show concise failure summary — not the entire build log
		printFailureSummary(output)
		return exterrors.FromAspireCommand(err, "publish", output)
	}

	return nil
}

// PushResult contains the parsed output from aspire do push.
type PushResult struct {
	// Images maps service name → full image reference (e.g., "myacr.azurecr.io/app:aspire-deploy-20260324")
	Images map[string]string
}

// RunDoPush executes `aspire do push` to build all container images and push
// them to the ACR in the target resource group. The envVars map is merged
// into the process environment to pass Azure__ResourceGroup, etc.
// Output is streamed live (for user progress) and also captured for parsing
// image names. Returns the push results.
func RunDoPush(ctx context.Context, appHostDir string, envVars map[string]string) (*PushResult, error) {
	aspirePath, err := CheckCli()
	if err != nil {
		return nil, err
	}

	args := []string{"do", "push", "--project", appHostDir}
	fmt.Printf("  $ aspire %s\n", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, aspirePath, args...)
	cmd.Dir = appHostDir
	cmd.Env = os.Environ()
	for k, v := range envVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Stream output live to user while also capturing for parsing
	var captured bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &captured)
	cmd.Stderr = io.MultiWriter(os.Stderr, &captured)

	if err := cmd.Run(); err != nil {
		output := StripAnsi(captured.String())
		return nil, exterrors.FromAspireCommand(err, "do push", output)
	}

	// Parse pushed image names from captured output.
	// Aspire uses Markdown formatting: "Successfully pushed **name** to `tag`"
	// which becomes ANSI bold/code in terminal. We strip codes and collapse lines.
	result := &PushResult{
		Images: make(map[string]string),
	}

	// Strip ANSI escape codes from terminal output
	clean := StripAnsi(captured.String())

	// Collapse all whitespace (newlines, tabs, multi-spaces) into single spaces
	collapsed := strings.Join(strings.Fields(clean), " ")

	// Find all "Successfully pushed <name> to <image>" patterns
	for {
		idx := strings.Index(collapsed, "Successfully pushed ")
		if idx < 0 {
			break
		}
		collapsed = collapsed[idx:]
		parts := strings.Fields(collapsed)
		// "Successfully pushed app to acr.azurecr.io/app:aspire-deploy-xxx (2.7s)"
		if len(parts) >= 5 && parts[3] == "to" {
			svcName := parts[2]
			imageRef := parts[4]
			result.Images[svcName] = imageRef
			fmt.Printf("  ✓ Pushed %s → %s\n", svcName, imageRef)
		}
		// Move past this match
		collapsed = collapsed[len("Successfully pushed "):]
	}

	if len(result.Images) == 0 {
		fmt.Printf("  ⚠ No images found in push output.\n")
	}

	return result, nil
}

// ToEnvVarName converts a service name to an uppercase env var compatible name.
// e.g., "api-service" -> "APISERVICE"
func ToEnvVarName(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", ""))
}

// printFailureSummary extracts and displays only the meaningful error lines
// from Aspire CLI output, filtering out the verbose build log noise.
// Shows: error lines, the "Build FAILED" summary, and key counts.
func printFailureSummary(output string) {
	lines := strings.Split(output, "\n")

	fmt.Println()
	fmt.Println("  ── Aspire output (errors only) ──")

	var errorLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)

		// Keep actual error lines, build summary, and key status lines
		if strings.Contains(lower, "error :") ||
			strings.Contains(lower, "error msb") ||
			strings.Contains(lower, "build failed") ||
			strings.Contains(lower, "error(s)") ||
			strings.Contains(lower, "warning(s)") ||
			strings.HasPrefix(trimmed, "❌") ||
			strings.HasPrefix(trimmed, "✗") {
			errorLines = append(errorLines, trimmed)
		}
	}

	if len(errorLines) > 0 {
		// Deduplicate — MSBuild often repeats the same error block
		seen := make(map[string]bool)
		for _, line := range errorLines {
			// Normalize: strip file paths in [[...]] for dedup comparison
			key := line
			if idx := strings.Index(key, "[["); idx > 0 {
				key = strings.TrimSpace(key[:idx])
			}
			if !seen[key] && key != "" {
				seen[key] = true
				fmt.Printf("  %s\n", line)
			}
		}
	} else {
		// No error lines found — show last 10 lines as fallback
		start := len(lines) - 10
		if start < 0 {
			start = 0
		}
		for _, line := range lines[start:] {
			if strings.TrimSpace(line) != "" {
				fmt.Printf("  %s\n", strings.TrimSpace(line))
			}
		}
	}
	fmt.Println()
}

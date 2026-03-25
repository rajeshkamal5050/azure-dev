// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

// Package exterrors provides structured error helpers for the microsoft.aspire extension.
//
// Use plain Go errors until the current code can confidently choose a final
// category, code, and suggestion. At that point, create a structured error with
// one of the helpers in this package.
package exterrors

import (
	"fmt"
	"strings"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

// ────────────────────────────────────────────────────────────────
// Base error constructors
// ────────────────────────────────────────────────────────────────

// Validation returns a validation error for user-input or manifest errors.
func Validation(code, message, suggestion string) error {
	return &azdext.LocalError{
		Message:    message,
		Code:       code,
		Category:   azdext.LocalErrorCategoryValidation,
		Suggestion: suggestion,
	}
}

// Dependency returns a dependency error for missing resources or services.
func Dependency(code, message, suggestion string) error {
	return &azdext.LocalError{
		Message:    message,
		Code:       code,
		Category:   azdext.LocalErrorCategoryDependency,
		Suggestion: suggestion,
	}
}

// Compatibility returns a compatibility error for version/feature mismatches.
func Compatibility(code, message, suggestion string) error {
	return &azdext.LocalError{
		Message:    message,
		Code:       code,
		Category:   azdext.LocalErrorCategoryCompatibility,
		Suggestion: suggestion,
	}
}

// Configuration returns a configuration error.
func Configuration(code, message, suggestion string) error {
	return &azdext.LocalError{
		Message:    message,
		Code:       code,
		Category:   azdext.LocalErrorCategoryLocal,
		Suggestion: suggestion,
	}
}

// Internal returns an internal error for unexpected failures.
func Internal(code, message string) error {
	return &azdext.LocalError{
		Message:  message,
		Code:     code,
		Category: azdext.LocalErrorCategoryInternal,
	}
}

// User returns a user-action error (e.g., cancellation).
func User(code, message string) error {
	return &azdext.LocalError{
		Message:  message,
		Code:     code,
		Category: azdext.LocalErrorCategoryUser,
	}
}

// ────────────────────────────────────────────────────────────────
// Error converters for Aspire CLI commands
// ────────────────────────────────────────────────────────────────

// FromAspireCommand converts an Aspire CLI failure into a structured error
// with an actionable suggestion. It pattern-matches known error signatures
// from the Aspire CLI output (auth failures, build errors, missing tools).
func FromAspireCommand(err error, command string, output string) error {
	if err == nil {
		return nil
	}

	lower := strings.ToLower(output)

	// Auth failures — Aspire validates `az login` before pushing
	if strings.Contains(lower, "azure cli authentication failed") ||
		strings.Contains(lower, "az login") ||
		strings.Contains(lower, "please run az") {
		return Dependency(
			CodeAspireAuthFailed,
			fmt.Sprintf("aspire %s failed: Azure CLI authentication required. Run 'az login' to authenticate, then retry", command),
			"Aspire CLI uses Azure CLI credentials to push images to ACR.",
		)
	}

	// Token expired
	if strings.Contains(lower, "token") && (strings.Contains(lower, "expired") || strings.Contains(lower, "refresh")) {
		return Dependency(
			CodeAspireAuthFailed,
			fmt.Sprintf("aspire %s failed: Azure credentials expired. Run 'az login' to refresh, then retry", command),
			"Run 'az login' to refresh your credentials, then retry.",
		)
	}

	// ACR unauthorized — registry auth issue
	if strings.Contains(lower, "unauthorized") && strings.Contains(lower, "azurecr.io") {
		return Dependency(
			CodeAspireAuthFailed,
			fmt.Sprintf("aspire %s failed: unauthorized access to Azure Container Registry. Run 'az login' and ensure you have AcrPush role on the registry", command),
			"Then run 'az acr login --name <registry>' to authenticate Docker.",
		)
	}

	// Build failures — dotnet build or docker build
	if strings.Contains(lower, "build failed") ||
		strings.Contains(lower, "compilation error") ||
		strings.Contains(lower, "dotnet build") ||
		(strings.Contains(lower, "msbuild") && strings.Contains(lower, "error")) {
		return Dependency(
			CodeAspireBuildFailed,
			fmt.Sprintf("aspire %s failed: project build error. Run 'dotnet build' on your AppHost to check for errors", command),
			"Run 'dotnet build' on your AppHost project to check for compilation errors.",
		)
	}

	// Docker not running
	if strings.Contains(lower, "docker") &&
		(strings.Contains(lower, "not running") ||
			strings.Contains(lower, "cannot connect") ||
			strings.Contains(lower, "is the docker daemon running")) {
		return Dependency(
			CodeAspirePushFailed,
			fmt.Sprintf("aspire %s failed: Docker is not running. Start Docker Desktop or the Docker daemon, then retry", command),
			"Start Docker Desktop or the Docker daemon, then retry.",
		)
	}

	// .NET SDK missing
	if strings.Contains(lower, "dotnet") &&
		(strings.Contains(lower, "not found") || strings.Contains(lower, "not installed")) {
		return Dependency(
			CodeDotnetSdkNotFound,
			fmt.Sprintf("aspire %s failed: .NET SDK not found. Install from https://dot.net/download", command),
			"Install the .NET SDK from https://dot.net/download, then retry.",
		)
	}

	// Project file issues
	if strings.Contains(lower, "project file") ||
		strings.Contains(lower, "could not find") ||
		strings.Contains(lower, "no such file") {
		return Validation(
			CodeAppHostNotFound,
			fmt.Sprintf("aspire %s failed: project file not found. Ensure your AppHost .csproj or apphost.cs exists", command),
			"Ensure your AppHost .csproj or apphost.cs exists and the --project path is correct.",
		)
	}

	// Network / connectivity
	if strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "network") && strings.Contains(lower, "unreachable") ||
		strings.Contains(lower, "name resolution") {
		return Dependency(
			CodeAspirePushFailed,
			fmt.Sprintf("aspire %s failed: network connectivity issue. Check your connection to Azure services", command),
			"Check your network connection and ensure you can reach Azure services.",
		)
	}

	// Generic fallback — printFailureSummary already displayed output inline
	return Dependency(
		CodeAspirePushFailed,
		fmt.Sprintf("aspire %s failed: %s. Check the Aspire output above for details", command, err),
		"Check the Aspire output above for details.",
	)
}

// ────────────────────────────────────────────────────────────────
// Error converters for environment / azd client operations
// ────────────────────────────────────────────────────────────────

// FromEnvGet wraps environment read failures with actionable context.
func FromEnvGet(err error, operation string) error {
	if err == nil {
		return nil
	}
	return Configuration(
		CodeEnvGetFailed,
		fmt.Sprintf("failed to %s: %s", operation, err),
		"Check that your azd environment exists with 'azd env list'.",
	)
}

// FromEnvSet wraps environment write failures with actionable context.
func FromEnvSet(err error, key string) error {
	if err == nil {
		return nil
	}
	return Configuration(
		CodeEnvSetFailed,
		fmt.Sprintf("failed to set %s: %s", key, err),
		fmt.Sprintf("Try setting it manually with 'azd env set %s <value>'.", key),
	)
}

// ────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────

// tailLines returns the last n non-empty lines from output.
func tailLines(output string, n int) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// Filter empty lines
	var nonEmpty []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) > n {
		nonEmpty = nonEmpty[len(nonEmpty)-n:]
	}
	return strings.Join(nonEmpty, "\n")
}

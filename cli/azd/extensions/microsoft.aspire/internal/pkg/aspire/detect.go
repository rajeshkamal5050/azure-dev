// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package aspire

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DetectAppHost scans the project directory for an Aspire AppHost.
// Supports both traditional .csproj-based AppHosts (with Aspire.AppHost.Sdk)
// and file-based AppHosts (apphost.cs with #:sdk directive).
// Returns the directory containing the AppHost.
func DetectAppHost(projectDir string) (string, error) {
	var appHostDir string

	err := filepath.Walk(projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}

		// Skip common non-project dirs
		name := info.Name()
		if info.IsDir() && (name == "bin" || name == "obj" || name == ".git" || name == "node_modules" ||
			name == ".azure" || name == "aspire-output" || name == "azd-experiment" || name == ".aspire-init") {
			return filepath.SkipDir
		}

		// Check for file-based AppHost (apphost.cs with #:sdk directive)
		if strings.EqualFold(name, "apphost.cs") {
			//nolint:gosec // path comes from filepath.Walk within the user's project directory
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			content := string(data)
			if strings.Contains(content, "Aspire.AppHost.Sdk") || strings.Contains(content, "Aspire.Hosting.Sdk") {
				appHostDir = filepath.Dir(path)
				return filepath.SkipAll
			}
		}

		// Check for traditional .csproj-based AppHost
		if strings.HasSuffix(name, ".csproj") {
			//nolint:gosec // path comes from filepath.Walk within the user's project directory
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			content := string(data)
			if strings.Contains(content, "Aspire.AppHost.Sdk") || strings.Contains(content, "Aspire.Hosting.Sdk") {
				appHostDir = filepath.Dir(path)
				return filepath.SkipAll
			}
		}

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("error scanning for AppHost: %w", err)
	}

	if appHostDir == "" {
		return "", fmt.Errorf(
			"no Aspire AppHost found in %s.\n\n"+
				"An AppHost project (.csproj with Aspire.AppHost.Sdk) is required.\n"+
				"See: https://learn.microsoft.com/dotnet/aspire/fundamentals/app-host-overview",
			projectDir,
		)
	}

	return appHostDir, nil
}

// CheckAcaEnvironment verifies that the AppHost references the
// Aspire.Hosting.Azure.AppContainers package, which is required
// for Azure Container Apps Bicep generation.
// Supports both .csproj PackageReference and file-based #:package directives.
func CheckAcaEnvironment(appHostDir string) (bool, error) {
	entries, err := os.ReadDir(appHostDir)
	if err != nil {
		return false, err
	}

	for _, entry := range entries {
		name := entry.Name()
		// Check .csproj files
		if strings.HasSuffix(name, ".csproj") || strings.EqualFold(name, "apphost.cs") {
			//nolint:gosec // path within user's project directory
			data, err := os.ReadFile(filepath.Join(appHostDir, name))
			if err != nil {
				continue
			}

			content := string(data)
			if strings.Contains(content, "Aspire.Hosting.Azure.AppContainers") {
				return true, nil
			}
		}
	}

	return false, nil
}

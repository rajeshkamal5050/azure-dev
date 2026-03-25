// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"microsoft.aspire/internal/exterrors"
	"microsoft.aspire/internal/pkg/aspire"
	"microsoft.aspire/internal/pkg/generator"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

func newInitCommand(flags *rootFlagsDefinition) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize an Aspire project for Azure deployment with azd.",
		Long: `Detects an Aspire AppHost, generates azure.yaml and infrastructure 
artifacts so the project can be deployed with standard azd commands 
(azd provision, azd deploy, azd up).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := azdext.WithAccessToken(cmd.Context())

			setupDebugLogging(cmd.Flags())

			azdClient, err := azdext.NewAzdClient()
			if err != nil {
				return exterrors.Internal(exterrors.CodeAzdClientFailed, fmt.Sprintf("failed to create azd client: %s", err))
			}
			defer azdClient.Close()

			// Wait for debugger if AZD_EXT_DEBUG is set
			if err := azdext.WaitForDebugger(ctx, azdClient); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, azdext.ErrDebuggerAborted) {
					return nil
				}
				return fmt.Errorf("failed waiting for debugger: %w", err)
			}

			return runInit(ctx, azdClient, flags)
		},
	}
}

func runInit(ctx context.Context, azdClient *azdext.AzdClient, flags *rootFlagsDefinition) error {
	// ── Branding ──────────────────────────────────────────────────
	printBanner(os.Stdout)

	// ── Step 1: Ensure azd project + environment exist ───────────
	projectConfig, err := ensureProject(ctx, azdClient, flags)
	if err != nil {
		return err
	}

	projectPath := projectConfig.Path
	projectName := projectConfig.Name

	env, err := ensureEnvironment(ctx, azdClient, flags)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s %s\n", color.CyanString("Project:"), projectName)
	fmt.Printf("%s %s\n", color.CyanString("Path:"), projectPath)
	fmt.Printf("%s %s\n\n", color.CyanString("Environment:"), env.Name)

	// ── Step 1b: Ensure AZURE_RESOURCE_GROUP is set ──────────────
	// Aspire's main.bicep uses targetScope='subscription' and expects
	// resourceGroupName as a param. azd doesn't auto-set this.
	if err := ensureResourceGroup(ctx, azdClient, env.Name); err != nil {
		return err
	}

	// ── Step 2: Check Aspire CLI ─────────────────────────────────
	printStep("Checking Aspire CLI...")
	aspirePath, err := aspire.CheckCli()
	if err != nil {
		return exterrors.Dependency(
			exterrors.CodeAspireCliNotFound,
			"Aspire CLI not found. Install with: dotnet tool install -g Microsoft.Aspire.Cli",
			"Install with: dotnet tool install -g Microsoft.Aspire.Cli",
		)
	}
	fmt.Printf("  %s Aspire CLI found: %s\n", color.GreenString("✓"), aspirePath)

	// ── Step 3: Detect AppHost ───────────────────────────────────
	printStep("Detecting Aspire AppHost...")
	appHostDir, err := aspire.DetectAppHost(projectPath)
	if err != nil {
		return exterrors.Validation(
			exterrors.CodeAppHostNotFound,
			fmt.Sprintf("no AppHost found: %s", err),
			"Ensure your project has an AppHost .csproj with Aspire.AppHost.Sdk, "+
				"or an apphost.cs file with '#:sdk Aspire.AppHost.Sdk' directive.\n"+
				"Create one with: dotnet new aspire-apphost",
		)
	}
	fmt.Printf("  %s AppHost found: %s\n", color.GreenString("✓"), relPath(projectPath, appHostDir))

	// ── Step 4: Check ACA environment package ────────────────────
	printStep("Checking Azure Container Apps support...")
	hasAca, err := aspire.CheckAcaEnvironment(appHostDir)
	if err != nil {
		fmt.Printf("  %s Could not verify ACA support: %s\n", color.YellowString("!"), err)
	} else if !hasAca {
		fmt.Printf("\n  %s\n", color.YellowString("WARNING: Aspire.Hosting.Azure.AppContainers package not found in AppHost."))
		fmt.Println()
		fmt.Println("  This package is required for Azure Container Apps deployment.")
		fmt.Println("  Add it to your AppHost project:")
		fmt.Println()
		fmt.Printf("    %s\n", color.CyanString("dotnet add package Aspire.Hosting.Azure.AppContainers"))
		fmt.Println()
		fmt.Println("  Then add to your AppHost Program.cs:")
		fmt.Println()
		fmt.Printf("    %s\n", color.CyanString(`builder.AddAzureContainerAppEnvironment("aca");`))
		fmt.Println()

		if !flags.NoPrompt {
			confirmResp, err := azdClient.Prompt().Confirm(ctx, &azdext.ConfirmRequest{
				Options: &azdext.ConfirmOptions{
					Message:      "Continue anyway? (init will fail if package is missing during aspire publish)",
					DefaultValue: boolPtr(false),
				},
			})
			if err != nil {
				return err
			}
			if !*confirmResp.Value {
				return exterrors.User(exterrors.CodeCancelled, "cancelled by user")
			}
		}
	} else {
		fmt.Printf("  %s Azure Container Apps support detected\n", color.GreenString("✓"))
	}

	// ── Step 5: Run aspire do publish-manifest ───────────────────
	manifestDir := filepath.Join(projectPath, ".aspire-init", "manifest-output")
	os.MkdirAll(manifestDir, 0755)

	printStep("Running aspire do publish-manifest...")
	if err := aspire.RunPublishManifest(ctx, appHostDir, manifestDir); err != nil {
		// Error is already structured by FromAspireCommand — pass through directly
		return err
	}

	// Find the manifest file
	manifestPath := filepath.Join(manifestDir, "aspire-manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		return exterrors.Dependency(
			exterrors.CodeAspireManifestFailed,
			"aspire-manifest.json not found in output",
			"Check aspire do publish-manifest output above for errors.",
		)
	}
	fmt.Printf("  %s Manifest generated\n", color.GreenString("✓"))

	// ── Step 6: Run aspire publish (structured Bicep) ────────────
	infraDir := filepath.Join(projectPath, "infra")

	printStep("Running aspire publish (Bicep generation)...")
	if err := aspire.RunPublish(ctx, appHostDir, infraDir); err != nil {
		// Error is already structured by FromAspireCommand — pass through directly
		return err
	}
	fmt.Printf("  %s Infrastructure generated in infra/\n", color.GreenString("✓"))

	// ── Step 7: Parse manifest ───────────────────────────────────
	printStep("Parsing manifest...")
	manifest, err := aspire.ParseManifest(manifestPath)
	if err != nil {
		return exterrors.Validation(
			exterrors.CodeManifestParseError,
			fmt.Sprintf("failed to parse manifest: %s", err),
			"Check aspire-manifest.json format.",
		)
	}

	services := manifest.ExtractServices()
	if len(services) == 0 {
		return exterrors.Validation(
			exterrors.CodeNoDeployableServices,
			"no deployable services found in manifest",
			"Ensure your AppHost defines at least one project or container resource.",
		)
	}

	preCanned := manifest.ExtractPreCannedServices()

	fmt.Printf("  %s Found %d deployable service(s):\n", color.GreenString("✓"), len(services))
	for _, svc := range services {
		external := ""
		if svc.IsExternal {
			external = color.CyanString(" (external)")
		}
		fmt.Printf("    • %s%s\n", svc.Name, external)
	}
	if len(preCanned) > 0 {
		fmt.Printf("  %s Found %d pre-canned container(s) (provisioned via Bicep):\n", color.GreenString("✓"), len(preCanned))
		for _, pc := range preCanned {
			fmt.Printf("    • %s\n", pc.Name)
		}
	}

	// ── Step 8: Generate azure.yaml ──────────────────────────────
	printStep("Generating azure.yaml...")
	azureYamlPath := filepath.Join(projectPath, "azure.yaml")
	appHostRelPath := "./" + relPath(projectPath, appHostDir)

	// Always overwrite — azd init --minimal creates a stub that we replace
	if err := generator.GenerateAzureYaml(projectName, appHostRelPath, services, azureYamlPath); err != nil {
		return exterrors.Configuration(
			exterrors.CodeGenerateAzureYamlFailed,
			fmt.Sprintf("failed to generate azure.yaml: %s", err),
			"Check file permissions in project directory.",
		)
	}
	fmt.Printf("  %s azure.yaml\n", color.GreenString("+"))

	// ── Step 9: Generate main.parameters.json ────────────────────
	printStep("Generating main.parameters.json...")
	paramsPath := filepath.Join(infraDir, "main.parameters.json")
	preCannedParams := generator.PreCannedParameterEntries(preCanned)
	if err := generator.GenerateMainParameters(paramsPath, preCannedParams); err != nil {
		return exterrors.Configuration(
			exterrors.CodeGenerateParamsFailed,
			fmt.Sprintf("failed to generate main.parameters.json: %s", err),
			"Check file permissions in infra/ directory.",
		)
	}
	fmt.Printf("  %s infra/main.parameters.json\n", color.GreenString("+"))

	// ── Step 9a: Pre-generate passwords for pre-canned services ──
	// Passwords must exist in .env BEFORE provision so that:
	// 1. secretOrRandomPassword picks them up (won't generate new ones)
	// 2. readEnvironmentVariable in bicepparam files can resolve them during deploy
	// This ensures the same password is used for the container AND the services that connect to it.
	if len(preCannedParams) > 0 {
		printStep("Pre-generating service passwords...")
		for _, pc := range preCanned {
			for paramName, paramValue := range pc.Params {
				if !generator.IsSecretParam(paramName, paramValue) {
					continue
				}
				envVar := generator.SecretEnvVar(paramName)

				// Check if already set
				existing, _ := azdClient.Environment().GetValues(ctx, &azdext.GetEnvironmentRequest{
					Name: env.Name,
				})
				alreadySet := false
				if existing != nil {
					for _, kv := range existing.KeyValues {
						if kv.Key == envVar && kv.Value != "" {
							alreadySet = true
							break
						}
					}
				}

				if !alreadySet {
					pwd := generator.GeneratePassword()
					if _, err := azdClient.Environment().SetValue(ctx, &azdext.SetEnvRequest{
						EnvName: env.Name,
						Key:     envVar,
						Value:   pwd,
					}); err != nil {
						return exterrors.Internal(
							exterrors.CodeAzdClientFailed,
							fmt.Sprintf("failed to set %s: %s", envVar, err),
						)
					}
					fmt.Printf("  %s %s (auto-generated)\n", color.GreenString("✓"), envVar)
				}
			}
		}
	}

	// ── Step 9b: Wire pre-canned services into main.bicep ───────
	if len(preCanned) > 0 {
		printStep("Wiring pre-canned containers into main.bicep...")
		mainBicepPath := filepath.Join(infraDir, "main.bicep")
		if err := generator.PatchMainBicep(mainBicepPath, preCanned); err != nil {
			return exterrors.Configuration(
				exterrors.CodeGenerateParamsFailed,
				fmt.Sprintf("failed to patch main.bicep: %s", err),
				"Check file permissions in infra/ directory.",
			)
		}
		for _, pc := range preCanned {
			fmt.Printf("  %s infra/%s → main.bicep module\n", color.GreenString("+"), pc.Name)
		}
	}

	// ── Step 10: Generate .bicepparam per service ────────────────
	printStep("Generating .bicepparam files...")
	for _, svc := range services {
		if err := generator.GenerateBicepparam(svc, infraDir); err != nil {
			return exterrors.Configuration(
				exterrors.CodeGenerateBicepparamFailed,
				fmt.Sprintf("failed to generate %s.bicepparam: %s", svc.Name, err),
				"Check file permissions in infra/ directory.",
			)
		}
		fmt.Printf("  %s infra/%s.bicepparam\n", color.GreenString("+"), svc.Name)
	}

	// ── Step 11: Clean up temp files ─────────────────────────────
	os.RemoveAll(filepath.Join(projectPath, ".aspire-init"))

	// ── Summary ──────────────────────────────────────────────────
	insecureServices := manifest.InsecureInternalServices()
	printSummary(projectPath, services, infraDir, insecureServices)

	return nil
}

// printStep displays a step header.
func printStep(msg string) {
	fmt.Printf("\n%s\n", color.HiWhiteString(msg))
}

func printSummary(projectPath string, services []aspire.ServiceInfo, infraDir string, insecureServices []string) {
	fmt.Println()
	fmt.Println(strings.Repeat("─", 60))
	color.Green("  ✓ Aspire project initialized for azd!")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	color.HiWhite("Generated files:")
	fmt.Printf("  %s  azure.yaml\n", color.GreenString("+"))
	fmt.Printf("  %s  infra/main.parameters.json\n", color.GreenString("+"))

	// List Aspire-generated infra files
	filepath.WalkDir(infraDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(projectPath, path)
		if strings.HasSuffix(rel, ".bicepparam") {
			fmt.Printf("  %s  %s\n", color.GreenString("+"), rel)
		} else if strings.HasSuffix(rel, ".bicep") {
			fmt.Printf("  %s  %s  %s\n", color.CyanString("→"), rel, color.HiBlackString("(from Aspire)"))
		}
		return nil
	})

	fmt.Println()

	// Warn about internal HTTP services needing allowInsecure
	if len(insecureServices) > 0 {
		sort.Strings(insecureServices)
		fmt.Printf("  %s Add 'allowInsecure: true' to Bicep ingress for internal HTTP services:\n",
			color.YellowString("!"))
		fmt.Printf("      %s\n", strings.Join(insecureServices, ", "))
		fmt.Println()
	}

	color.HiWhite("Next steps:")
	fmt.Printf("  1. %s          %s\n", color.CyanString("azd provision"), color.HiBlackString("# Deploy infrastructure"))
	fmt.Printf("  2. %s             %s\n", color.CyanString("azd deploy"), color.HiBlackString("# Build images + deploy services"))
	fmt.Println()
	fmt.Printf("  Or run %s to do both in one step.\n", color.CyanString("azd up"))
	fmt.Println()
}

func relPath(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return rel
}

func boolPtr(b bool) *bool {
	return &b
}

// ensureProject checks for an existing azd project. If none exists,
// it runs `azd init --minimal` via the Workflow API to create one.
func ensureProject(ctx context.Context, azdClient *azdext.AzdClient, flags *rootFlagsDefinition) (*azdext.ProjectConfig, error) {
	projectResp, err := azdClient.Project().Get(ctx, &azdext.EmptyRequest{})
	if err != nil {
		fmt.Println("No azd project found — initializing one now...")

		initArgs := []string{"init", "--minimal", "--no-prompt"}

		workflow := &azdext.Workflow{
			Name: "init",
			Steps: []*azdext.WorkflowStep{
				{Command: &azdext.WorkflowCommand{Args: initArgs}},
			},
		}

		if _, err := azdClient.Workflow().Run(ctx, &azdext.RunWorkflowRequest{
			Workflow: workflow,
		}); err != nil {
			return nil, exterrors.Dependency(
				exterrors.CodeProjectNotFound,
				fmt.Sprintf("failed to initialize azd project: %s", err),
				"Run 'azd init --minimal' manually, then retry.",
			)
		}

		projectResp, err = azdClient.Project().Get(ctx, &azdext.EmptyRequest{})
		if err != nil {
			return nil, exterrors.Dependency(
				exterrors.CodeProjectNotFound,
				"project not found after initialization",
				"Run 'azd init --minimal' manually, then retry.",
			)
		}

		fmt.Println()
	}

	return projectResp.Project, nil
}

// ensureEnvironment checks for an existing azd environment. If none exists,
// it creates one via the Workflow API using the project directory name.
func ensureEnvironment(ctx context.Context, azdClient *azdext.AzdClient, flags *rootFlagsDefinition) (*azdext.Environment, error) {
	envResp, err := azdClient.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err == nil && envResp.Environment != nil {
		return envResp.Environment, nil
	}

	fmt.Println("No azd environment found — creating one now...")

	envArgs := []string{"env", "new"}

	// Use current directory name as default environment name
	cwd, cwdErr := os.Getwd()
	if cwdErr == nil {
		envName := sanitizeEnvName(filepath.Base(cwd)) + "-dev"
		envArgs = append(envArgs, envName)
	}

	workflow := &azdext.Workflow{
		Name: "env new",
		Steps: []*azdext.WorkflowStep{
			{Command: &azdext.WorkflowCommand{Args: envArgs}},
		},
	}

	if _, err := azdClient.Workflow().Run(ctx, &azdext.RunWorkflowRequest{
		Workflow: workflow,
	}); err != nil {
		return nil, exterrors.Dependency(
			exterrors.CodeEnvironmentNotFound,
			fmt.Sprintf("failed to create azd environment: %s", err),
			"Run 'azd env new' manually, then retry.",
		)
	}

	envResp, err = azdClient.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err != nil || envResp.Environment == nil {
		return nil, exterrors.Dependency(
			exterrors.CodeEnvironmentNotFound,
			"environment not found after creation",
			"Run 'azd env new' manually, then retry.",
		)
	}

	return envResp.Environment, nil
}

// sanitizeEnvName removes characters not allowed in azd environment names.
func sanitizeEnvName(name string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// ensureResourceGroup checks if AZURE_RESOURCE_GROUP is set in the azd
// environment. If not, sets it to "rg-{envName}" to match Aspire convention.
// Also ensures AZURE_PRINCIPAL_ID is set (needed by main.bicep).
func ensureResourceGroup(ctx context.Context, azdClient *azdext.AzdClient, envName string) error {
	envValues, err := azdClient.Environment().GetValues(ctx, &azdext.GetEnvironmentRequest{
		Name: envName,
	})
	if err != nil {
		return exterrors.FromEnvGet(err, "read environment values for resource group check")
	}

	envMap := make(map[string]string)
	for _, kv := range envValues.KeyValues {
		envMap[kv.Key] = kv.Value
	}

	// Set AZURE_RESOURCE_GROUP if missing
	if envMap["AZURE_RESOURCE_GROUP"] == "" {
		rgName := "rg-" + envName
		if _, err := azdClient.Environment().SetValue(ctx, &azdext.SetEnvRequest{
			EnvName: envName,
			Key:     "AZURE_RESOURCE_GROUP",
			Value:   rgName,
		}); err != nil {
			return exterrors.FromEnvSet(err, "AZURE_RESOURCE_GROUP")
		}
		fmt.Printf("  Set %s=%s\n", color.CyanString("AZURE_RESOURCE_GROUP"), rgName)
	}

	return nil
}

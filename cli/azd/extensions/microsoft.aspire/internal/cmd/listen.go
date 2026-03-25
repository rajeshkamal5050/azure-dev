// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"strings"

	"microsoft.aspire/internal/exterrors"
	"microsoft.aspire/internal/pkg/aspire"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/spf13/cobra"
)

func newListenCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "listen",
		Short:  "Starts the extension and listens for events.",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := azdext.WithAccessToken(cmd.Context())

			setupDebugLogging(cmd.Flags())

			azdClient, err := azdext.NewAzdClient()
			if err != nil {
				return exterrors.Internal(
					exterrors.CodeAzdClientFailed,
					fmt.Sprintf("failed to create azd client: %s", err),
				)
			}
			defer azdClient.Close()

			host := azdext.NewExtensionHost(azdClient).
				WithProjectEventHandler("postprovision", func(ctx context.Context, args *azdext.ProjectEventArgs) error {
					return postprovisionHandler(ctx, azdClient, args)
				})

			if err := host.Run(ctx); err != nil {
				return exterrors.Internal(
					exterrors.CodeAzdClientFailed,
					fmt.Sprintf("extension host stopped unexpectedly: %s", err),
				)
			}

			return nil
		},
	}
}

// postprovisionHandler runs after `azd provision` creates infrastructure.
// It executes `aspire do push` to build and push container images to ACR,
// then sets SERVICE_<NAME>_IMAGE_NAME env vars so `azd deploy` can use them.
// This runs at postprovision (not predeploy) because azd resolves image URLs
// at config load time — env vars must be set BEFORE `azd deploy` starts.
func postprovisionHandler(ctx context.Context, azdClient *azdext.AzdClient, args *azdext.ProjectEventArgs) error {
	projectPath := args.Project.Path

	// Guard: only run for projects initialized by this extension.
	// Our `azd aspire init` writes an `aspire:` section into azure.yaml which
	// lands in AdditionalProperties. Old built-in Aspire flow doesn't have this.
	props := args.Project.GetAdditionalProperties()
	if props == nil || props.GetFields()["aspire"] == nil {
		return nil
	}

	// Get current environment
	currentEnv, err := azdClient.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err != nil {
		return exterrors.FromEnvGet(err, "get current environment")
	}

	envName := currentEnv.Environment.Name

	// Read required env vars for aspire do push
	envValues, err := azdClient.Environment().GetValues(ctx, &azdext.GetEnvironmentRequest{
		Name: envName,
	})
	if err != nil {
		return exterrors.FromEnvGet(err, "read environment values")
	}

	envMap := make(map[string]string)
	for _, kv := range envValues.KeyValues {
		envMap[kv.Key] = kv.Value
	}

	// Validate required Azure env vars — each one tells the user exactly what's missing
	resourceGroup := envMap["AZURE_RESOURCE_GROUP"]
	subscriptionId := envMap["AZURE_SUBSCRIPTION_ID"]
	location := envMap["AZURE_LOCATION"]

	var missing []string
	if resourceGroup == "" {
		missing = append(missing, "AZURE_RESOURCE_GROUP")
	}
	if subscriptionId == "" {
		missing = append(missing, "AZURE_SUBSCRIPTION_ID")
	}
	if location == "" {
		missing = append(missing, "AZURE_LOCATION")
	}
	if len(missing) > 0 {
		return exterrors.Validation(
			exterrors.CodeMissingAzureEnvVars,
			fmt.Sprintf("missing required environment variables: %s. Run 'azd provision' first, or set them with 'azd env set <VAR> <value>'", strings.Join(missing, ", ")),
			fmt.Sprintf("Set them with:\n%s\n\nOr run 'azd provision' first — it sets these automatically.",
				buildEnvSetHints(missing)),
		)
	}

	// Detect AppHost
	appHostPath, err := aspire.DetectAppHost(projectPath)
	if err != nil {
		return exterrors.Validation(
			exterrors.CodeAppHostNotFound,
			fmt.Sprintf("no Aspire AppHost found in %s", projectPath),
			"Ensure your project has an AppHost .csproj with Aspire.AppHost.Sdk, "+
				"or an apphost.cs file with '#:sdk Aspire.AppHost.Sdk' directive.",
		)
	}

	// Run aspire do push — captures output and parses actual image names
	fmt.Println("Running aspire do push...")
	pushEnv := map[string]string{
		"Azure__ResourceGroup":  resourceGroup,
		"Azure__SubscriptionId": subscriptionId,
		"Azure__Location":       location,
	}

	pushResult, err := aspire.RunDoPush(ctx, appHostPath, pushEnv)
	if err != nil {
		// Error is already structured by FromAspireCommand with actionable suggestion
		return err
	}

	// Set SERVICE_*_IMAGE_NAME env vars with actual pushed image references.
	// These are persisted to .env and read by `azd deploy` when it starts.
	for svcName, imageRef := range pushResult.Images {
		envKey := fmt.Sprintf("SERVICE_%s_IMAGE_NAME", aspire.ToEnvVarName(svcName))
		if _, err := azdClient.Environment().SetValue(ctx, &azdext.SetEnvRequest{
			EnvName: envName,
			Key:     envKey,
			Value:   imageRef,
		}); err != nil {
			return exterrors.FromEnvSet(err, envKey)
		}

		fmt.Printf("  ✓ %s → %s\n", svcName, imageRef)
	}

	// Set AZURE_CONTAINER_REGISTRY_ENDPOINT so azd can do docker login to ACR.
	// Aspire outputs this as "aca_AZURE_CONTAINER_REGISTRY_ENDPOINT" but azd
	// core expects "AZURE_CONTAINER_REGISTRY_ENDPOINT" (without prefix).
	acrEndpoint := envMap["aca_AZURE_CONTAINER_REGISTRY_ENDPOINT"]
	if acrEndpoint != "" {
		if _, err := azdClient.Environment().SetValue(ctx, &azdext.SetEnvRequest{
			EnvName: envName,
			Key:     "AZURE_CONTAINER_REGISTRY_ENDPOINT",
			Value:   acrEndpoint,
		}); err != nil {
			return exterrors.FromEnvSet(err, "AZURE_CONTAINER_REGISTRY_ENDPOINT")
		}
		fmt.Printf("  ✓ Set AZURE_CONTAINER_REGISTRY_ENDPOINT=%s\n", acrEndpoint)
	}

	fmt.Println("  ✓ aspire do push completed successfully.")
	return nil
}

// buildEnvSetHints generates copy-pasteable `azd env set` commands for missing vars.
func buildEnvSetHints(missing []string) string {
	var lines []string
	for _, v := range missing {
		lines = append(lines, fmt.Sprintf("  azd env set %s <value>", v))
	}
	return strings.Join(lines, "\n")
}

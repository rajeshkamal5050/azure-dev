// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package generator

import (
	"fmt"
	"os"
	"strings"

	"microsoft.aspire/internal/pkg/aspire"
)

// GenerateBicepparam creates a .bicepparam file for a service.
// This is the "param bridge" that maps azd env vars to the Aspire
// service Bicep params using Bicep's readEnvironmentVariable() function.
//
// Manifest expression translation:
//
//	"{aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID}"
//	  → Bicep param: aca_outputs_azure_container_apps_environment_id
//	  → azd env var: aca_AZURE_CONTAINER_APPS_ENVIRONMENT_ID
//	  → bicepparam:  param aca_outputs_... = readEnvironmentVariable('aca_AZURE_CONTAINER_APPS_ENVIRONMENT_ID')
//
//	"{apiservice.containerImage}"
//	  → param apiservice_containerimage = readEnvironmentVariable('SERVICE_APISERVICE_IMAGE_NAME')
//
//	"{apiservice.containerPort}"
//	  → param apiservice_containerport = '8080' (literal)
func GenerateBicepparam(svc aspire.ServiceInfo, infraDir string) error {
	var sb strings.Builder

	// using directive points to the Bicep module
	bicepRelPath := svc.BicepPath
	// The manifest uses flat paths like "apiservice-containerapp.module.bicep"
	// but aspire publish puts them in subdirectories like "apiservice/apiservice.bicep"
	// We use the structured path from aspire publish
	modulePath := fmt.Sprintf("./%s/%s.bicep", svc.Name, svc.Name)
	sb.WriteString(fmt.Sprintf("using '%s'\n\n", modulePath))

	for paramName, paramValue := range svc.Params {
		line := resolveParamLine(paramName, paramValue, svc.Name)
		sb.WriteString(line + "\n")
	}

	outputPath := fmt.Sprintf("%s/%s.bicepparam", infraDir, svc.Name)

	//nolint:gosec // outputPath is within the user's project directory
	if err := os.WriteFile(outputPath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", outputPath, err)
	}

	_ = bicepRelPath // used for reference; structured path is computed above
	return nil
}

// resolveParamLine translates a manifest param expression to a bicepparam line.
func resolveParamLine(paramName string, paramValue string, serviceName string) string {
	// Container image → read from SERVICE_<NAME>_IMAGE_NAME env var
	if strings.HasSuffix(paramValue, ".containerImage}") {
		envVar := fmt.Sprintf("SERVICE_%s_IMAGE_NAME", aspire.ToEnvVarName(serviceName))
		return fmt.Sprintf("param %s = readEnvironmentVariable('%s')", paramName, envVar)
	}

	// Container port → use literal value
	if strings.HasSuffix(paramValue, ".containerPort}") {
		return fmt.Sprintf("param %s = '8080'", paramName)
	}

	if strings.HasPrefix(paramValue, "{") && strings.HasSuffix(paramValue, "}") {
		expr := paramValue[1 : len(paramValue)-1] // strip braces
		parts := strings.SplitN(expr, ".", 3)

		// Infrastructure output references like "{aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID}"
		// These map to azd env vars with the pattern: <resource>_<OUTPUT_NAME>
		if len(parts) == 3 && parts[1] == "outputs" {
			envVar := fmt.Sprintf("%s_%s", parts[0], parts[2])
			return fmt.Sprintf("param %s = readEnvironmentVariable('%s')", paramName, envVar)
		}

		// Parameter resource references like "{postgres-password.value}"
		// These are parameter.v0 resources in the manifest (typically secrets).
		// Map to env var: POSTGRES_PASSWORD (resource name uppercased, hyphens → underscores)
		if len(parts) == 2 && parts[1] == "value" {
			envVar := strings.ToUpper(strings.ReplaceAll(parts[0], "-", "_"))
			return fmt.Sprintf("param %s = readEnvironmentVariable('%s')", paramName, envVar)
		}
	}

	// Fallback: treat as literal
	return fmt.Sprintf("param %s = '%s'", paramName, paramValue)
}

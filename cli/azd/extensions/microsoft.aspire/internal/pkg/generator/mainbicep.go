// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package generator

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"

	"microsoft.aspire/internal/pkg/aspire"
)

// PatchMainBicep appends module references for pre-canned container services
// to the Aspire-generated main.bicep. These services (e.g., Redis, PostgreSQL,
// RabbitMQ) use pre-built images and must be provisioned during `azd provision`
// rather than deployed via `azd deploy`.
//
// For each pre-canned service, this function:
//  1. Adds @secure() param declarations for any secret params (passwords)
//  2. Adds a module block referencing the service's Bicep file
//  3. Passes through aca outputs and secret params
func PatchMainBicep(mainBicepPath string, preCanned []aspire.PreCannedService) error {
	if len(preCanned) == 0 {
		return nil
	}

	existing, err := os.ReadFile(mainBicepPath)
	if err != nil {
		return fmt.Errorf("failed to read main.bicep: %w", err)
	}

	var sb strings.Builder
	sb.Write(existing)
	sb.WriteString("\n// ── Pre-canned container services (deployed during provision) ──\n")

	for _, svc := range preCanned {
		// Collect secret params (passwords) that need @secure() declarations
		var secretParams []secretParam
		for paramName, paramValue := range svc.Params {
			if IsSecretParam(paramName, paramValue) {
				secretParams = append(secretParams, secretParam{
					bicepName: paramName,
					envVar:    SecretEnvVar(paramName),
				})
			}
		}

		// Emit @secure() param declarations
		for _, sp := range secretParams {
			sb.WriteString(fmt.Sprintf("\n@secure()\nparam %s string\n", sp.bicepName))
		}

		// Bicep module name — replace hyphens with underscores for valid identifiers
		moduleName := strings.ReplaceAll(svc.Name, "-", "_")

		sb.WriteString(fmt.Sprintf("\nmodule %s '%s/%s.bicep' = {\n", moduleName, svc.Name, svc.Name))
		sb.WriteString(fmt.Sprintf("  name: '%s'\n", svc.Name))
		sb.WriteString("  scope: rg\n")
		sb.WriteString("  params: {\n")
		sb.WriteString("    location: location\n")

		// Pass through all params
		for paramName, paramValue := range svc.Params {
			if IsSecretParam(paramName, paramValue) {
				// Secret param — pass the main.bicep param through
				sb.WriteString(fmt.Sprintf("    %s: %s\n", paramName, paramName))
			} else if isAcaOutputRef(paramValue) {
				// ACA output reference — resolve to aca module output
				bicepExpr := resolveAcaOutput(paramValue)
				sb.WriteString(fmt.Sprintf("    %s: %s\n", paramName, bicepExpr))
			}
			// Skip containerPort and other literal params — they have defaults
		}

		sb.WriteString("  }\n")
		sb.WriteString("}\n")
	}

	if err := os.WriteFile(mainBicepPath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write main.bicep: %w", err)
	}

	return nil
}

// PreCannedParameterEntries adds secret parameter entries for pre-canned services
// to the parameters map used by GenerateMainParameters.
// Passwords are pre-generated during init (Step 9a) and stored in .env,
// so we reference them directly via ${ENV_VAR} expansion.
func PreCannedParameterEntries(preCanned []aspire.PreCannedService) map[string]ParameterMapping {
	params := make(map[string]ParameterMapping)

	for _, svc := range preCanned {
		for paramName, paramValue := range svc.Params {
			if IsSecretParam(paramName, paramValue) {
				envVar := SecretEnvVar(paramName)
				params[paramName] = ParameterMapping{
					Value: fmt.Sprintf("${%s}", envVar),
				}
			}
		}
	}

	return params
}

type secretParam struct {
	bicepName string
	envVar    string
}

// IsSecretParam detects password/secret params from the manifest expression.
// Manifest expressions like "{postgres-password.value}" or "{redis-password.value}"
// indicate secrets that need @secure() in Bicep and secretOrRandomPassword in params.
func IsSecretParam(paramName string, paramValue string) bool {
	lower := strings.ToLower(paramName)
	return strings.Contains(lower, "password") || strings.Contains(lower, "secret")
}

// SecretEnvVar converts a Bicep param name like "postgres_password_value"
// to an azd env var name like "POSTGRES_PASSWORD".
func SecretEnvVar(paramName string) string {
	// Remove common suffixes like "_value"
	name := strings.TrimSuffix(paramName, "_value")
	return strings.ToUpper(name)
}

// GeneratePassword creates a random password suitable for service credentials.
func GeneratePassword() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = charset[randByte()%byte(len(charset))]
	}
	return string(b)
}

func randByte() byte {
	b := make([]byte, 1)
	_, _ = rand.Read(b)
	return b[0]
}

// isAcaOutputRef checks if a manifest param value references an ACA output.
// e.g., "{aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID}"
func isAcaOutputRef(paramValue string) bool {
	return strings.HasPrefix(paramValue, "{") &&
		strings.Contains(paramValue, ".outputs.") &&
		strings.HasSuffix(paramValue, "}")
}

// resolveAcaOutput converts a manifest expression to a Bicep module output reference.
// e.g., "{aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID}"
//
//	→ "aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID"
//
// Handles hyphenated module names (e.g., "aca-acr" → "aca_acr") since Bicep
// identifiers cannot contain hyphens.
func resolveAcaOutput(paramValue string) string {
	expr := paramValue[1 : len(paramValue)-1] // strip braces
	parts := strings.SplitN(expr, ".", 3)
	if len(parts) == 3 {
		moduleName := strings.ReplaceAll(parts[0], "-", "_")
		return fmt.Sprintf("%s.%s.%s", moduleName, parts[1], parts[2])
	}
	return expr
}

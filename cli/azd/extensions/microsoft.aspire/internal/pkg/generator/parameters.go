// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package generator

import (
	"encoding/json"
	"fmt"
	"os"
)

// ParameterMapping maps an azd env var to a main.bicep parameter.
type ParameterMapping struct {
	Value string `json:"value"`
}

// GenerateMainParameters creates main.parameters.json that maps
// azd environment variables to the Aspire main.bicep parameters.
//
// Aspire's main.bicep declares params: location, principalId, resourceGroupName.
// azd provides: ${AZURE_LOCATION}, ${AZURE_PRINCIPAL_ID}, ${AZURE_RESOURCE_GROUP}.
// Pre-canned services add secret params (passwords) via secretOrRandomPassword.
func GenerateMainParameters(outputPath string, extraParams map[string]ParameterMapping) error {
	params := map[string]ParameterMapping{
		"location": {
			Value: "${AZURE_LOCATION}",
		},
		"principalId": {
			Value: "${AZURE_PRINCIPAL_ID}",
		},
		"resourceGroupName": {
			Value: "${AZURE_RESOURCE_GROUP}",
		},
	}

	// Merge in pre-canned service params (passwords, etc.)
	for k, v := range extraParams {
		params[k] = v
	}

	wrapper := map[string]any{
		"$schema":        "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
		"contentVersion": "1.0.0.0",
		"parameters":     params,
	}

	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal parameters: %w", err)
	}

	//nolint:gosec // outputPath is within the user's project directory
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write main.parameters.json: %w", err)
	}

	return nil
}

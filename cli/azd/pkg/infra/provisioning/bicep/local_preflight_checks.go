// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package bicep

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	"github.com/azure/azure-dev/cli/azd/pkg/ai"
	"github.com/azure/azure-dev/cli/azd/pkg/azapi"
	"github.com/azure/azure-dev/cli/azd/pkg/azure"
	"github.com/azure/azure-dev/cli/azd/pkg/keyvault"
)

// locationGetterFn retrieves the list of Azure locations where a resource type is available.
// Returns display names (e.g. "East US 2") as returned by the ARM Providers API.
// Returns nil if the resource type is unknown.
type locationGetterFn func(ctx context.Context, subscriptionID string, resourceType string) ([]string, error)

// displayToCanonicalMapFn builds a mapping from lowercase display name to canonical name
// (e.g. "east us 2" → "eastus2") by querying the Subscriptions ListLocations API.
type displayToCanonicalMapFn func(
	ctx context.Context, subscriptionID string,
) (map[string]string, error)

// isARMExpression reports whether a string looks like an unresolved ARM template expression
// (e.g. "[parameters('location')]").
func isARMExpression(s string) bool {
	return strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")
}

// resourceTypeDisplayName returns a human-friendly display name for an Azure resource type
// (e.g. "Azure AI Services" for "Microsoft.CognitiveServices/accounts").
// Falls back to the raw resource type string when no mapping exists.
func resourceTypeDisplayName(resourceType string) string {
	if name := azapi.GetResourceTypeDisplayName(azapi.AzureResourceType(resourceType)); name != "" {
		return name
	}
	return resourceType
}

// reverseMapToEnvVars finds all environment variable names that produced a given resolved value
// by matching the value against ARM parameters and looking up the env mapping.
// Returns nil if the value is hardcoded (no env var mapping found).
func reverseMapToEnvVars(
	value any,
	armParams azure.ArmParameters,
	envMapping map[string][]string,
) []string {
	if envMapping == nil {
		return nil
	}
	valStr := fmt.Sprintf("%v", value)
	var result []string
	for paramName, param := range armParams {
		if fmt.Sprintf("%v", param.Value) == valStr {
			if envVars, ok := envMapping[paramName]; ok && len(envVars) == 1 {
				result = append(result, envVars[0])
			}
		}
	}
	return result
}

// locationMismatch records a resource type that is not available in the chosen location.
type locationMismatch struct {
	resourceType string
	location     string
}

// ModelDeploymentFailure holds structured information about a failed AI model deployment check.
// It carries the catalog data already fetched during validation so the interactive fix flow
// can present options without additional API calls.
type ModelDeploymentFailure struct {
	Location       string             // Parent account location
	ModelName      string             // Requested model name from template
	ModelVersion   string             // Requested model version
	SkuName        string             // Requested SKU name
	SkuCapacity    int                // Requested capacity
	DeploymentName string             // Deployment name from snapshot (e.g. "gpt-4o-mini")
	FailureType    string             // Error code, e.g. "model_not_available"
	Models         []ai.AiModel       // Full model catalog for this location
	FoundModel     *ai.AiModel        // Matched model (nil if model not found)
	FoundVersion   *ai.AiModelVersion // Matched version (nil if version not found)
	MatchedSku     *ai.AiModelSku     // Matched SKU (nil if SKU not found)
	Usages         []ai.AiModelUsage  // Quota/usage data for this location (may be nil)
}

// checkResourceLocationAvailability validates that each resource in the snapshot
// is being deployed to a location where its resource type is available.
//
// Resources without a location (e.g. subscription-scope resources, child resources) and
// resources with unresolved ARM expressions are silently skipped.
func checkResourceLocationAvailability(
	ctx context.Context,
	resources []armTemplateResource,
	subscriptionID string,
	getLocations locationGetterFn,
	resolveLocations displayToCanonicalMapFn,
) (*PreflightCheckResult, error) {
	if len(resources) == 0 {
		return nil, nil
	}

	// Collect unique resource types that need location lookups.
	uniqueTypes := map[string]string{} // lowercase → original casing
	for _, r := range resources {
		loc := strings.TrimSpace(r.Location)
		if loc == "" || isARMExpression(loc) {
			continue
		}
		typeLower := strings.ToLower(r.Type)
		if _, exists := uniqueTypes[typeLower]; !exists {
			uniqueTypes[typeLower] = r.Type
		}
	}

	if len(uniqueTypes) == 0 {
		return nil, nil
	}

	// Fetch locations for all unique types in parallel.
	type locResult struct {
		typeLower string
		locations []string
		err       error
	}
	resultsCh := make(chan locResult, len(uniqueTypes))
	var wg sync.WaitGroup
	for _, origType := range uniqueTypes {
		wg.Add(1)
		go func(rt string) {
			defer wg.Done()
			locs, err := getLocations(ctx, subscriptionID, rt)
			resultsCh <- locResult{
				typeLower: strings.ToLower(rt),
				locations: locs,
				err:       err,
			}
		}(origType)
	}
	wg.Wait()
	close(resultsCh)

	// Build cache from parallel results.
	locationCache := map[string][]string{}
	for res := range resultsCh {
		if res.err != nil {
			log.Printf(
				"local preflight: could not fetch locations for %s, skipping: %v",
				uniqueTypes[res.typeLower], res.err)
			continue
		}
		locationCache[res.typeLower] = res.locations
	}

	// Check every resource (not just unique types) against its own location.
	// Normalize by stripping spaces for comparison since the ARM Providers API returns
	// display names ("Japan East") while Bicep snapshots use canonical names ("japaneast").
	normalize := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, " ", ""))
	}

	var mismatches []locationMismatch
	seenMismatch := map[string]bool{} // dedup output by "type|location"
	for _, r := range resources {
		loc := strings.TrimSpace(r.Location)
		if loc == "" || isARMExpression(loc) {
			continue
		}

		typeLower := strings.ToLower(r.Type)
		allowedLocations, ok := locationCache[typeLower]
		if !ok || allowedLocations == nil {
			continue
		}

		locNorm := normalize(loc)
		found := false
		for _, allowed := range allowedLocations {
			if normalize(allowed) == locNorm {
				found = true
				break
			}
		}
		if !found {
			key := typeLower + "|" + locNorm
			if !seenMismatch[key] {
				seenMismatch[key] = true
				mismatches = append(mismatches, locationMismatch{
					resourceType: r.Type, location: loc,
				})
			}
		}
	}

	if len(mismatches) == 0 {
		// Build a list of verified resource display names for the success message.
		// Only include types that have a known display name — skip raw type strings.
		var verified []string
		seenVerified := map[string]bool{}
		for typeLower := range locationCache {
			displayName := azapi.GetResourceTypeDisplayName(azapi.AzureResourceType(uniqueTypes[typeLower]))
			if displayName != "" && !seenVerified[displayName] {
				seenVerified[displayName] = true
				verified = append(verified, displayName)
			}
		}
		slices.Sort(verified)
		var msg string
		if len(verified) > 0 {
			msg = fmt.Sprintf("Resource location verified for %s.", strings.Join(verified, ", "))
		} else {
			msg = "Resource location verified."
		}
		return &PreflightCheckResult{
			Severity: PreflightCheckSuccess,
			Message:  msg,
		}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"%d resource type(s) are not available in the configured location:\n", len(mismatches)))
	for _, m := range mismatches {
		displayName := resourceTypeDisplayName(m.resourceType)
		sb.WriteString(fmt.Sprintf("  - %s (%s) is not available in '%s'\n",
			displayName, m.resourceType, m.location))
	}

	// Compute the intersection of valid locations across all failed types to suggest alternatives.
	// Build a display-name → canonical-name map so suggestions use the correct azd format.
	var displayMap map[string]string
	if resolveLocations != nil {
		var err error
		displayMap, err = resolveLocations(ctx, subscriptionID)
		if err != nil {
			log.Printf("local preflight: could not resolve location names: %v", err)
		}
	}
	suggested := suggestCommonLocations(mismatches, locationCache, displayMap)
	if len(suggested) > 0 {
		sb.WriteString(fmt.Sprintf(
			"Regions that support all %d resource types: %s\n",
			len(mismatches), strings.Join(suggested, ", ")))
	}
	sb.WriteString("Change the deployment location with 'azd env set AZURE_LOCATION <region>'.")

	return &PreflightCheckResult{
		Severity: PreflightCheckError,
		Code:     "local_preflight.location_not_available",
		Message:  sb.String(),
	}, nil
}

// suggestCommonLocations finds locations that support all failed resource types and returns
// up to 5 canonical location names (e.g. "eastus2"). The displayMap maps lowercase display
// names to canonical names; if nil, falls back to stripping spaces.
func suggestCommonLocations(
	mismatches []locationMismatch,
	locationCache map[string][]string,
	displayMap map[string]string,
) []string {
	if len(mismatches) == 0 {
		return nil
	}

	// Convert a display name to canonical form using the dynamic map when available.
	toCanonical := func(displayName string) string {
		lower := strings.ToLower(displayName)
		if displayMap != nil {
			if canonical, ok := displayMap[lower]; ok {
				return canonical
			}
		}
		// Fallback: strip spaces (reliable convention but not API-sourced).
		return strings.ReplaceAll(lower, " ", "")
	}

	// Start with locations from the first failed type, intersect with the rest.
	firstType := strings.ToLower(mismatches[0].resourceType)
	candidates := map[string]bool{}
	for _, loc := range locationCache[firstType] {
		candidates[toCanonical(loc)] = true
	}

	for i := 1; i < len(mismatches); i++ {
		typeLower := strings.ToLower(mismatches[i].resourceType)
		allowed := map[string]bool{}
		for _, loc := range locationCache[typeLower] {
			allowed[toCanonical(loc)] = true
		}
		for loc := range candidates {
			if !allowed[loc] {
				delete(candidates, loc)
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Prefer well-known regions by ordering popular ones first.
	preferred := []string{
		"eastus", "eastus2", "westus2", "westus3",
		"centralus", "northeurope", "westeurope",
		"southeastasia", "japaneast", "australiaeast",
	}
	var result []string
	for _, p := range preferred {
		if candidates[p] {
			result = append(result, p)
			if len(result) >= 5 {
				return result
			}
		}
	}

	// Fill remaining from any other candidates.
	for loc := range candidates {
		found := false
		for _, r := range result {
			if r == loc {
				found = true
				break
			}
		}
		if !found {
			result = append(result, loc)
			if len(result) >= 5 {
				return result
			}
		}
	}

	return result
}

// modelListFn retrieves the AI model catalog for a subscription and set of locations.
type modelListFn func(ctx context.Context, subscriptionId string, locations []string) ([]ai.AiModel, error)

// usageListFn retrieves quota/usage data for a subscription and location.
type usageListFn func(ctx context.Context, subscriptionId string, location string) ([]ai.AiModelUsage, error)

// checkModelDeploymentAvailability validates that each CognitiveServices model deployment
// in the snapshot references a model name, version, and SKU that exist in the model catalog.
// When listUsages is non-nil, also checks that remaining quota is sufficient for the requested capacity.
//
// The location for each deployment is derived from its parent CognitiveServices/accounts
// resource by splitting the deployment name (e.g. "accountName/deploymentName") and finding
// the matching account in the resource list.
func checkModelDeploymentAvailability(
	ctx context.Context,
	resources []armTemplateResource,
	subscriptionID string,
	listModels modelListFn,
	listUsages usageListFn,
) (*PreflightCheckResult, []ModelDeploymentFailure, error) {
	// Build a map of account name → location from CognitiveServices/accounts resources.
	accountLocations := map[string]string{}
	for _, r := range resources {
		if strings.EqualFold(r.Type, "Microsoft.CognitiveServices/accounts") {
			accountLocations[strings.ToLower(r.Name)] = r.Location
		}
	}

	// Collect deployment resources (deduplicate by name).
	type deploymentInfo struct {
		resource     armTemplateResource
		props        armCognitiveDeploymentProperties
		skuName      string
		skuCapacity  int
		parentAcctLo string // location from parent account
		deployName   string // deployment name (second part of account/deployment)
	}
	var deployments []deploymentInfo
	seenDeployments := map[string]bool{}

	for _, r := range resources {
		if !strings.EqualFold(r.Type, "Microsoft.CognitiveServices/accounts/deployments") {
			continue
		}
		nameLower := strings.ToLower(r.Name)
		if seenDeployments[nameLower] {
			continue
		}
		seenDeployments[nameLower] = true
		if len(r.Properties) == 0 {
			continue
		}

		var props armCognitiveDeploymentProperties
		if err := json.Unmarshal(r.Properties, &props); err != nil || props.Model.Name == "" {
			continue
		}

		// Derive location from parent account.
		parts := strings.SplitN(r.Name, "/", 2)
		if len(parts) < 2 {
			continue
		}
		acctName := strings.ToLower(parts[0])
		location, ok := accountLocations[acctName]
		if !ok || location == "" {
			log.Printf("local preflight: no parent account found for deployment %s, skipping", r.Name)
			continue
		}

		skuName := ""
		skuCapacity := 0
		if sku, ok := r.SKU.Value(); ok {
			skuName = sku.Name
			if sku.Capacity != nil {
				skuCapacity = *sku.Capacity
			}
		}

		deployments = append(deployments, deploymentInfo{
			resource:     r,
			props:        props,
			skuName:      skuName,
			skuCapacity:  skuCapacity,
			parentAcctLo: location,
			deployName:   parts[1],
		})
	}

	if len(deployments) == 0 {
		return nil, nil, nil
	}

	// Query model catalog per unique location (cached).
	modelCache := map[string][]ai.AiModel{}
	// Cache usage/quota data per location for display in interactive fix prompts.
	usageCache := map[string][]ai.AiModelUsage{}

	// Track which locations already had their "Available models" hint appended so
	// we don't repeat the (potentially long) list for every deployment in the same region.
	availableModelsShown := map[string]bool{}

	// Track which model+location combos already had their version/SKU hint shown
	// to avoid repeating the same list for duplicate deployments.
	availableVersionsShown := map[string]bool{}
	availableSkusShown := map[string]bool{}

	const maxAvailableModels = 10
	const maxAvailableVersions = 10
	const maxAvailableSkus = 10

	// Accumulate all issues across deployments, keyed by severity.
	type modelIssue struct {
		severity PreflightCheckSeverity
		code     string
		message  string
	}
	var issues []modelIssue
	var failures []ModelDeploymentFailure
	validatedCount := 0

	for _, d := range deployments {
		locLower := strings.ToLower(d.parentAcctLo)
		models, cached := modelCache[locLower]
		if !cached {
			var err error
			models, err = listModels(ctx, subscriptionID, []string{locLower})
			if err != nil {
				log.Printf("local preflight: could not list models for %s, skipping: %v", locLower, err)
				continue
			}
			modelCache[locLower] = models
		}
		validatedCount++
		// Fetch usages if not cached yet (best-effort for quota display).
		if _, hasCached := usageCache[locLower]; !hasCached && listUsages != nil {
			usages, err := listUsages(ctx, subscriptionID, d.parentAcctLo)
			if err != nil {
				log.Printf("local preflight: could not fetch usages for %s: %v", locLower, err)
			} else {
				usageCache[locLower] = usages
			}
		}

		modelName := d.props.Model.Name
		modelVersion := d.props.Model.Version

		// Find matching model by name.
		var found *ai.AiModel
		for i := range models {
			if strings.EqualFold(models[i].Name, modelName) {
				found = &models[i]
				break
			}
		}
		if found == nil {
			msg := fmt.Sprintf(
				"AI model '%s' is not available in location '%s'.",
				modelName, d.parentAcctLo)
			// Only show the available-models hint once per location to avoid
			// repeating the (potentially long) list for every failed deployment.
			if !availableModelsShown[locLower] && len(models) > 0 {
				availableModelsShown[locLower] = true
				availableNames := make([]string, 0, len(models))
				for _, m := range models {
					availableNames = append(availableNames, m.Name)
				}
				slices.Sort(availableNames)
				if len(availableNames) > maxAvailableModels {
					shown := strings.Join(availableNames[:maxAvailableModels], ", ")
					msg += fmt.Sprintf("\nAvailable models in %s: %s, and %d more",
						d.parentAcctLo, shown, len(availableNames)-maxAvailableModels)
				} else {
					msg += fmt.Sprintf("\nAvailable models in %s: %s",
						d.parentAcctLo, strings.Join(availableNames, ", "))
				}
			}
			msg += "\nSee https://learn.microsoft.com/azure/ai-services/openai/concepts/models"
			issues = append(issues, modelIssue{
				severity: PreflightCheckError,
				code:     "local_preflight.model_not_available",
				message:  msg,
			})
			failures = append(failures, ModelDeploymentFailure{
				Location:       d.parentAcctLo,
				ModelName:      modelName,
				ModelVersion:   modelVersion,
				SkuName:        d.skuName,
				SkuCapacity:    d.skuCapacity,
				DeploymentName: d.deployName,
				FailureType:    "model_not_available",
				Models:         models,
				Usages:         usageCache[locLower],
			})
			continue
		}

		// Find matching version.
		var foundVersion *ai.AiModelVersion
		for i := range found.Versions {
			if found.Versions[i].Version == modelVersion {
				foundVersion = &found.Versions[i]
				break
			}
		}
		if foundVersion == nil {
			msg := fmt.Sprintf(
				"AI model '%s' version '%s' is not available in location '%s'.",
				modelName, modelVersion, d.parentAcctLo)
			versionKey := strings.ToLower(modelName) + "|" + locLower
			if !availableVersionsShown[versionKey] && len(found.Versions) > 0 {
				availableVersionsShown[versionKey] = true
				availableVersions := make([]string, 0, len(found.Versions))
				for _, v := range found.Versions {
					availableVersions = append(availableVersions, v.Version)
				}
				if len(availableVersions) > maxAvailableVersions {
					shown := strings.Join(availableVersions[:maxAvailableVersions], ", ")
					msg += fmt.Sprintf("\nAvailable versions: %s, and %d more",
						shown, len(availableVersions)-maxAvailableVersions)
				} else {
					msg += fmt.Sprintf("\nAvailable versions: %s",
						strings.Join(availableVersions, ", "))
				}
			}
			issues = append(issues, modelIssue{
				severity: PreflightCheckError,
				code:     "local_preflight.model_version_not_available",
				message:  msg,
			})
			failures = append(failures, ModelDeploymentFailure{
				Location:       d.parentAcctLo,
				ModelName:      modelName,
				ModelVersion:   modelVersion,
				SkuName:        d.skuName,
				SkuCapacity:    d.skuCapacity,
				DeploymentName: d.deployName,
				FailureType:    "version_not_available",
				Models:         models,
				FoundModel:     found,
				Usages:         usageCache[locLower],
			})
			continue
		}

		// Find matching SKU.
		var matchedSku *ai.AiModelSku
		if d.skuName != "" {
			for i := range foundVersion.Skus {
				if strings.EqualFold(foundVersion.Skus[i].Name, d.skuName) {
					matchedSku = &foundVersion.Skus[i]
					break
				}
			}
			if matchedSku == nil {
				msg := fmt.Sprintf(
					"SKU '%s' is not available for AI model '%s' version '%s' in location '%s'.",
					d.skuName, modelName, modelVersion, d.parentAcctLo)
				skuKey := strings.ToLower(modelName) + "|" + modelVersion + "|" + locLower
				if !availableSkusShown[skuKey] && len(foundVersion.Skus) > 0 {
					availableSkusShown[skuKey] = true
					availableSkus := make([]string, 0, len(foundVersion.Skus))
					for _, sku := range foundVersion.Skus {
						availableSkus = append(availableSkus, sku.Name)
					}
					if len(availableSkus) > maxAvailableSkus {
						shown := strings.Join(availableSkus[:maxAvailableSkus], ", ")
						msg += fmt.Sprintf("\nAvailable SKUs: %s, and %d more",
							shown, len(availableSkus)-maxAvailableSkus)
					} else {
						msg += fmt.Sprintf("\nAvailable SKUs: %s",
							strings.Join(availableSkus, ", "))
					}
				}
				issues = append(issues, modelIssue{
					severity: PreflightCheckError,
					code:     "local_preflight.model_sku_not_available",
					message:  msg,
				})
				failures = append(failures, ModelDeploymentFailure{
					Location:       d.parentAcctLo,
					ModelName:      modelName,
					ModelVersion:   modelVersion,
					SkuName:        d.skuName,
					SkuCapacity:    d.skuCapacity,
					DeploymentName: d.deployName,
					FailureType:    "sku_not_available",
					Models:         models,
					FoundModel:     found,
					FoundVersion:   foundVersion,
					Usages:         usageCache[locLower],
				})
				continue
			}
		}

		// Check quota: remaining capacity >= requested capacity.
		if matchedSku != nil && matchedSku.UsageName != "" && d.skuCapacity > 0 {
			if usages, ok := usageCache[locLower]; ok {
				for _, u := range usages {
					if strings.EqualFold(u.Name, matchedSku.UsageName) {
						remaining := u.Limit - u.CurrentValue
						if remaining < float64(d.skuCapacity) {
							msg := fmt.Sprintf(
								"Insufficient quota for AI model '%s' (%s) in '%s': "+
									"requested capacity %d, remaining quota %.0f (limit %.0f, used %.0f).",
								modelName, d.skuName, d.parentAcctLo,
								d.skuCapacity, remaining, u.Limit, u.CurrentValue)

							msg += "\nFree up quota by deleting unused deployments, reduce the requested capacity, " +
								"or try a different region." +
								"\nRequest a quota increase at " +
								"https://portal.azure.com/#view/Microsoft_Azure_Capacity/QuotaMenuBlade/~/myQuotas"

							issues = append(issues, modelIssue{
								severity: PreflightCheckWarning,
								code:     "local_preflight.model_quota_insufficient",
								message:  msg,
							})
						}
						break
					}
				}
			}
		}
	}

	if len(issues) == 0 {
		if validatedCount == 0 {
			return nil, nil, nil
		}
		return &PreflightCheckResult{
			Severity: PreflightCheckSuccess,
			Message:  "AI model deployment configuration is valid.",
		}, nil, nil
	}

	// Consolidate: highest severity wins, all messages combined.
	// Use blank-line separators between issues so each is visually distinct.
	highestSeverity := PreflightCheckWarning
	code := issues[0].code
	var sb strings.Builder
	for i, issue := range issues {
		if issue.severity == PreflightCheckError {
			highestSeverity = PreflightCheckError
			code = issue.code
		}
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(issue.message)
	}

	return &PreflightCheckResult{
		Severity: highestSeverity,
		Code:     code,
		Message:  sb.String(),
	}, failures, nil
}

// deletedVaultGetFn checks whether a specific soft-deleted vault exists.
// Returns nil when the vault does not exist.
type deletedVaultGetFn func(ctx context.Context, subscriptionId, vaultName, location string) (*keyvault.DeletedVault, error)

// checkSoftDeletedResources detects when a KeyVault resource in the snapshot has the same
// name as an existing soft-deleted vault. Azure will fail deployment in this case because
// vault names are globally unique and soft-deleted vaults retain their name until purged.
func checkSoftDeletedResources(
	ctx context.Context,
	resources []armTemplateResource,
	subscriptionID string,
	location string,
	getDeletedVault deletedVaultGetFn,
) (*PreflightCheckResult, error) {
	// Collect unique vault names from the snapshot.
	seen := map[string]bool{}
	var vaultNames []string
	for _, r := range resources {
		if strings.EqualFold(r.Type, "Microsoft.KeyVault/vaults") && !isARMExpression(r.Name) && r.Name != "" {
			lower := strings.ToLower(r.Name)
			if !seen[lower] {
				seen[lower] = true
				vaultNames = append(vaultNames, r.Name)
			}
		}
	}
	if len(vaultNames) == 0 {
		return nil, nil
	}

	type conflict struct {
		vaultName string
	}
	var conflicts []conflict
	checkedCount := 0
	for _, name := range vaultNames {
		dv, err := getDeletedVault(ctx, subscriptionID, name, location)
		if err != nil {
			log.Printf("local preflight: could not check deleted vault %q, skipping: %v", name, err)
			continue
		}
		checkedCount++
		if dv != nil {
			conflicts = append(conflicts, conflict{vaultName: name})
		}
	}

	if len(conflicts) == 0 {
		if checkedCount == 0 {
			return nil, nil
		}
		return &PreflightCheckResult{
			Severity: PreflightCheckSuccess,
			Message:  "No Key Vault name conflicts with soft-deleted vaults.",
		}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"%d Key Vault(s) conflict with soft-deleted vaults in this subscription:\n", len(conflicts)))
	for _, c := range conflicts {
		sb.WriteString(fmt.Sprintf("  - '%s' exists as a soft-deleted vault\n", c.vaultName))
	}
	sb.WriteString("Purge the deleted vault(s) with 'az keyvault purge --name <vault-name>' or use a different name.")

	return &PreflightCheckResult{
		Severity: PreflightCheckWarning,
		Code:     "local_preflight.soft_deleted_resource",
		Message:  sb.String(),
	}, nil
}

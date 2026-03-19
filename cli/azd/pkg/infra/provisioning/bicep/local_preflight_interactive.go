// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package bicep

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/azure/azure-dev/cli/azd/internal/tracing"
	"github.com/azure/azure-dev/cli/azd/internal/tracing/fields"
	"github.com/azure/azure-dev/cli/azd/pkg/ai"
	"github.com/azure/azure-dev/cli/azd/pkg/azure"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
)

// setParamValue updates the parameter that currently holds snapshotValue to newValue,
// writing to the correct store based on where the value originated:
//   - If the value traces to env vars via envMapping → writes to .env via DotenvSet
//   - Otherwise, if the ARM param is stored in config.json (infra.parameters.*) → writes to config.json
//
// This ensures the fix is picked up on retry regardless of whether the template uses
// ${ENV_VAR} syntax or bare Bicep params stored via interactive prompts.
func (p *BicepProvider) setParamValue(
	snapshotValue any,
	newValue string,
	armParams azure.ArmParameters,
	envMapping map[string][]string,
) {
	// Path A: env var mapped params → .env
	envVars := reverseMapToEnvVars(snapshotValue, armParams, envMapping)
	for _, envVar := range envVars {
		p.env.DotenvSet(envVar, newValue)
	}

	// Path B: config.json params → infra.parameters.<paramName>
	// Always check — a param can exist in both stores (env var + config.json).
	valStr := fmt.Sprintf("%v", snapshotValue)
	for paramName, param := range armParams {
		if fmt.Sprintf("%v", param.Value) == valStr {
			configKey := configInfraParametersKey + paramName
			if _, has := p.env.Config.Get(configKey); has {
				if err := p.env.Config.Set(configKey, newValue); err != nil {
					log.Printf("local preflight: failed to set config %s: %v", configKey, err)
				}
			}
		}
	}
}

// setParamValueInt is like setParamValue but for integer values (e.g., capacity).
func (p *BicepProvider) setParamValueInt(
	snapshotValue any,
	newValue int,
	armParams azure.ArmParameters,
	envMapping map[string][]string,
) {
	// Path A: env var mapped params → .env (stored as string)
	envVars := reverseMapToEnvVars(snapshotValue, armParams, envMapping)
	for _, envVar := range envVars {
		p.env.DotenvSet(envVar, fmt.Sprintf("%d", newValue))
	}

	// Path B: config.json params → infra.parameters.<paramName> (stored as int)
	// Always check — a param can exist in both stores.
	valStr := fmt.Sprintf("%v", snapshotValue)
	for paramName, param := range armParams {
		if fmt.Sprintf("%v", param.Value) == valStr {
			configKey := configInfraParametersKey + paramName
			if _, has := p.env.Config.Get(configKey); has {
				if err := p.env.Config.Set(configKey, newValue); err != nil {
					log.Printf("local preflight: failed to set config %s: %v", configKey, err)
				}
			}
		}
	}
}

// setLocationParam updates the AI resource location parameter specifically,
// skipping AZURE_LOCATION (the main deployment location) to avoid changing the
// resource group's region when only the CognitiveServices account location should change.
func (p *BicepProvider) setLocationParam(
	oldLocation string,
	newLocation string,
	armParams azure.ArmParameters,
	envMapping map[string][]string,
) {
	valStr := fmt.Sprintf("%v", oldLocation)
	for paramName, param := range armParams {
		if fmt.Sprintf("%v", param.Value) != valStr {
			continue
		}

		// Skip the main deployment location — it controls the resource group,
		// not the CognitiveServices account.
		vars := envMapping[paramName]
		if len(vars) == 1 && vars[0] == environment.LocationEnvVarName {
			continue
		}

		// Update env vars (if mapped, single-var only).
		if len(vars) == 1 {
			p.env.DotenvSet(vars[0], newLocation)
		}

		// Update config.json.
		configKey := configInfraParametersKey + paramName
		if _, has := p.env.Config.Get(configKey); has {
			if err := p.env.Config.Set(configKey, newLocation); err != nil {
				log.Printf("local preflight: failed to set config %s: %v", configKey, err)
			}
		}
	}
}

// refreshArmParams syncs armParams values from the current env and config.json state.
// This must be called after a successful interactive fix so that subsequent fixes
// (e.g., a second model deployment) use up-to-date values for parameter matching.
// Config.json is read first; env vars take priority and overwrite.
func (p *BicepProvider) refreshArmParams(
	armParams azure.ArmParameters,
	envMapping map[string][]string,
) {
	for paramName := range armParams {
		// Config.json (lower priority).
		configKey := configInfraParametersKey + paramName
		if configVal, ok := p.env.Config.Get(configKey); ok {
			armParams[paramName] = azure.ArmParameter{Value: configVal}
		}

		// Env vars take priority.
		if envKeys, ok := envMapping[paramName]; ok {
			for _, key := range envKeys {
				if val := p.env.Getenv(key); val != "" {
					armParams[paramName] = azure.ArmParameter{Value: val}
					break
				}
			}
		}
	}
}

// buildUsageMap converts a slice of AiModelUsage to a map keyed by usage name
// for efficient lookup during quota label formatting.
func buildUsageMap(usages []ai.AiModelUsage) map[string]ai.AiModelUsage {
	m := make(map[string]ai.AiModelUsage, len(usages))
	for _, u := range usages {
		m[u.Name] = u
	}
	return m
}

// modelQuotaLabel returns a gray-formatted quota summary for a model,
// showing the maximum remaining quota across all non-finetune versions and SKUs.
// Returns empty string if no usage data is available.
func modelQuotaLabel(model ai.AiModel, usageMap map[string]ai.AiModelUsage) string {
	if len(usageMap) == 0 {
		return ""
	}
	maxRemaining := modelMaxQuota(model, usageMap)
	if maxRemaining < 0 {
		return ""
	}
	return " " + output.WithGrayFormat("[up to %.0f quota available]", maxRemaining)
}

// modelMaxQuota returns the maximum remaining quota across all non-finetune SKUs.
// Returns -1 if no matching usage data is found.
func modelMaxQuota(model ai.AiModel, usageMap map[string]ai.AiModelUsage) float64 {
	var maxRemaining float64
	found := false
	for _, v := range model.Versions {
		for _, sku := range v.Skus {
			if ai.IsFinetuneUsageName(sku.UsageName) {
				continue
			}
			if usage, ok := usageMap[sku.UsageName]; ok {
				rem := usage.Limit - usage.CurrentValue
				if !found || rem > maxRemaining {
					maxRemaining = rem
					found = true
				}
			}
		}
	}
	if !found {
		return -1
	}
	return maxRemaining
}

// skuQuotaLabel returns a gray-formatted remaining quota for a specific SKU.
func skuQuotaLabel(sku ai.AiModelSku, usageMap map[string]ai.AiModelUsage) string {
	if len(usageMap) == 0 {
		return ""
	}
	usage, ok := usageMap[sku.UsageName]
	if !ok {
		return ""
	}
	remaining := usage.Limit - usage.CurrentValue
	return " " + output.WithGrayFormat("[%.0f quota available]", remaining)
}

// filterNonFinetuneSkus returns SKUs that are not fine-tune specific.
// Falls back to the original list if all SKUs are fine-tune.
func filterNonFinetuneSkus(skus []ai.AiModelSku) []ai.AiModelSku {
	filtered := make([]ai.AiModelSku, 0, len(skus))
	for _, s := range skus {
		if !ai.IsFinetuneUsageName(s.UsageName) {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		return skus
	}
	return filtered
}

// offerModelDeploymentFixes checks whether any model deployment failures can be fixed interactively
// (i.e., the failed value traces back to a single environment variable). If fixable failures exist,
// it prompts the user through model → version → SKU → capacity selection, updates the env vars,
// and returns true to signal that the caller should retry validation with the updated values.
//
// Returns (true, nil) if fixes were applied and a retry is needed.
// Returns (false, nil) if no fixable failures or user declined.
func (p *BicepProvider) offerModelDeploymentFixes(
	ctx context.Context,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, error) {
	// Determine which failures are fixable (value traces to env var or config.json).
	var fixable []ModelDeploymentFailure
	configGetter := p.env.Config.Get
	for _, f := range p.modelDeploymentFailures {
		if isParamConfigurable(f.ModelName, armParams, envMapping, configGetter) {
			fixable = append(fixable, f)
		}
	}

	if len(fixable) == 0 {
		return false, nil
	}

	// Blank line separating error report from interactive prompt (codebase convention).
	p.console.Message(ctx, "")

	fix, err := p.console.Confirm(ctx, input.ConsoleOptions{
		Message:      "Would you like to fix AI model configuration interactively?",
		DefaultValue: true,
	})
	if err != nil {
		return false, fmt.Errorf("prompting for interactive fix: %w", err)
	}
	if !fix {
		tracing.SetUsageAttributes(fields.LocalPreflightFixAccepted.Bool(false))
		return false, nil
	}

	tracing.SetUsageAttributes(fields.LocalPreflightFixAccepted.Bool(true))

	p.console.Message(ctx, "")

	anyFixed := false
	var fixSummary []string
	for i := range fixable {
		// Snapshot armParams before the fix to detect location changes.
		prevArmParams := maps.Clone(armParams)

		fixed, summary, err := p.fixModelDeployment(ctx, fixable[i], envMapping, armParams)
		if err != nil {
			return false, err
		}
		if fixed {
			anyFixed = true
			if summary != "" {
				fixSummary = append(fixSummary, summary)
			}

			// Sync armParams with current env/config so subsequent fixes
			// (e.g., a second model in the same template) match on up-to-date values.
			p.refreshArmParams(armParams, envMapping)

			// If the location param changed, update remaining failures' Location
			// so their recovery flow uses the new location (not the stale original).
			for paramName, prevParam := range prevArmParams {
				prevVal := fmt.Sprintf("%v", prevParam.Value)
				curVal := fmt.Sprintf("%v", armParams[paramName].Value)
				if prevVal == fixable[i].Location && curVal != prevVal {
					for j := i + 1; j < len(fixable); j++ {
						if fixable[j].Location == prevVal {
							fixable[j].Location = curVal
						}
					}
					break
				}
			}
		}
	}

	if anyFixed {
		if err := p.envManager.Save(ctx, p.env); err != nil {
			return false, fmt.Errorf("saving env after model fix: %w", err)
		}
		p.console.Message(ctx, "")
		for _, s := range fixSummary {
			p.console.Message(ctx, fmt.Sprintf("  Updated: %s", s))
		}
		p.console.Message(ctx, "Retrying validation with updated configuration...")
		return true, nil
	}

	return false, nil
}

// fixModelDeployment walks the user through fixing a single model deployment failure.
// Depending on the failure type, it prompts for model, version, SKU, and/or capacity selection.
// It updates the corresponding env vars and returns true if any values were changed,
// along with a human-readable summary of the change.
func (p *BicepProvider) fixModelDeployment(
	ctx context.Context,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	switch failure.FailureType {
	case "model_not_available":
		return p.fixModelSelection(ctx, failure, envMapping, armParams)
	case "version_not_available":
		return p.fixVersionSelection(ctx, failure, envMapping, armParams)
	case "sku_not_available":
		return p.fixSkuSelection(ctx, failure, envMapping, armParams)
	default:
		return false, "", nil
	}
}

// fixModelSelection presents a recovery menu for model_not_available failures.
// Options depend on whether the location parameter is configurable:
//  1. Choose a different model in the same region
//  2. Choose a different model (all regions) — only if location is configurable
//  3. Choose a different location for the current model — only if location is configurable
//
// The menu loops if the user declines after a quota warning, allowing them to try again.
func (p *BicepProvider) fixModelSelection(
	ctx context.Context,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	configGetter := p.env.Config.Get
	locationConfigurable := isParamConfigurable(
		failure.Location, armParams, envMapping, configGetter)

	options := []string{
		fmt.Sprintf("Choose a different model in %s", failure.Location),
	}
	if locationConfigurable {
		options = append(options,
			"Choose a different model (all regions)",
			fmt.Sprintf("Choose a different location for %s", failure.ModelName),
		)
	}

	for {
		// Reset params to original values before each attempt.
		// This undoes any stale writes from a previously declined attempt.
		p.setParamValue(failure.ModelName, failure.ModelName, armParams, envMapping)
		p.setLocationParam(failure.Location, failure.Location, armParams, envMapping)

		idx, err := p.console.Select(ctx, input.ConsoleOptions{
			Message: "What would you like to do?",
			Options: options,
		})
		if err != nil {
			return false, "", fmt.Errorf("selecting recovery action: %w", err)
		}

		var fixed bool
		var summary string

		switch {
		case idx == 0:
			fixed, summary, err = p.selectModelInRegion(ctx, failure, envMapping, armParams)
		case idx == 1 && locationConfigurable:
			fixed, summary, err = p.selectModelAllRegions(ctx, failure, envMapping, armParams)
		case idx == 2 && locationConfigurable:
			fixed, summary, err = p.selectLocationForModel(ctx, failure, envMapping, armParams)
		}

		if err != nil {
			return false, "", err
		}
		if fixed {
			return true, summary, nil
		}
		// User declined (e.g., quota warning) — loop back to recovery menu.
	}
}

// selectModelInRegion prompts the user to select a different model available in the
// same region, with quota-aware labels. Then cascades to version → SKU → capacity → deployment name.
func (p *BicepProvider) selectModelInRegion(
	ctx context.Context,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	models := failure.Models
	if len(models) == 0 {
		return false, "", nil
	}

	usageMap := buildUsageMap(failure.Usages)

	type modelEntry struct {
		name  string
		label string
	}
	entries := make([]modelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, modelEntry{
			name:  m.Name,
			label: m.Name + modelQuotaLabel(m, usageMap),
		})
	}
	slices.SortFunc(entries, func(a, b modelEntry) int {
		return strings.Compare(a.name, b.name)
	})

	labels := make([]string, len(entries))
	for i, e := range entries {
		labels[i] = e.label
	}

	idx, err := p.console.Select(ctx, input.ConsoleOptions{
		Message: fmt.Sprintf("Select a model available in '%s':", failure.Location),
		Options: labels,
	})
	if err != nil {
		return false, "", fmt.Errorf("selecting model: %w", err)
	}
	selectedName := entries[idx].name

	p.setParamValue(failure.ModelName, selectedName, armParams, envMapping)

	var selectedModel *ai.AiModel
	for i := range models {
		if models[i].Name == selectedName {
			selectedModel = &models[i]
			break
		}
	}
	if selectedModel == nil || len(selectedModel.Versions) == 0 {
		p.promptDeploymentName(ctx, selectedName, failure, envMapping, armParams)
		return true, fmt.Sprintf("model %s → %s", failure.ModelName, selectedName), nil
	}

	fixed, summary, err := p.selectVersionAndSku(
		ctx, selectedModel, failure, usageMap, envMapping, armParams)
	if err != nil {
		return false, "", err
	}
	if summary == "" {
		summary = fmt.Sprintf("model %s → %s", failure.ModelName, selectedName)
	}
	return fixed, summary, nil
}

// selectModelAllRegions fetches models across all regions and lets the user pick one.
// If the selected model requires a location change, prompts for location selection.
// Then cascades to version → SKU → capacity → deployment name.
func (p *BicepProvider) selectModelAllRegions(
	ctx context.Context,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	subId := p.env.GetSubscriptionId()

	allModels, err := p.aiModelService.ListModels(ctx, subId, nil)
	if err != nil {
		return false, "", fmt.Errorf("listing models across regions: %w", err)
	}
	if len(allModels) == 0 {
		return false, "", nil
	}

	// Build sorted labels with region count.
	type modelEntry struct {
		name  string
		label string
		model ai.AiModel
	}
	entries := make([]modelEntry, 0, len(allModels))
	for _, m := range allModels {
		label := fmt.Sprintf("%s %s",
			m.Name,
			output.WithGrayFormat("[available in %d region(s)]", len(m.Locations)),
		)
		entries = append(entries, modelEntry{name: m.Name, label: label, model: m})
	}
	slices.SortFunc(entries, func(a, b modelEntry) int {
		return strings.Compare(a.name, b.name)
	})

	labels := make([]string, len(entries))
	for i, e := range entries {
		labels[i] = e.label
	}

	idx, err := p.console.Select(ctx, input.ConsoleOptions{
		Message: "Select a model (all regions):",
		Options: labels,
	})
	if err != nil {
		return false, "", fmt.Errorf("selecting model: %w", err)
	}
	selectedName := entries[idx].name
	selectedAllRegion := entries[idx].model

	p.setParamValue(failure.ModelName, selectedName, armParams, envMapping)

	// Determine deployment location.
	deployLocation := failure.Location
	if !slices.Contains(selectedAllRegion.Locations, failure.Location) {
		locs := slices.Clone(selectedAllRegion.Locations)
		slices.Sort(locs)

		locIdx, err := p.console.Select(ctx, input.ConsoleOptions{
			Message: fmt.Sprintf("Select a location for '%s':", selectedName),
			Options: locs,
		})
		if err != nil {
			return false, "", fmt.Errorf("selecting location: %w", err)
		}
		deployLocation = locs[locIdx]
		p.setLocationParam(failure.Location, deployLocation, armParams, envMapping)
	}

	// Fetch location-specific models for accurate version/SKU data.
	locationModels, err := p.aiModelService.ListModels(ctx, subId, []string{deployLocation})
	if err != nil {
		log.Printf("local preflight: failed to list models for %s: %v", deployLocation, err)
		locationModels = []ai.AiModel{selectedAllRegion}
	}

	var selectedModel *ai.AiModel
	for i := range locationModels {
		if locationModels[i].Name == selectedName {
			selectedModel = &locationModels[i]
			break
		}
	}
	if selectedModel == nil || len(selectedModel.Versions) == 0 {
		p.promptDeploymentName(ctx, selectedName, failure, envMapping, armParams)
		return true, fmt.Sprintf("model %s → %s in %s",
			failure.ModelName, selectedName, deployLocation), nil
	}

	// Fetch usages for the deployment location (reuse failure usages if same location).
	var usageMap map[string]ai.AiModelUsage
	if deployLocation == failure.Location && len(failure.Usages) > 0 {
		usageMap = buildUsageMap(failure.Usages)
	} else {
		usages, usageErr := p.aiModelService.ListUsages(ctx, subId, deployLocation)
		if usageErr != nil {
			log.Printf("local preflight: failed to fetch usages for %s: %v",
				deployLocation, usageErr)
		}
		usageMap = buildUsageMap(usages)
	}

	// Warn if the selected model has no usable quota in the chosen location.
	if len(usageMap) > 0 {
		maxRemaining := modelMaxQuota(*selectedModel, usageMap)
		if maxRemaining <= 0 {
			p.console.Message(ctx, fmt.Sprintf(
				"\n  %s Model '%s' has no available quota in '%s'.",
				output.WithWarningFormat("WARNING:"), selectedName, deployLocation))
			p.console.Message(ctx,
				"  You can proceed, but deployment may fail due to insufficient quota.")
			p.console.Message(ctx, "")

			proceed, confirmErr := p.console.Confirm(ctx, input.ConsoleOptions{
				Message:      "Continue with this model?",
				DefaultValue: false,
			})
			if confirmErr != nil {
				return false, "", confirmErr
			}
			if !proceed {
				return false, "", nil // signals fixModelSelection to loop back
			}
		}
	}

	fixed, summary, err := p.selectVersionAndSku(
		ctx, selectedModel, failure, usageMap, envMapping, armParams)
	if err != nil {
		return false, "", err
	}
	if summary == "" {
		summary = fmt.Sprintf("model %s → %s in %s",
			failure.ModelName, selectedName, deployLocation)
	}
	return fixed, summary, nil
}

// selectLocationForModel lets the user pick a different location where the current
// model is available. The model name stays the same; only the location is updated.
// On retry, the validation will re-check the model in the new location.
func (p *BicepProvider) selectLocationForModel(
	ctx context.Context,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	subId := p.env.GetSubscriptionId()

	allModels, err := p.aiModelService.ListModels(ctx, subId, nil)
	if err != nil {
		return false, "", fmt.Errorf("listing models for location search: %w", err)
	}

	var targetModel *ai.AiModel
	for i := range allModels {
		if allModels[i].Name == failure.ModelName {
			targetModel = &allModels[i]
			break
		}
	}
	if targetModel == nil || len(targetModel.Locations) == 0 {
		p.console.Message(ctx, fmt.Sprintf(
			"  Model '%s' is not available in any region.", failure.ModelName))
		return false, "", nil
	}

	locs := slices.Clone(targetModel.Locations)
	slices.Sort(locs)

	locIdx, err := p.console.Select(ctx, input.ConsoleOptions{
		Message: fmt.Sprintf("Select a location for '%s':", failure.ModelName),
		Options: locs,
	})
	if err != nil {
		return false, "", fmt.Errorf("selecting location: %w", err)
	}
	newLocation := locs[locIdx]

	p.setLocationParam(failure.Location, newLocation, armParams, envMapping)

	return true, fmt.Sprintf("location %s → %s for model %s",
		failure.Location, newLocation, failure.ModelName), nil
}

// fixVersionSelection prompts the user to select a different version, then cascades to SKU.
func (p *BicepProvider) fixVersionSelection(
	ctx context.Context,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	if failure.FoundModel == nil || len(failure.FoundModel.Versions) == 0 {
		return false, "", nil
	}
	usageMap := buildUsageMap(failure.Usages)
	return p.selectVersionAndSku(ctx, failure.FoundModel, failure, usageMap, envMapping, armParams)
}

// fixSkuSelection prompts the user to select a different SKU.
func (p *BicepProvider) fixSkuSelection(
	ctx context.Context,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	if failure.FoundVersion == nil || len(failure.FoundVersion.Skus) == 0 {
		return false, "", nil
	}
	usageMap := buildUsageMap(failure.Usages)
	fixed, summary, err := p.selectSku(
		ctx, failure.FoundVersion, failure, usageMap, envMapping, armParams)
	if err != nil {
		return false, "", err
	}
	if fixed {
		p.promptDeploymentName(ctx, failure.ModelName, failure, envMapping, armParams)
	}
	return fixed, summary, err
}

// selectVersionAndSku prompts for version selection (with quota labels), then cascades to
// SKU selection and deployment name prompt.
func (p *BicepProvider) selectVersionAndSku(
	ctx context.Context,
	model *ai.AiModel,
	failure ModelDeploymentFailure,
	usageMap map[string]ai.AiModelUsage,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	versions := model.Versions
	if len(versions) == 0 {
		return true, "", nil
	}

	// Build version labels with default indicator and quota info.
	versionLabels := make([]string, 0, len(versions))
	for _, v := range versions {
		label := v.Version
		if v.IsDefault {
			label += " (default)"
		}
		if len(usageMap) > 0 {
			var maxRem float64
			found := false
			for _, sku := range filterNonFinetuneSkus(v.Skus) {
				if usage, ok := usageMap[sku.UsageName]; ok {
					rem := usage.Limit - usage.CurrentValue
					if !found || rem > maxRem {
						maxRem = rem
						found = true
					}
				}
			}
			if found {
				label += " " + output.WithGrayFormat(
					"[up to %.0f quota available]", maxRem)
			}
		}
		versionLabels = append(versionLabels, label)
	}

	defaultIdx := 0
	for i, v := range versions {
		if v.IsDefault {
			defaultIdx = i
			break
		}
	}

	idx, err := p.console.Select(ctx, input.ConsoleOptions{
		Message:      fmt.Sprintf("Select a version for '%s':", model.Name),
		Options:      versionLabels,
		DefaultValue: versionLabels[defaultIdx],
	})
	if err != nil {
		return false, "", fmt.Errorf("selecting version: %w", err)
	}
	selectedVersion := &versions[idx]

	if failure.ModelVersion != "" {
		p.setParamValue(failure.ModelVersion, selectedVersion.Version, armParams, envMapping)
	}

	if len(selectedVersion.Skus) == 0 {
		p.promptDeploymentName(ctx, model.Name, failure, envMapping, armParams)
		return true, fmt.Sprintf(
			"%s (version %s)", model.Name, selectedVersion.Version), nil
	}

	fixed, skuSummary, err := p.selectSku(
		ctx, selectedVersion, failure, usageMap, envMapping, armParams)
	if err != nil {
		return false, "", err
	}
	p.promptDeploymentName(ctx, model.Name, failure, envMapping, armParams)

	summary := fmt.Sprintf("%s (version %s, SKU %s)",
		model.Name, selectedVersion.Version, skuSummary)
	return fixed, summary, nil
}

// selectSku prompts for SKU selection with quota labels, then prompts for capacity.
func (p *BicepProvider) selectSku(
	ctx context.Context,
	version *ai.AiModelVersion,
	failure ModelDeploymentFailure,
	usageMap map[string]ai.AiModelUsage,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) (bool, string, error) {
	skus := filterNonFinetuneSkus(version.Skus)
	if len(skus) == 0 {
		return true, "", nil
	}

	// If only one SKU, auto-select it.
	if len(skus) == 1 {
		if failure.SkuName != "" {
			p.setParamValue(failure.SkuName, skus[0].Name, armParams, envMapping)
		}
		p.promptCapacity(ctx, skus[0], failure, envMapping, armParams)
		return true, skus[0].Name, nil
	}

	skuLabels := make([]string, 0, len(skus))
	for _, s := range skus {
		skuLabels = append(skuLabels, s.Name+skuQuotaLabel(s, usageMap))
	}

	idx, err := p.console.Select(ctx, input.ConsoleOptions{
		Message: "Select a SKU:",
		Options: skuLabels,
	})
	if err != nil {
		return false, "", fmt.Errorf("selecting SKU: %w", err)
	}
	selectedSku := skus[idx]

	if failure.SkuName != "" {
		p.setParamValue(failure.SkuName, selectedSku.Name, armParams, envMapping)
	}

	p.promptCapacity(ctx, selectedSku, failure, envMapping, armParams)

	return true, selectedSku.Name, nil
}

// promptCapacity prompts the user for deployment capacity if the capacity parameter
// is configurable. Shows SKU constraints and validates input against them.
func (p *BicepProvider) promptCapacity(
	ctx context.Context,
	sku ai.AiModelSku,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) {
	if failure.SkuCapacity <= 0 {
		return
	}
	configGetter := p.env.Config.Get
	if !isParamConfigurable(failure.SkuCapacity, armParams, envMapping, configGetter) {
		return
	}

	defaultCap := ai.ResolveCapacity(sku, nil)
	if defaultCap <= 0 {
		defaultCap = 1
	}

	// Build constraint hint.
	var constraints []string
	if sku.MinCapacity > 0 {
		constraints = append(constraints, fmt.Sprintf("min: %d", sku.MinCapacity))
	}
	if sku.MaxCapacity > 0 {
		constraints = append(constraints, fmt.Sprintf("max: %d", sku.MaxCapacity))
	}
	if sku.CapacityStep > 0 {
		constraints = append(constraints, fmt.Sprintf("step: %d", sku.CapacityStep))
	}

	hint := ""
	if len(constraints) > 0 {
		hint = " " + output.WithGrayFormat("(%s)", strings.Join(constraints, ", "))
	}

	response, err := p.console.Prompt(ctx, input.ConsoleOptions{
		Message:      fmt.Sprintf("Enter deployment capacity for %s%s:", sku.Name, hint),
		DefaultValue: fmt.Sprintf("%d", defaultCap),
	})
	if err != nil {
		log.Printf("local preflight: capacity prompt failed: %v", err)
		return
	}

	val, err := strconv.Atoi(strings.TrimSpace(response))
	if err != nil || val <= 0 {
		log.Printf("local preflight: invalid capacity input %q, using default %d",
			response, defaultCap)
		val = int(defaultCap)
	}

	preferred := int32(val)
	validated := ai.ResolveCapacity(sku, &preferred)
	if validated <= 0 {
		validated = preferred
	}
	p.setParamValueInt(failure.SkuCapacity, int(validated), armParams, envMapping)
}

// promptDeploymentName prompts the user for a deployment name if the deployment name
// parameter is configurable. Defaults to the model name.
func (p *BicepProvider) promptDeploymentName(
	ctx context.Context,
	modelName string,
	failure ModelDeploymentFailure,
	envMapping map[string][]string,
	armParams azure.ArmParameters,
) {
	if failure.DeploymentName == "" {
		return
	}
	configGetter := p.env.Config.Get
	if !isParamConfigurable(failure.DeploymentName, armParams, envMapping, configGetter) {
		return
	}

	response, err := p.console.Prompt(ctx, input.ConsoleOptions{
		Message:      "Enter model deployment name (defaults to model name):",
		DefaultValue: modelName,
	})
	if err != nil {
		log.Printf("local preflight: deployment name prompt failed: %v", err)
		return
	}

	name := strings.TrimSpace(response)
	if name == "" {
		name = modelName
	}
	p.setParamValue(failure.DeploymentName, name, armParams, envMapping)
}

// isParamConfigurable checks whether a snapshot value traces back to a configurable parameter:
// either via env var mapping or via config.json infra.parameters entry.
func isParamConfigurable(
	value any,
	armParams azure.ArmParameters,
	envMapping map[string][]string,
	configGetter func(string) (any, bool),
) bool {
	// Path A: env var mapped
	if len(reverseMapToEnvVars(value, armParams, envMapping)) > 0 {
		return true
	}
	// Path B: config.json
	valStr := fmt.Sprintf("%v", value)
	for paramName, param := range armParams {
		if fmt.Sprintf("%v", param.Value) == valStr {
			if configGetter != nil {
				if _, has := configGetter(configInfraParametersKey + paramName); has {
					return true
				}
			}
		}
	}
	return false
}



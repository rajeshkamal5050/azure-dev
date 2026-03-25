// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package exterrors

// Error codes for user cancellation.
const (
	CodeCancelled = "cancelled"
)

// Error codes for validation errors.
const (
	CodeAppHostNotFound          = "apphost_not_found"
	CodeInvalidManifest          = "invalid_manifest"
	CodeMissingAcaEnvironment    = "missing_aca_environment"
	CodeMissingAspirePackage     = "missing_aspire_hosting_package"
	CodeInvalidBicep             = "invalid_bicep"
	CodeNoDeployableServices     = "no_deployable_services"
	CodeManifestParseError       = "manifest_parse_error"
	CodeInvalidServiceDefinition = "invalid_service_definition"
	CodeMissingAzureEnvVars      = "missing_azure_env_vars"
)

// Error codes for dependency errors.
const (
	CodeAspireCliNotFound    = "aspire_cli_not_found"
	CodeDotnetSdkNotFound    = "dotnet_sdk_not_found"
	CodeAspirePublishFailed  = "aspire_publish_failed"
	CodeAspireManifestFailed = "aspire_manifest_failed"
	CodeAspirePushFailed     = "aspire_push_failed"
	CodeProjectNotFound      = "project_not_found"
	CodeEnvironmentNotFound  = "environment_not_found"
	CodeAspireAuthFailed     = "aspire_auth_failed"
	CodeAspireBuildFailed    = "aspire_build_failed"
)

// Error codes for configuration errors.
const (
	CodeGenerateAzureYamlFailed  = "generate_azure_yaml_failed"
	CodeGenerateParamsFailed     = "generate_params_failed"
	CodeGenerateBicepparamFailed = "generate_bicepparam_failed"
	CodeCopyInfraFailed          = "copy_infra_failed"
	CodeEnvSetFailed             = "env_set_failed"
	CodeEnvGetFailed             = "env_get_failed"
)

// Error codes for internal errors.
const (
	CodeAzdClientFailed = "azd_client_failed"
	CodeFilesystemError = "filesystem_error"
)

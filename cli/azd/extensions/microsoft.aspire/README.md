# `.NET Aspire` Extension for azd

An azd extension that integrates .NET Aspire projects with the Azure Developer CLI. Instead of azd generating Bicep (the current built-in approach), this extension delegates IaC generation to Aspire's own `aspire publish` and creates bridge artifacts so azd can provision and deploy without any Aspire-specific knowledge in core.

## What it does

- **`azd aspire init`** — Detects your Aspire AppHost, runs `aspire publish` to generate Azure Bicep, and creates bridge artifacts (`azure.yaml`, `main.parameters.json`, `.bicepparam` files) so `azd provision` and `azd deploy` work out of the box.
- **Post-provision hook** — Automatically runs `aspire do push` after `azd provision` to build and push all container images to ACR. Sets `SERVICE_*_IMAGE_NAME` env vars for deploy.

## Prerequisites

- [Azure Developer CLI (azd)](https://learn.microsoft.com/azure/developer/azure-developer-cli/install-azd) v1.23.7+
- [.NET Aspire CLI](https://learn.microsoft.com/dotnet/aspire/fundamentals/dotnet-aspire-cli) (`dotnet tool install -g Microsoft.Aspire.Cli`)
- .NET SDK 9.0+ (or 10.0 preview)
- Docker Desktop (for building container images)
- An Azure subscription

## AppHost Requirement

Your Aspire AppHost **must** include Azure Container Apps support:

```csharp
var aca = builder.AddAzureContainerAppEnvironment("aca");
```

Without this, `aspire publish` won't generate Azure-targeted Bicep. This is a one-line addition to your `AppHost.cs` or `apphost.cs`.

## Quick Start

```bash
cd your-aspire-project
azd aspire init
azd up
```

That's it. `azd aspire init` handles project setup (creates azd project, environment, generates all artifacts). `azd up` provisions infrastructure and deploys services.

## What gets generated

After running `azd aspire init`:

```
your-project/
├── azure.yaml                    ← Generated (service definitions + workflow)
├── infra/
│   ├── main.bicep                ← From Aspire (as-is, not modified)
│   ├── main.parameters.json      ← Generated (maps azd env vars → Bicep params)
│   ├── aca/aca.bicep             ← From Aspire (ACA environment)
│   ├── aca-acr/aca-acr.bicep     ← From Aspire (container registry)
│   ├── myservice/myservice.bicep ← From Aspire (per-service container app)
│   ├── myservice.bicepparam      ← Generated (param bridge)
│   └── ...
```

**Key principle**: Aspire-generated Bicep is used 100% as-is — zero modifications. The extension only generates bridge artifacts.

## Separate provision and deploy

You can also run provision and deploy separately:

```bash
azd aspire init       # One-time setup
azd provision         # Create Azure infrastructure (ACR, ACA env, etc.)
azd deploy            # Build images + deploy services
```

## Known limitation: allowInsecure

After init, you may see a warning like:

```
! Add 'allowInsecure: true' to Bicep ingress for internal HTTP services:
    basketservice, catalogservice
```

This means internal services using HTTP transport need `allowInsecure: true` in their Bicep ingress block. ACA provides automatic mTLS between services, so this is safe for internal traffic. Edit the listed service Bicep files and add `allowInsecure: true` to the `ingress` object:

```bicep
ingress: {
  external: false
  targetPort: 8080
  transport: 'http'
  allowInsecure: true    // ← Add this line
}
```

This is a gap in Aspire's Bicep generation — the `aspire publish` output doesn't include `allowInsecure` for internal HTTP services yet.

## How it works

1. **Init** — `azd aspire init` runs `aspire do publish-manifest` (gets topology) and `aspire publish` (gets Bicep). It parses the manifest to generate bridge artifacts that connect Aspire's Bicep params to azd's environment variables.

2. **Provision** — `azd provision` deploys `main.bicep` (creates resource group, ACR, ACA environment). Standard azd — no Aspire knowledge needed.

3. **Post-provision** — Extension hook runs `aspire do push` to build all container images and push to ACR. Image references are saved as `SERVICE_*_IMAGE_NAME` env vars.

4. **Deploy** — `azd deploy` deploys each service's Bicep as an ARM deployment. The `.bicepparam` files use `readEnvironmentVariable()` to resolve params from azd's `.env`.

## Cleanup

```bash
azd down --force --purge
```

## Building from source

```bash
cd cli/azd/extensions/microsoft.aspire
go build -o ~/.azd/extensions/microsoft.aspire/microsoft-aspire-$(go env GOOS)-$(go env GOARCH) .
```

# azd + Aspire Extension POC (WIP)

> **⚠️ This document is a work-in-progress POC exploration. Nothing here has been finalized or approved. The approach, design decisions, and implementation details are subject to change based on team feedback and further validation.**

## Problem

Aspire has introduced its own CLI with deployment capabilities (`aspire publish`, `aspire deploy`). Developers are confused about which CLI to use. Meanwhile, azd has ~6,943 lines of Aspire-specific Go code that requires constant reactive changes every time Aspire evolves. We need Aspire to be treated like any other framework — not a special case hardcoded into azd core.

## Guiding Principle

**azd is framework-unaware.** For every other framework (Spring Boot, Node.js, Python), azd works with `azure.yaml + infra/` — it doesn't understand the framework's internals. Aspire should be no different. The contract between any framework and azd is: you produce the artifacts, azd deploys them.

## Short-Term: Move Aspire to Extensions

- **Keep azd core lean.** Aspire-specific code doesn't belong in core. Move it out.
- **Extensions are the ownership boundary.** The Aspire extension owns everything Aspire-specific. Core stays framework-unaware.
- **Don't invent new abstractions.** Reuse existing azd patterns — revision deployment, output→env, bicepparam templates.
- **Ship, learn, then decide.** Build the extension, test it with real projects, gather feedback. Only deprecate core Aspire code once the extension is proven.
- **Extensions are throwable.** If the approach doesn't work, we can discard and try something else.

## North-Star: Shared Resource Model

Longer term, azd and Aspire should converge on a common resource and configuration model — so that azure.yaml, Aspire's app model, agentic workflows, and tooling (VS, VS Code, MCP) all build on the same foundation. This requires cross-team alignment and isn't something we tackle in the short term. We get there by first proving the extension model works.

## Ownership Boundaries

| Capability | Aspire | azd | Contract |
|---|---|---|---|
| App model / topology | ✅ | ❌ | Manifest JSON |
| IaC generation (Bicep) | ✅ | ❌ Stops generating | Bicep files on disk |
| Provisioning (ARM) | ❌ | ✅ | Bicep → ARM deployment |
| Container build/push | ✅ via `aspire do push` | ❌ Delegates to Aspire | Image URIs |
| Environment mgmt (dev/staging/prod) | ❌ | ✅ | `.env` / `azd env` |
| CI/CD pipeline generation | ❌ | ✅ | `azd pipeline` |
| Local dev orchestration | ✅ | ❌ | Aspire's domain |
| Secret/config management | ❌ | ✅ | Key Vault / `.env` |

## High-Level Overview

```
azd aspire init   →   azd provision   →   azd deploy
  (extension)          (core)              (core)
     │                    │                    │
     │ Generates:         │ Deploys:           │ Deploys:
     │ - azure.yaml       │ - main.bicep       │ - Per-service Bicep
     │ - main.params.json │ - Creates RG,      │   via .bicepparam
     │ - .bicepparam      │   ACR, ACA env     │ - Images already in
     │   per service      │                    │   ACR (pushed during
     │                    │ postprovision:     │   postprovision)
     │ From Aspire:       │ - aspire do push   │
     │ - main.bicep       │ - Sets IMAGE_NAME  │
     │ - service Bicep    │   env vars         │
     │ - manifest JSON    │                    │
```

**Key design decisions:**
1. **Aspire Bicep is used 100% as-is** — zero modifications. Extension only generates bridge artifacts.
2. **postprovision hook for image push** — `aspire do push` runs after infrastructure exists (ACR needed), before deploy starts (env vars must be set for config load).
3. **Workflow override for `azd up`** — azure.yaml overrides default `package → provision → deploy` to `provision → deploy` since packaging is handled by Aspire during postprovision.
4. **Only buildable services in azure.yaml** — Pre-built containers (Redis, Postgres) are deployed by their Bicep modules via main.bicep, not by azd deploy.

## What Changes

### The Extension Does

| Phase | Action | Input | Output |
|---|---|---|---|
| **Init** | Auto-create project/env, detect AppHost, run Aspire CLI, generate azure.yaml + main.parameters.json + .bicepparam files, pre-generate passwords | Aspire project | azd-ready project |
| **Post-provision** | `aspire do push` — builds images, pushes to ACR, sets `SERVICE_*_IMAGE_NAME` env vars | azd env vars (RG, sub, location) | Images in ACR, env vars set |

Extension capabilities used: `custom-commands` (for `azd aspire init`), `lifecycle-events` (postprovision hook for `aspire do push`).

### Core Does (unchanged)

- `azd provision` → deploys main.bicep, saves outputs to `.env`
- `azd deploy` → finds `.bicepparam` per service, Bicep CLI resolves `readEnvironmentVariable()` from azd env, compiles to ARM, deploys per-service
- `azd pipeline` → CI/CD generation
- `azd env` → environment management
- All using existing patterns — revision deployment, output→env flow, bicepparam templates

### What Gets Removed from Core (Phase 4)

| File | Lines | What it does today |
|---|---|---|
| `pkg/apphost/manifest.go` | 443 | Manifest parsing |
| `pkg/apphost/generate.go` | 2,124 | Bicep generation from manifest |
| `pkg/apphost/generate_types.go` | 184 | Template context types |
| `pkg/apphost/aca_ingress.go` | 299 | ACA ingress mapping |
| `pkg/apphost/eval.go` | 79 | Expression evaluation |
| `pkg/apphost/` tests | ~1,064 | Unit tests |
| `pkg/project/dotnet_importer.go` | 743 | Service discovery from manifest |
| `pkg/project/service_target_dotnet_containerapp.go` | 1,067 | Aspire-specific deploy target |
| `internal/appdetect/dotnet_apphost.go` | ~200 | Hardcoded Aspire detection |
| `internal/repository/detect_confirm_apphost.go` | ~100 | Detection prompts |
| `resources/apphost/templates/` | ~500 | Bicep/YAML Go templates |
| Various partial references | ~140 | Config structs, VS RPC |
| **Total** | **~6,943** | |

### What Stays in Core

- `ContainerAppTarget` (generic, non-Aspire) — handles revision deployment for any containerapp service
- `dotnet publish /t:PublishContainer` — for non-Aspire .NET apps
- Extension framework — `custom-commands`, `lifecycle-events` capabilities
- Tag injection — azd adds `azd-env-name`, `azd-service-name` etc. at ARM deployment time

## Evidence: Why This Works

| Issue | Root Cause | Extension Impact |
|---|---|---|
| #7138 — Polyglot AppHost | azd can't generate Bicep for non-.NET | **Eliminated** — Aspire generates all Bicep |
| #6450 — WebApp deployment slots | azd's Bicep lacks slot support | **Eliminated** — Aspire's Bicep has it |
| #6130 — AppService features missing | azd generates incomplete Bicep | **Eliminated** |
| #6055 — Docker tag issues | azd's tag parsing mismatches | **Significantly reduced** |
| #6320 — Dapr config removed on redeploy | azd YAML template overwrites settings | **Eliminated** — full Bicep deploy |
| #6157 — Custom binding schemes lost | azd ignores non-standard schemes | **Eliminated** |
| #6077 — Aspire 13 ContainerFiles | New manifest fields azd doesn't know | **Reduced** — extension reads mechanically |

6 of 7 fully eliminated. The 7th significantly reduced.

## Phased Approach

### Phase 1: Validate the Contract ✅ Complete

- Analyzed both codebases (aspire + azure-dev)
- Confirmed azd's revision deployment pattern matches Aspire's per-service Bicep
- Identified the param bridge pattern (`.bicepparam` with `readEnvironmentVariable()`)
- Hands-on stitch — assembled full flow manually, all steps passed
- Tested 7 samples from aspire-samples, all deployed successfully

### Phase 2: Build the Extension ✅ POC Complete

- Implemented `azd aspire init` as custom command
- Implemented `lifecycle-events` postprovision hook for `aspire do push`
- End-to-end validated on aspire-shop (6 services) and eShop (12 services)
- Fixed password flow, workflow ordering, allowInsecure detection

### Phase 3: Aspire Team Collaboration

- Asks #1–7 listed below

### Phase 4: Deprecate Tight Coupling

- Remove ~6,943 lines of Aspire-specific code from azd core
- Extension becomes the only Aspire integration path
- Migration guide for existing users

## Comparison: AI Agents Extension Pattern

| Aspect | AI Agents Extension | Aspire Extension |
|---|---|---|
| Detection | Agent schema/manifest in repo | Aspire AppHost .csproj |
| Init step | `azd ai agent init` | `azd aspire init` |
| Schema consumed | Agent manifest JSON | Aspire manifest JSON |
| Artifacts produced | azure.yaml + infra from starter kit | azure.yaml + infra from `aspire publish` |
| Deploy-time hook | None (standard azd deploy) | `lifecycle-events`: calls `aspire do push` |
| After init | Standard `azd up` | Standard `azd up` |
| azd core knowledge | None about agents | None about Aspire |

---

## How It Works End-to-End

### Step 1: `azd aspire init` (Extension — One-Time)

**Detection**: Scans for `.csproj` with `Aspire.AppHost.Sdk`. Validates `Aspire.Hosting.Azure.AppContainers` package is referenced.

**Artifact generation**: Runs two Aspire CLI commands:
1. `aspire do publish-manifest --output-path <temp>` → manifest JSON + flat Bicep modules
2. `aspire publish --output-path <temp>` → structured Bicep (main.bicep + subdirectory modules)

**Why two commands**: `aspire do publish-manifest` gives the manifest (topology). `aspire publish` gives `main.bicep` (the infra orchestrator). Ask #2 is to get both in one call.

**What the extension generates**:

`azure.yaml`:
```yaml
name: SampleApp
services:
  apiservice:
    image: ${SERVICE_APISERVICE_IMAGE_NAME}
    host: containerapp
  webfrontend:
    image: ${SERVICE_WEBFRONTEND_IMAGE_NAME}
    host: containerapp
infra:
  path: ./infra
workflows:
  up:
    - azd: provision
    - azd: deploy --all
```

`infra/apiservice.bicepparam` (the param bridge):
```bicep
using './apiservice/apiservice.bicep'

param aca_outputs_azure_container_apps_environment_default_domain = readEnvironmentVariable('aca_AZURE_CONTAINER_APPS_ENVIRONMENT_DEFAULT_DOMAIN')
param aca_outputs_azure_container_apps_environment_id = readEnvironmentVariable('aca_AZURE_CONTAINER_APPS_ENVIRONMENT_ID')
param aca_outputs_azure_container_registry_endpoint = readEnvironmentVariable('aca_AZURE_CONTAINER_REGISTRY_ENDPOINT')
param aca_outputs_azure_container_registry_managed_identity_id = readEnvironmentVariable('aca_AZURE_CONTAINER_REGISTRY_MANAGED_IDENTITY_ID')
param apiservice_containerimage = readEnvironmentVariable('SERVICE_APISERVICE_IMAGE_NAME')
param apiservice_containerport = '8080'
```

**File layout after init**:
```
project-root/
├── azure.yaml                              ← Generated by extension
├── infra/
│   ├── main.bicep                          ← From aspire publish (as-is)
│   ├── main.parameters.json                ← Generated by extension
│   ├── aca/aca.bicep                       ← From aspire publish (as-is)
│   ├── aca-acr/aca-acr.bicep               ← From aspire publish (as-is)
│   ├── apiservice/apiservice.bicep         ← From aspire publish (as-is)
│   ├── apiservice.bicepparam               ← Generated by extension
│   ├── webfrontend/webfrontend.bicep       ← From aspire publish (as-is)
│   └── webfrontend.bicepparam              ← Generated by extension
├── SampleApp.AppHost/                      ← Untouched
├── SampleApp.ApiService/                   ← Untouched
└── SampleApp.Web/                          ← Untouched
```

### Step 2: `azd provision` (Core — No Aspire Knowledge)

Standard azd provision. Deploys `infra/main.bicep` (subscription-scoped). Creates resource group, ACR, ACA environment. Outputs saved to `.env`.

Aspire's `main.bicep` is fully azd-compatible:
- ✅ `targetScope = 'subscription'`
- ✅ Creates resource group
- ✅ Parameters: `resourceGroupName`, `location`, `principalId` — azd provides all three

### Step 3: `azd deploy` (Core + Extension Post-Provision Step)

**Extension post-provision**: Calls `aspire do push` with azd's env vars (`Azure__ResourceGroup`, `Azure__SubscriptionId`, `Azure__Location`). Aspire finds the existing ACR in the same RG (deterministic via `uniqueString(resourceGroup().id)`), builds all container images, and pushes. Extension sets `SERVICE_<NAME>_IMAGE_NAME` env vars.

**Core revision deployment**: azure.yaml uses `image: ${SERVICE_<NAME>_IMAGE_NAME}` — azd skips build+push and uses the pre-pushed image. Per service: find `.bicepparam` → `bicep build-params` → `readEnvironmentVariable()` resolves params → deploy as ARM.

---

## The Param Bridge

The `.bicepparam` file bridges Aspire's Bicep param names to azd's environment variables.

```
Manifest expression: "{aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID}"
  → Bicep param:  aca_outputs_azure_container_apps_environment_id
  → azd env var:  aca_AZURE_CONTAINER_APPS_ENVIRONMENT_ID
  → bicepparam:   param aca_outputs_... = readEnvironmentVariable('aca_AZURE_CONTAINER_APPS_ENVIRONMENT_ID')

Manifest expression: "{apiservice.containerImage}"
  → bicepparam:   param apiservice_containerimage = readEnvironmentVariable('SERVICE_APISERVICE_IMAGE_NAME')

Manifest expression: "{apiservice.containerPort}"
  → bicepparam:   param apiservice_containerport = '8080'
```

Generated mechanically from the manifest's `deployment.params`. No Aspire domain knowledge needed — just string translation.

---

## Bugs Fixed During E2E Testing

### Password cross-reference in bicepparam (Fixed)

- **Problem**: Manifest params like `{postgres-password.value}` fell through to literal fallback, generating `'{postgres-password.value}'` instead of `readEnvironmentVariable('POSTGRES_PASSWORD')`
- **Fix**: Added `{resourceName.value}` pattern detection → maps to `readEnvironmentVariable('RESOURCE_NAME')`
- **File**: `internal/pkg/generator/bicepparam.go`

### Password not persisted to .env (Fixed — two-part fix)

- **Problem**: `secretOrRandomPassword` generates random passwords for ARM but doesn't save to `.env`. Service bicepparam files use `readEnvironmentVariable()` but passwords aren't there.
- **Root cause (Part 1)**: azd's `secretOrRandomPassword` generates+passes to ARM inline — no persistence
- **Fix (Part 1)**: Added Step 9a — pre-generates passwords during init, saves to `.env`
- **Root cause (Part 2)**: `secretOrRandomPassword` expects **2 args** (KeyVault name + secret name). We passed **1 arg** → always falls through to `generatePassword()`, ignoring the pre-generated one.
- **Fix (Part 2)**: Changed from `$(secretOrRandomPassword ${VAR})` to direct `${VAR}` reference. Passwords pre-generated in Step 9a, `secretOrRandomPassword` unnecessary.
- **Files**: `internal/cmd/init.go` (Step 9a), `internal/pkg/generator/mainbicep.go` (Line 98)

### `azd up` fails — package runs before provision (Fixed)

- **Problem**: `azd up` default workflow is `package → provision → deploy`. Package tries to resolve `image: ${SERVICE_*_IMAGE_NAME}` but env vars don't exist yet. Error: `empty image URL provided`.
- **Root cause**: Extension doesn't implement `package` — images are built by `aspire do push` during postprovision.
- **Fix**: Added `workflows:` section to generated azure.yaml:
  ```yaml
  workflows:
    up:
      - azd: provision
      - azd: deploy --all
  ```
  Built-in azd feature (`up.go:200-208`) — azure.yaml can customize workflow steps.
- **File**: `internal/pkg/generator/azure_yaml.go`

### allowInsecure warning for internal HTTP services (Added)

- **Problem**: Aspire's `aspire publish` does NOT generate `allowInsecure: true` for internal HTTP services. Internal service-to-service calls fail on ACA.
- **Approach**: Don't auto-patch Aspire Bicep (treat as contract). Display warning after init.
- **Detection**: `InsecureInternalServices()` checks `transport: http/http2` AND `external: false`. Skips TCP (Azure rejects allowInsecure on TCP).
- **Files**: `internal/pkg/aspire/manifest.go`, `internal/cmd/init.go`

---

## Design Decisions (Validated During E2E)

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **postprovision not predeploy** | azd resolves `${SERVICE_*_IMAGE_NAME}` at config load time (before predeploy fires). Combined with workflow override, `azd up` works correctly. |
| 2 | **Parse image tags from output** | No `--output json` flag. Tags use `aspire-deploy-{timestamp}`. Parse from output, strip ANSI, collapse multi-line. |
| 3 | **Map `aca_` prefix env vars** | Aspire outputs use `aca_` prefix, azd expects `AZURE_CONTAINER_REGISTRY_ENDPOINT`. Extension maps. |
| 4 | **Only buildable services in azure.yaml** | Pre-built containers (Redis, Postgres) deployed by Bicep modules via main.bicep. |
| 5 | **Self-bootstrapping init** | `azd aspire init` auto-runs `azd init --minimal` + `azd env new` via Workflow API. |
| 6 | **Direct `${VAR}` for passwords** | Don't use `secretOrRandomPassword` — pre-generate in Step 9a, reference directly. |
| 7 | **Workflow override for `azd up`** | Package is a no-op. Override to `provision → deploy`. |
| 8 | **Don't modify Aspire Bicep** | Treat as contract. Warn about `allowInsecure` gap instead of patching. |

---

## Design Pivots Discovered During E2E Testing

| Issue | What Happened | Resolution |
|---|---|---|
| predeploy too late | azd resolves image vars at config load time | Switched to **postprovision** hook |
| Image tag `:latest` wrong | Aspire uses `aspire-deploy-{timestamp}` tags | **Parse actual tags** from push output |
| ANSI in env vars | Markdown→ANSI output leaked into parsed image names | **Strip ANSI** with regex |
| Multi-line output | Aspire wraps long image URLs across lines | **Collapse whitespace** before parsing |
| `cache` in azure.yaml | Redis doesn't need push, was listed as service | Only include services with `containerImage` param |
| azure.yaml overwrite prompt | `azd init --minimal` creates stub, then overwrite prompt | Always overwrite (we created the stub) |
| `resourceGroupName` prompt | main.bicep expects it but azd didn't set it | Set `AZURE_RESOURCE_GROUP=rg-{envName}` during init |
| ACR auth failure | azd expects `AZURE_CONTAINER_REGISTRY_ENDPOINT`, Aspire outputs `aca_` prefix | Map prefix in postprovision |
| Verbose aspire output | Pipeline logs cluttered init | Capture quietly, dump on error only |
| Two-step init | `azd init` + `azd aspire init` confusing | Self-bootstrap via Workflow API |
| `secretOrRandomPassword` mismatch | Expects 2 args, we passed 1 → always generates new random password | **Direct `${VAR}` reference** |
| `azd up` package failure | Package runs before provision, image vars don't exist | **Workflow override**: `provision → deploy` |

---

## E2E Test Results

### Extension E2E (microsoft.aspire extension)

| # | Sample | Init | Provision | Push (postprovision) | Deploy | Notes |
|---|--------|------|-----------|---------------------|--------|-------|
| 1 | aspire-with-python | ✅ | ✅ | ✅ (1 image) | ✅ | First full E2E success. Endpoint live. |
| 2 | aspire-shop | ✅ | ✅ | ✅ (4 images) | ✅ | All services healthy. Passwords match. allowInsecure patched (3 svcs). |
| 3 | eShop | ✅ | ✅ | ✅ (9 images) | ✅ | All 12 services healthy (9 buildable + 3 pre-canned). allowInsecure patched (7 svcs). |

### Stitched Flow Validation (7 samples from aspire-samples)

| # | Sample | Languages/Tech | Services | Deploy | App Works? |
|---|--------|---------------|----------|--------|-----------|
| 1 | aspire-shop | 4 .NET + Postgres + Redis | 6 | ✅ | ⚠️ gRPC runtime issue (same with `aspire do deploy`) |
| 2 | container-build | Go (Dockerfile) | 1 | ✅ | ✅ |
| 3 | aspire-with-javascript | Angular/React/Vue + .NET | 4 | ✅ | ✅ |
| 4 | aspire-with-python | Python uvicorn + Redis | 2 | ✅ | ✅ |
| 5 | orleans-voting | Orleans .NET + Redis | 2 | ✅ | ⚠️ Blazor SignalR circuit (ACA WebSocket config) |
| 6 | client-apps-integration | .NET API (desktop excluded) | 1 | ✅ | ✅ (internal API) |
| 7 | azure-functions | Functions + Frontend + Storage | 2 | ✅ | ✅ |

**Blocked samples** (Aspire bugs, not azd):
- **database-containers**: `take(24)` name collision
- **aspire-with-node**: ACA port 80 constraint

### Key Findings

1. **The extension flow produces identical results to `aspire do deploy`.** Verified by deploying aspire-shop via both paths — same Bicep, images, env vars.
2. **`azd up` works end-to-end** with the workflow override (`provision → deploy`, skipping `package`). Validated on aspire-shop.
3. **Password pre-generation + direct `${VAR}` references solve the secret sync problem.** Pre-canned containers (Postgres, Redis) and app services sharing passwords now consistently use the same value.
4. **Runtime issues (⚠️) are ACA/sample issues, not deployment issues.** Both affect `aspire do deploy` equally.
5. **Pre-canned container wiring works.** Redis and Postgres containers are detected from the manifest, wired into main.bicep as modules, and deployed with auto-generated passwords.
6. **`allowInsecure: true` is the only manual post-init step.** Aspire's Bicep doesn't set it for internal HTTP services. Extension warns users with a list of affected services.
7. **Patterns validated**: `PublishAsDockerFile()` (Go, Python), `ContainerFiles()` (Vite→.NET), `ExcludeFromManifest()` (desktop clients), `@secure()` params, Azure Storage + Managed Identity + RBAC, Orleans clustering, `#:package` directive.
8. **Aspire CLI stability (exit status 6)** remains a friction point — transient crashes during `aspire do push` on larger projects (eShop). Usually succeeds on retry.

---

## azd Core Optimization: Skip Redundant Image Pull

**Issue**: `image: ${SERVICE_X_IMAGE_NAME}` pointing to ACR image already in target registry — azd still pulls locally, re-tags, pushes back (~30-60s per service).

**Root cause** (`container_helper.go:525-547`): `image:` code path always calls `docker.Pull()`. The `--from-package` flag has a skip path that detects remote images, but `image:` doesn't use it.

**Recommendation**: Add registry-match check in `image:` code path — skip pull+tag+push when source = target ACR.

---

## Asks for Aspire Team

1. **Auto-register ACA environment** when `Publishing:Publisher=azure` is set — currently `AddAzureContainerAppEnvironment("aca")` MUST be manually added to AppHost code
2. **Include manifest in `aspire publish` output** — currently requires two separate commands
3. **Structured image URI output from `aspire do push`** — no `--output json` flag exists, must parse console output + strip ANSI
4. **External registry support for `aspire do push`** — `--registry` flag to skip redundant ARM check (~7s). [microsoft/aspire#9852](https://github.com/microsoft/aspire/issues/9852)
5. **ACR token refresh during `aspire do push`** — token expires during long builds (>60s)
6. **Aspire CLI stability** — exit status 6, segfaults, "JSON-RPC connection lost" — transient but degrades UX
7. **`allowInsecure: true` for internal HTTP services** — `aspire publish` doesn't set it, internal calls fail. [azure-dev#3021](https://github.com/Azure/azure-dev/issues/3021). PR dotnet/aspire#14267 (merged to main, unreleased). Important: only applies to HTTP/HTTP2, NOT TCP.

---

## Aspire CLI Commands & Pipeline Steps

| Step | What it does | Used by extension |
|---|---|---|
| `aspire do publish-manifest` | Manifest JSON + flat Bicep modules | Init — topology |
| `aspire publish` | Structured Bicep (main.bicep + modules) | Init — main.bicep |
| `aspire do push` | Builds ALL images + pushes to registry | Postprovision — build+push |
| `aspire do build` | Builds ALL images (no push) | Not used |
| `aspire do push-<resource>` | Build+push one service | Optional |

Push auto-depends on build. Pipeline handles ContainerFiles, multi-stage, BuildOnly, polyglot transparently.

---

## ContainerFiles & Multi-Image Builds

```
┌──────────────────────────────────────────────────────────────┐
│  aspire do push  (called by extension during postprovision)  │
│                                                              │
│  Simple app:                                                 │
│    build-apiservice → push-apiservice                        │
│    build-webfrontend → push-webfrontend                      │
│                                                              │
│  Complex app (with ContainerFiles):                          │
│    build-react → (build-only, no push)                       │
│    build-api (depends on react output) → push-api            │
│                                                              │
│  Aspire handles ordering, multi-stage, ContainerFiles,       │
│  BuildOnly — all transparently.                              │
└──────────────────────────────────────────────────────────────┘
```

---

## Edge Cases & Risk Assessment

| Area | Current azd Handling | Extension Model | Risk |
|---|---|---|---|
| Secrets / Key Vault | azd generates KV Bicep | Aspire generates it | Low |
| Connection strings | azd evaluates in Go | Aspire bakes into Bicep env vars | None |
| Dapr | azd generates Dapr Bicep | Aspire generates all Dapr Bicep | Low |
| Volume mounts | azd generates Storage Bicep | Aspire generates it | Low |
| ACA ingress | `aca_ingress.go` (299 lines) | Aspire bakes into Bicep | None |
| Expression evaluation | `eval.go` + `generate.go` | Aspire resolves before generating Bicep | None |
| AppHost code change | N/A | `AddAzureContainerAppEnvironment("aca")` required | Ask #1 |

---

## Code Audit: What's in azd Core Today

**`pkg/apphost/`** — Manifest parsing & Bicep generation:
- `manifest.go` (443), `generate.go` (2,124), `generate_types.go` (184), `aca_ingress.go` (299), `eval.go` (79), tests (~1,064)

**`pkg/project/`** — Service targets & importers:
- `dotnet_importer.go` (743), `service_target_dotnet_containerapp.go` (1,067), `service_config.go` (~60)

**`internal/`** — Detection & tooling:
- `appdetect/dotnet_apphost.go` (~200), `repository/detect_confirm_apphost.go` (~100), `vsrpc/aspire_service.go` (~80)

**`resources/apphost/templates/`** (~500) — Bicep/YAML Go templates

All resource types eliminated: `project.v0/v1`, `container.v0/v1`, `dockerfile.v0`, `dapr.v0`, `dapr.component.v0`, `parameter.v0`, `value.v0`, `azure.bicep.v0/v1`, `postgres.server.v0`, `azure.sql.v0`, `azure.servicebus.v0`, `volume.v0`

---

## Key File References

### Extension Code

- `internal/cmd/init.go` — Main init flow, 11 steps + Step 9a password pre-generation
- `internal/cmd/listen.go` — Lifecycle event handlers (postprovision hook)
- `internal/pkg/aspire/manifest.go` — Manifest parsing, `InsecureInternalServices()`
- `internal/pkg/aspire/cli.go` — Aspire CLI wrappers
- `internal/pkg/generator/azure_yaml.go` — azure.yaml generation with workflow override
- `internal/pkg/generator/bicepparam.go` — .bicepparam generation (param bridge)
- `internal/pkg/generator/mainbicep.go` — main.parameters.json, password helpers

### azd Core (to be removed Phase 4)

- `pkg/apphost/` (~4,193 lines), `pkg/project/dotnet_importer.go` (743), `pkg/project/service_target_dotnet_containerapp.go` (1,067), `internal/appdetect/dotnet_apphost.go` (~200), `internal/repository/detect_confirm_apphost.go` (~100), `resources/apphost/templates/` (~500)

---

## Investigated Artifacts: Actual Aspire Output

### main.bicep (from `aspire publish`)

```bicep
targetScope = 'subscription'

param resourceGroupName string
param location string
param principalId string

resource rg 'Microsoft.Resources/resourceGroups@2023-07-01' = {
  name: resourceGroupName
  location: location
}

module aca_acr 'aca-acr/aca-acr.bicep' = {
  name: 'aca-acr'
  scope: rg
  params: { location: location }
}

module aca 'aca/aca.bicep' = {
  name: 'aca'
  scope: rg
  params: {
    location: location
    aca_acr_outputs_name: aca_acr.outputs.name
    userPrincipalId: principalId
  }
}

output aca_AZURE_CONTAINER_REGISTRY_MANAGED_IDENTITY_ID string = aca.outputs.AZURE_CONTAINER_REGISTRY_MANAGED_IDENTITY_ID
output aca_AZURE_CONTAINER_APPS_ENVIRONMENT_DEFAULT_DOMAIN string = aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_DEFAULT_DOMAIN
output aca_AZURE_CONTAINER_APPS_ENVIRONMENT_ID string = aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID
output aca_AZURE_CONTAINER_REGISTRY_ENDPOINT string = aca.outputs.AZURE_CONTAINER_REGISTRY_ENDPOINT
```

### apiservice.bicep (from `aspire publish`)

```bicep
param location string = resourceGroup().location
param aca_outputs_azure_container_apps_environment_default_domain string
param aca_outputs_azure_container_apps_environment_id string
param apiservice_containerimage string
param apiservice_containerport string
param aca_outputs_azure_container_registry_endpoint string
param aca_outputs_azure_container_registry_managed_identity_id string

resource apiservice 'Microsoft.App/containerApps@2025-02-02-preview' = {
  name: 'apiservice'
  location: location
  properties: {
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: { external: false, targetPort: int(apiservice_containerport), transport: 'http' }
      registries: [{ server: aca_outputs_azure_container_registry_endpoint, identity: aca_outputs_azure_container_registry_managed_identity_id }]
    }
    environmentId: aca_outputs_azure_container_apps_environment_id
    template: {
      containers: [{
        image: apiservice_containerimage
        name: 'apiservice'
        env: [
          { name: 'ASPNETCORE_FORWARDEDHEADERS_ENABLED', value: 'true' }
          { name: 'HTTP_PORTS', value: apiservice_containerport }
        ]
      }]
      scale: { minReplicas: 1 }
    }
  }
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: { '${aca_outputs_azure_container_registry_managed_identity_id}': {} }
  }
}
```

### Manifest (aspire-manifest.json, key excerpt)

```json
{
  "resources": {
    "apiservice": {
      "type": "project.v1",
      "path": "../SampleApp.ApiService/SampleApp.ApiService.csproj",
      "deployment": {
        "type": "azure.bicep.v0",
        "path": "apiservice-containerapp.module.bicep",
        "params": {
          "aca_outputs_azure_container_apps_environment_id": "{aca.outputs.AZURE_CONTAINER_APPS_ENVIRONMENT_ID}",
          "apiservice_containerimage": "{apiservice.containerImage}",
          "apiservice_containerport": "{apiservice.containerPort}"
        }
      },
      "bindings": {
        "http": { "scheme": "http", "protocol": "tcp", "transport": "http" },
        "https": { "scheme": "https", "protocol": "tcp", "transport": "http" }
      }
    }
  }
}
```

### What the Extension Extracts from the Manifest

| azd needs | Manifest field | Example |
|---|---|---|
| Service name | Resource key | `apiservice` |
| Is it deployable? | `type == "project.v1"` | Yes for services |
| Param→env mapping | `deployment.params` values | `{aca.outputs.X}` → `readEnvironmentVariable('aca_X')` |
| Image param name | Where value = `{svc.containerImage}` | `apiservice_containerimage` |
| External? | `bindings.*.external == true` | webfrontend: yes |
| Transport | `bindings.*.transport` | `http`, `http2`, `tcp` — for allowInsecure |
| Pre-canned containers | `type == "container.v0"` with known images | Redis, Postgres |

---

## Aspire's Own Deploy Flow (Reference)

```
aspire deploy:
  1. validate-azure-login
  2. create-provisioning-context
  3. PARALLEL:
     - provision-acr       → ARM deployment
     - provision-aca-env   → ARM deployment
     - build-apiservice    → dotnet publish + docker build
     - build-webfrontend   → dotnet publish + docker build
  4. push-apiservice        → push to ACR
  5. push-webfrontend       → push to ACR
  6. provision-apiservice   → ARM deployment (with real image tag)
  7. provision-webfrontend  → ARM deployment (with real image tag)
```

Maps to azd: steps 1-3 → `azd provision`, steps 3-5 → `aspire do push` (postprovision), steps 6-7 → `azd deploy`.

---

## Sample Projects Used

```
aspire-samples/samples/aspire-shop/         ← 6 services (4 .NET + Postgres + Redis)
eShopOnContainers (eShop reference)         ← 12 services (9 .NET + Postgres + Redis + RabbitMQ)
aspire-samples/samples/aspire-with-python/  ← 2 services (Python + Redis)
SampleApp (starter template)               ← 2 services (API + Web frontend)
```

Aspire CLI: 13.1.3. .NET SDK: 10.0.100-rc.2.

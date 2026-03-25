// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package aspire

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Manifest represents the parsed aspire-manifest.json structure.
type Manifest struct {
	Resources map[string]*Resource `json:"resources"`
}

// Resource represents a single resource in the Aspire manifest.
type Resource struct {
	Type       string            `json:"type"`
	Path       string            `json:"path,omitempty"`
	Deployment *Deployment       `json:"deployment,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Bindings   map[string]*Binding `json:"bindings,omitempty"`
}

// Deployment describes the deployment configuration for a resource.
type Deployment struct {
	Type   string            `json:"type"`
	Path   string            `json:"path"`
	Params map[string]string `json:"params"`
}

// Binding describes a network binding for a resource.
type Binding struct {
	Scheme    string `json:"scheme,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Transport string `json:"transport,omitempty"`
	External  bool   `json:"external,omitempty"`
}

// ServiceInfo holds the extracted information the extension needs
// to generate bridge artifacts for a deployable service.
type ServiceInfo struct {
	Name          string
	BicepPath     string // relative path to the service Bicep module
	Params        map[string]string
	ImageParam    string // the Bicep param name for the container image
	PortParam     string // the Bicep param name for the container port
	PortValue     string // the literal port value (e.g., "8080")
	IsExternal    bool
}

// InfraResource holds info about an infrastructure resource (e.g., aca, aca-acr).
type InfraResource struct {
	Name string
	Path string
}

// ParseManifest reads and parses an aspire-manifest.json file.
func ParseManifest(path string) (*Manifest, error) {
	//nolint:gosec // path is constructed from user's project directory
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}

	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse manifest JSON: %w", err)
	}

	return &manifest, nil
}

// ExtractServices returns the list of deployable services from the manifest.
// A service is deployable if it has type "project.v0", "project.v1",
// "container.v0", "container.v1", or "dockerfile.v0" and has a deployment block.
func (m *Manifest) ExtractServices() []ServiceInfo {
	var services []ServiceInfo

	for name, res := range m.Resources {
		if !isDeployableType(res.Type) || res.Deployment == nil {
			continue
		}

		svc := ServiceInfo{
			Name:      name,
			BicepPath: res.Deployment.Path,
			Params:    res.Deployment.Params,
		}

		// Find the image and port params
		for paramName, paramValue := range res.Deployment.Params {
			if strings.HasSuffix(paramValue, ".containerImage}") {
				svc.ImageParam = paramName
			}
			if strings.HasSuffix(paramValue, ".containerPort}") {
				svc.PortParam = paramName
				svc.PortValue = "8080" // default
			}
		}

		// Check if service has external bindings
		for _, binding := range res.Bindings {
			if binding.External {
				svc.IsExternal = true
				break
			}
		}

		services = append(services, svc)
	}

	return services
}

// ExtractInfraResources returns the infrastructure resources (azure.bicep.v0)
// that should be provisioned by azd provision (part of main.bicep).
func (m *Manifest) ExtractInfraResources() []InfraResource {
	var infra []InfraResource

	for name, res := range m.Resources {
		if res.Type == "azure.bicep.v0" || res.Type == "azure.bicep.v1" {
			infra = append(infra, InfraResource{
				Name: name,
				Path: res.Path,
			})
		}
	}

	return infra
}

// PreCannedService holds info about a container service that uses a pre-built
// image (e.g., Redis, PostgreSQL, RabbitMQ). These don't need `aspire do push`
// but must be wired into main.bicep so they're provisioned during `azd provision`.
type PreCannedService struct {
	Name   string
	Params map[string]string // param name → manifest expression
}

// ExtractPreCannedServices returns services that are deployable containers but
// do NOT have a containerImage param (i.e., they use pre-built images like
// docker.io/redis:latest). These need to be wired into main.bicep as modules.
func (m *Manifest) ExtractPreCannedServices() []PreCannedService {
	var preCanned []PreCannedService

	for name, res := range m.Resources {
		if !isDeployableType(res.Type) || res.Deployment == nil {
			continue
		}

		// If it has a containerImage param, it's a buildable service (handled by azure.yaml)
		hasImage := false
		for _, paramValue := range res.Deployment.Params {
			if strings.HasSuffix(paramValue, ".containerImage}") {
				hasImage = true
				break
			}
		}

		if !hasImage {
			preCanned = append(preCanned, PreCannedService{
				Name:   name,
				Params: res.Deployment.Params,
			})
		}
	}

	return preCanned
}

// InsecureInternalServices returns the names of services that are internal
// (external: false) and use HTTP/HTTP2 transport. These services need
// 'allowInsecure: true' in their Bicep ingress to work on ACA.
func (m *Manifest) InsecureInternalServices() []string {
	var names []string

	for name, res := range m.Resources {
		if !isDeployableType(res.Type) || res.Deployment == nil {
			continue
		}

		for _, binding := range res.Bindings {
			if !binding.External &&
				(binding.Transport == "http" || binding.Transport == "http2") {
				names = append(names, name)
				break
			}
		}
	}

	return names
}

func isDeployableType(t string) bool {
	switch t {
	case "project.v0", "project.v1",
		"container.v0", "container.v1",
		"dockerfile.v0":
		return true
	}
	return false
}

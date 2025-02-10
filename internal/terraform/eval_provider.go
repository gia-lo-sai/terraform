// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"
	"log"

	"github.com/hashicorp/hcl/v2"

	"maps"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/providers"
)

func buildProviderConfig(ctx EvalContext, addr addrs.AbsProviderConfig, config *configs.Provider) hcl.Body {
	var configBody hcl.Body
	if config != nil {
		configBody = config.Config
	}

	var inputBody hcl.Body
	inputConfig := ctx.ProviderInput(addr)
	if len(inputConfig) > 0 {
		inputBody = configs.SynthBody("<input-prompt>", inputConfig)
	}

	switch {
	case configBody != nil && inputBody != nil:
		log.Printf("[TRACE] buildProviderConfig for %s: merging explicit config and input", addr)
		return hcl.MergeBodies([]hcl.Body{inputBody, configBody})
	case configBody != nil:
		log.Printf("[TRACE] buildProviderConfig for %s: using explicit config only", addr)
		return configBody
	case inputBody != nil:
		log.Printf("[TRACE] buildProviderConfig for %s: using input only", addr)
		return inputBody
	default:
		log.Printf("[TRACE] buildProviderConfig for %s: no configuration at all", addr)
		return hcl.EmptyBody()
	}
}

// getProvider returns the providers.Interface and schema for a given provider.
func getProvider(ctx EvalContext, addr addrs.AbsProviderConfig) (providers.Interface, providers.ProviderSchema, error) {
	if addr.Provider.Type == "" {
		// Should never happen
		panic("GetProvider used with uninitialized provider configuration address")
	}
	provider := ctx.Provider(addr)
	if provider == nil {
		return nil, providers.ProviderSchema{}, fmt.Errorf("provider %s not initialized", addr)
	}
	// Not all callers require a schema, so we will leave checking for a nil
	// schema to the callers.
	schema, err := ctx.ProviderSchema(addr)
	if err != nil {
		return nil, providers.ProviderSchema{}, fmt.Errorf("failed to read schema for provider %s: %w", addr, err)
	}

	identitySchemas, err := ctx.ResourceIdentitySchemas(addr)
	if err == nil && len(identitySchemas.IdentityTypes) > 0 {
		resourceTypes := make(map[string]providers.Schema, len(schema.ResourceTypes))
		maps.Copy(resourceTypes, schema.ResourceTypes)

		// We only merge resource identity schemas when a provider has them available
		for name, identitySchema := range identitySchemas.IdentityTypes {
			if resource, ok := schema.ResourceTypes[name]; !ok {
				// This shouldn't happen, but in case we get an identity for a non-existent resource type
				log.Printf("[WARN] Failed to find resource type %s for provider %s", name, addr)
				continue
			} else {
				resourceTypes[name] = providers.Schema{
					Body:    resource.Body,
					Version: resource.Version,

					Identity:        identitySchema.Body,
					IdentityVersion: identitySchema.Version,
				}
			}
		}

		schema.ResourceTypes = resourceTypes
	}

	return provider, schema, nil
}

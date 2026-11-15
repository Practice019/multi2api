package channelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

type CatalogProvider struct {
	ChannelID  string   `json:"channel_id"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	ProviderID string   `json:"provider_id,omitempty"`
	Endpoints  []string `json:"endpoints"`
	Enabled    bool     `json:"enabled"`
}

type PublicCatalogModel struct {
	ID        string            `json:"id"`
	Endpoints []string          `json:"endpoints"`
	Providers []CatalogProvider `json:"providers"`
}

type UnifiedCatalog struct {
	Version string               `json:"snapshot_version"`
	ReadAt  string               `json:"read_at"`
	Source  string               `json:"source"`
	Models  []PublicCatalogModel `json:"models"`
}

func PublicNames(ch *Channel) map[string]string {
	names := map[string]string{}
	if ch == nil {
		return names
	}
	declared := map[string]bool{}
	for _, model := range ch.Models {
		declared[model] = true
	}
	mapped := map[string]bool{}
	for public, real := range ch.ModelMap {
		if public != "" && real != "" && declared[real] {
			names[public] = real
			mapped[real] = true
		}
	}
	for _, model := range ch.Models {
		if model != "" && !mapped[model] {
			names[model] = model
		}
	}
	return names
}

func BuildCatalog(channels []*Channel) UnifiedCatalog {
	result := UnifiedCatalog{Models: []PublicCatalogModel{}, Source: "channel_configuration", ReadAt: time.Now().UTC().Format(time.RFC3339Nano)}
	byModel := map[string]*PublicCatalogModel{}
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		for public, real := range PublicNames(ch) {
			model := byModel[public]
			if model == nil {
				model = &PublicCatalogModel{ID: public, Endpoints: []string{}, Providers: []CatalogProvider{}}
				byModel[public] = model
			}
			endpoints := []string{}
			for _, endpoint := range []string{"chat", "images"} {
				if ch.SupportsEndpoint(real, endpoint) {
					endpoints = append(endpoints, endpoint)
				}
			}
			model.Providers = append(model.Providers, CatalogProvider{ChannelID: ch.ID, Name: ch.Name, Kind: ch.EffectiveKind(), ProviderID: ch.ProviderID, Endpoints: endpoints, Enabled: ch.Status == StatusEnabled})
			for _, endpoint := range endpoints {
				found := false
				for _, existing := range model.Endpoints {
					if existing == endpoint {
						found = true
					}
				}
				if !found {
					model.Endpoints = append(model.Endpoints, endpoint)
				}
			}
		}
	}
	for _, model := range byModel {
		sort.Strings(model.Endpoints)
		sort.Slice(model.Providers, func(i, j int) bool { return model.Providers[i].ChannelID < model.Providers[j].ChannelID })
		result.Models = append(result.Models, *model)
	}
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].ID < result.Models[j].ID })
	raw, _ := json.Marshal(result.Models)
	digest := sha256.Sum256(raw)
	result.Version = hex.EncodeToString(digest[:])
	return result
}

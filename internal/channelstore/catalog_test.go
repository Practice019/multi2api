package channelstore

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnifiedCatalogVersionAndProviders(t *testing.T) {
	channels := []*Channel{{ID: "a", Kind: "standard", Name: "standard", Status: 1, Models: []string{"real"}, ModelMap: map[string]string{"public": "real"}, Keys: []ChannelKey{{Key: "never-publish-secret", Status: 1}}}, {ID: "b", Kind: "builtin", ProviderID: "fake", Name: "builtin", Status: 1, Models: []string{"other"}, ModelMap: map[string]string{"public": "other"}, ModelEndpoints: map[string][]string{"other": {"images"}}}}
	first := BuildCatalog(channels)
	if len(first.Models) != 1 || len(first.Models[0].Providers) != 2 || len(first.Models[0].Endpoints) != 2 {
		t.Fatalf("catalog not merged: %+v", first)
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "never-publish-secret") {
		t.Fatal("catalog leaked credential")
	}
	channels[0].Keys[0].Key = "different-secret"
	if BuildCatalog(channels).Version != first.Version {
		t.Fatal("secret rotation should not change public catalog")
	}
	channels[1].Models = []string{}
	second := BuildCatalog(channels)
	if first.Version == second.Version || len(second.Models[0].Providers) != 1 {
		t.Fatal("removed provider not reflected in same snapshot")
	}
	if BuildCatalog([]*Channel{nil}).Models == nil {
		t.Fatal("empty models must be array")
	}
}

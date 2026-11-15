package channelstore

import (
	"path/filepath"
	"reflect"
	"testing"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

func TestRoutingMetadataRoundTrip(t *testing.T) {
	row := &store.Channel{OtherSettings: `{"foreign":true}`}
	applyChannelToRow(row, &Channel{ID: "ch-b", Kind: "builtin", ProviderID: "fake"})
	got := toChannelView(row, nil)
	if got.Kind != "builtin" || got.ProviderID != "fake" {
		t.Fatalf("metadata lost: %+v", got)
	}
	if toChannelView(&store.Channel{}, nil).Kind != "standard" {
		t.Fatal("legacy kind must default standard")
	}
}

func TestRoutingPolicyIntersection(t *testing.T) {
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	g := &groupstore.Group{ID: "g-a", Name: "route", PolicyVersion: 2, ChannelIDs: []string{"ch-b", "ch-a", "ch-a"}}
	if err = gs.Upsert(g); err != nil {
		t.Fatal(err)
	}
	channels := []*Channel{{ID: "ch-a", Status: 1, Models: []string{"real"}, ModelMap: map[string]string{"public": "real"}}, {ID: "ch-b", Kind: "builtin", ProviderID: "fake", Status: 1, Models: []string{"real"}, ModelMap: map[string]string{"public": "real"}}}
	got := ResolvePolicy(gs, channels, RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}, ModelLimitsEnabled: true, ModelLimits: []string{"public"}})
	if !got.Ready || !reflect.DeepEqual(got.Models, []string{"public"}) || len(got.Candidates["public"]) != 2 {
		t.Fatalf("intersection: %+v", got)
	}
	empty := ResolvePolicy(gs, channels, RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}, ModelLimitsEnabled: true})
	if len(empty.Models) != 0 {
		t.Fatalf("enabled empty whitelist allowed: %+v", empty)
	}
	invalid := ResolvePolicy(gs, []*Channel{nil, {ID: "ch-a", Status: 1, Models: []string{"real"}, ModelMap: map[string]string{"ghost": "not-declared"}}}, RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}})
	for _, model := range invalid.Models {
		if model == "ghost" {
			t.Fatal("undeclared mapped model became routable")
		}
	}
	missing := ResolvePolicy(gs, channels, RoutingPolicy{PolicyVersion: 2, Groups: []string{"route"}})
	if missing.Ready || len(missing.Models) != 0 {
		t.Fatalf("group names must not resolve: %+v", missing)
	}
}

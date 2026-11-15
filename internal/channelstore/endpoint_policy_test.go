package channelstore

import (
	"path/filepath"
	"testing"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

func TestRoutingEndpointIsolation(t *testing.T) {
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.SaveRouting("", "images", "", []string{"ch-image", "ch-chat"})
	if err != nil {
		t.Fatal(err)
	}
	image := &Channel{ID: "ch-image", Status: 1, Models: []string{"real"}, ModelMap: map[string]string{"shared": "real"}, ModelEndpoints: map[string][]string{"real": {"images"}}}
	chat := &Channel{ID: "ch-chat", Status: 1, Models: []string{"real"}, ModelMap: map[string]string{"shared": "real"}}
	for _, tc := range []struct{ endpoint, id string }{{"images", "ch-image"}, {"chat", "ch-chat"}} {
		result := ResolvePolicy(gs, []*Channel{image, chat}, RoutingPolicy{PolicyVersion: 2, Groups: []string{g.ID}, Endpoint: tc.endpoint})
		if len(result.Candidates["shared"]) != 1 || result.Candidates["shared"][0].Channel.ID != tc.id {
			t.Fatalf("endpoint %s: %+v", tc.endpoint, result)
		}
	}
	row := &store.Channel{}
	applyChannelToRow(row, image)
	restored := toChannelView(row, nil)
	if !restored.SupportsEndpoint("real", "images") || restored.SupportsEndpoint("real", "chat") {
		t.Fatal("endpoint metadata lost")
	}
}

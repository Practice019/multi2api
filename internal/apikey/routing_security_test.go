package apikey

import (
	"errors"
	"testing"

	"workbuddy2api/internal/store"
)

func TestRoutingPolicyProjectionAndSecurity(t *testing.T) {
	ips := "192.0.2.0/24\n2001:db8::1"
	original := &store.Token{Key: "fake-secret", PolicyVersion: 2, Status: store.TokenStatusEnabled, ModelLimitsEnabled: true, UnlimitedQuota: false, AllowIps: &ips, Group: store.EncodeTokenGroups([]string{"g-route"})}
	key := keyFromToken(original)
	s := &Store{keys: map[string]*Key{key.ID: key}, byBearer: map[string]string{key.ID: key.ID}}
	s.sqlMode.Store(true)
	if _, err := s.Validate(key.ID); !errors.Is(err, ErrQuota) {
		t.Fatalf("finite zero must reject: %v", err)
	}
	if !s.AllowsIP(key.ID, "192.0.2.4:443") || !s.AllowsIP(key.ID, "[2001:db8::1]:443") || s.AllowsIP(key.ID, "198.51.100.4:443") || s.AllowsIP(key.ID, "invalid") {
		t.Fatal("IP policy not enforced")
	}
	version, groups, limited, models, ok := s.RoutingPolicyOf(key.ID)
	if !ok || version != 2 || !limited || len(models) != 0 || len(groups) != 1 {
		t.Fatal("deny-empty policy lost")
	}
	row, err := keyToToken(key)
	if err != nil {
		t.Fatal(err)
	}
	if row.PolicyVersion != 2 || !row.ModelLimitsEnabled || row.UnlimitedQuota || row.AllowIps == nil || *row.AllowIps != ips {
		t.Fatal("security fields lost on round trip")
	}
	original.UnlimitedQuota = true
	s.keys[key.ID] = keyFromToken(original)
	if _, err = s.Validate(key.ID); err != nil {
		t.Fatalf("unlimited zero must pass: %v", err)
	}
}

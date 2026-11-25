package cursor

import "testing"

func TestResolveNetworkServiceIdentity(t *testing.T) {
	for _, tt := range []struct {
		name, id, savedName, want string
		services                  map[string]networkServiceIdentity
		deleted, wantError        bool
	}{
		{
			name: "renamed service", id: "original-id", savedName: "Wi-Fi", want: "Home Wi-Fi",
			services: map[string]networkServiceIdentity{"original-id": {ID: "original-id", Name: "Home Wi-Fi", Current: true}},
		},
		{
			name: "disabled service still belongs to current set", id: "usb-id", savedName: "USB", want: "USB",
			services: map[string]networkServiceIdentity{"usb-id": {ID: "usb-id", Name: "USB", Current: true}},
		},
		{
			name: "deleted service with reused name", id: "original-id", savedName: "Wi-Fi", deleted: true,
			services: map[string]networkServiceIdentity{"replacement-id": {ID: "replacement-id", Name: "Wi-Fi", Current: true}},
		},
		{
			name: "all services deleted", id: "original-id", savedName: "Wi-Fi", deleted: true,
			services: map[string]networkServiceIdentity{},
		},
		{
			name: "inventory unavailable", id: "original-id", savedName: "Wi-Fi", wantError: true,
		},
		{
			name: "different network location", id: "original-id", savedName: "Wi-Fi", wantError: true,
			services: map[string]networkServiceIdentity{"original-id": {ID: "original-id", Name: "Wi-Fi", Current: false}},
		},
		{
			name: "malformed existing service", id: "original-id", savedName: "Wi-Fi", wantError: true,
			services: map[string]networkServiceIdentity{"original-id": {ID: "original-id", Current: true}},
		},
		{
			name: "ambiguous current names", id: "original-id", savedName: "Wi-Fi", wantError: true,
			services: map[string]networkServiceIdentity{
				"original-id": {ID: "original-id", Name: "Wi-Fi", Current: true},
				"other-id":    {ID: "other-id", Name: "Wi-Fi", Current: true},
			},
		},
		{
			name: "legacy backup unchanged name", savedName: "Wi-Fi", want: "Wi-Fi",
			services: map[string]networkServiceIdentity{"original-id": {ID: "original-id", Name: "Wi-Fi", Current: true}},
		},
		{
			name: "legacy backup missing name is not proof of deletion", savedName: "Wi-Fi", wantError: true,
			services: map[string]networkServiceIdentity{"original-id": {ID: "original-id", Name: "Renamed", Current: true}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name, deleted, err := resolveNetworkService(tt.id, tt.savedName, tt.services)
			if name != tt.want || deleted != tt.deleted || (err != nil) != tt.wantError {
				t.Fatalf("name=%q deleted=%t err=%v", name, deleted, err)
			}
		})
	}
}

func TestCurrentNetworkServiceIgnoresOtherLocationsWithSameName(t *testing.T) {
	service, err := currentNetworkService("Wi-Fi", map[string]networkServiceIdentity{
		"current-id": {ID: "current-id", Name: "Wi-Fi", Current: true},
		"other-id":   {ID: "other-id", Name: "Wi-Fi", Current: false},
	})
	if err != nil || service.ID != "current-id" {
		t.Fatalf("service=%+v err=%v", service, err)
	}
}

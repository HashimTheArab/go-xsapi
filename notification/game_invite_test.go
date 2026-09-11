package notification

import "testing"

func TestUnmarshalGameInviteOptions(t *testing.T) {
	n, err := Unmarshal([]byte(`{
		"SubscriptionCategory":"Microsoft.Xbox.Multiplayer",
		"SubscriptionType":"GameInvites",
		"SubscriptionId":"invite-1",
		"Actions":[{"ActionId":"action-1"}],
		"NotificationOptions":{
			"Location":{"Id":"1739947436","Name":"Minecraft"},
			"Platforms":["android","uwp-desktop"]
		}
	}`))
	if err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	invite, ok := n.(*GameInvite)
	if !ok {
		t.Fatalf("notification type = %T, want *GameInvite", n)
	}
	if invite.Options.Location.ID != "1739947436" {
		t.Fatalf("location ID = %q, want 1739947436", invite.Options.Location.ID)
	}
	if len(invite.Options.Platforms) != 2 || invite.Options.Platforms[0] != "android" || invite.Options.Platforms[1] != "uwp-desktop" {
		t.Fatalf("platforms = %v, want [android uwp-desktop]", invite.Options.Platforms)
	}
}

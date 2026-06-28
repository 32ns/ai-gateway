package controlplane

import (
	"testing"

	"github.com/32ns/ai-gateway/internal/core"
)

func TestReadAccountQuotaParsesSnapshot(t *testing.T) {
	snapshot := ReadAccountQuota(core.Account{
		Credential: core.Credential{
			Metadata: map[string]string{
				core.AccountQuotaMetadataKey: `{"Source":"claude_oauth_usage","Primary":{"UsedPercent":40},"Secondary":{"UsedPercent":25}}`,
			},
		},
	})
	if snapshot == nil {
		t.Fatal("expected quota snapshot")
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 40 {
		t.Fatalf("primary = %#v, want 40", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.UsedPercent != 25 {
		t.Fatalf("secondary = %#v, want 25", snapshot.Secondary)
	}
}

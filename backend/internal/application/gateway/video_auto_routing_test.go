package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func TestAutoVideo720pTenSecondRouting(t *testing.T) {
	ctx := context.WithValue(context.Background(), videoDurationContextKey{}, 10)
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "auto-video.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierAuto,
		Name: "auto-video", SourceKey: "auto-video", EncryptedAccessToken: "encrypted",
		Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(web.NewAdapter(web.Config{}, nil, nil, nil, nil))
	for _, tc := range []struct {
		name      string
		tiers     clientkeydomain.TierScope
		remaining int
		want      bool
	}{
		{"all tiers with quota", clientkeydomain.TierScopeAll, 10, true},
		{"free plus super excludes auto", clientkeydomain.TierScopeFree | clientkeydomain.TierScopeSuper, 10, false},
		{"super only excludes auto", clientkeydomain.TierScopeSuper, 10, false},
		{"exhausted product quota", clientkeydomain.TierScopeAll, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			if err := accounts.SaveQuotaWindows(ctx, credential.ID, account.WebTierAuto, now, []account.QuotaWindow{{AccountID: credential.ID, Mode: account.QuotaModeWebVideo720p, Remaining: tc.remaining, Total: 10, SyncedAt: &now, Source: account.QuotaSourceUpstream}}); err != nil {
				t.Fatal(err)
			}
			selector := NewSelector(accounts, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), registry, time.Hour, time.Second, time.Minute)
			session, err := selector.beginSelectionSessionForKey(ctx, account.ProviderWeb, 0, "grok-imagine-video", videoQuotaMode(account.ProviderWeb, registry.QuotaMode(account.ProviderWeb, "grok-imagine-video"), "720p"), "", nil, false, clientkeydomain.AccountScope{Tiers: tc.tiers})
			if !tc.want {
				if err == nil {
					t.Fatal("unexpected eligible auto account")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			lease, err := session.Acquire(ctx, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			if lease.Credential.ID != credential.ID || lease.Credential.WebTier != account.WebTierAuto {
				t.Fatalf("unexpected credential: id=%d tier=%s", lease.Credential.ID, lease.Credential.WebTier)
			}
		})
	}
}

func TestVideoDurationPolicyHotReload(t *testing.T) {
	adapter := web.NewAdapter(web.Config{}, nil, nil, nil, nil)
	registry := provider.NewRegistry(adapter)
	selector := NewSelector(nil, nil, nil, registry, time.Hour, time.Second, time.Minute)
	for _, minimum := range []int{7, 6, 1, 10, 7} {
		adapter.UpdateConfig(web.Config{NonFreeVideoMinDuration: minimum})
		for _, tier := range []account.WebTier{account.WebTierAuto, account.WebTierSuper, account.WebTierHeavy, account.WebTierBasic} {
			for _, seconds := range []int{5, 6, 7, 10} {
				credential := account.Credential{WebTier: tier}
				want := seconds >= minimum
				if tier == account.WebTierBasic {
					want = seconds == 6
				}
				ctx := context.WithValue(context.Background(), videoDurationContextKey{}, seconds)
				if got := selector.candidateSupportsVideoDuration(ctx, account.ProviderWeb, credential); got != want {
					t.Fatalf("minimum=%d tier=%s seconds=%d got=%v want=%v", minimum, tier, seconds, got, want)
				}
			}
		}
	}
}

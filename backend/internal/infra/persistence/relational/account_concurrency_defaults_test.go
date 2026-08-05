package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAccountRepositoryAppliesManagedWebConcurrencyDefaults(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	tests := []struct {
		name string
		tier account.WebTier
		want int
	}{
		{name: "auto", tier: account.WebTierAuto, want: account.DefaultMaxConcurrent},
		{name: "basic", tier: account.WebTierBasic, want: account.DefaultMaxConcurrent},
		{name: "super", tier: account.WebTierSuper, want: account.DefaultWebMaxConcurrent},
		{name: "heavy", tier: account.WebTierHeavy, want: account.DefaultWebHeavyMaxConcurrent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, created, err := repo.UpsertByIdentity(ctx, account.Credential{
				Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: test.tier,
				Name: test.name, SourceKey: test.name, EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
			})
			if err != nil || !created {
				t.Fatalf("created = %t, err = %v", created, err)
			}
			if value.MaxConcurrent != test.want || !value.MaxConcurrentManaged {
				t.Fatalf("concurrency = %d, managed = %t; want %d, true", value.MaxConcurrent, value.MaxConcurrentManaged, test.want)
			}
		})
	}

	manual, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierHeavy,
		Name: "manual", SourceKey: "manual", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive, MaxConcurrent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manual.MaxConcurrent != 4 || manual.MaxConcurrentManaged {
		t.Fatalf("manual concurrency = %d, managed = %t", manual.MaxConcurrent, manual.MaxConcurrentManaged)
	}
}

func TestQuotaSyncUpdatesOnlyManagedConcurrency(t *testing.T) {
	ctx := context.Background()
	repo := NewAccountRepository(openTestDatabase(t))
	managed, _, err := repo.UpsertByIdentity(ctx, account.Credential{
		Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierAuto,
		Name: "managed", SourceKey: "managed", EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repo.ReplaceQuotaWindows(ctx, managed.ID, account.WebTierSuper, now, nil); err != nil {
		t.Fatal(err)
	}
	managed, err = repo.Get(ctx, managed.ID)
	if err != nil || managed.MaxConcurrent != account.DefaultWebMaxConcurrent || !managed.MaxConcurrentManaged {
		t.Fatalf("Super sync = %#v, err = %v", managed, err)
	}
	if err := repo.ReplaceQuotaWindows(ctx, managed.ID, account.WebTierHeavy, now.Add(time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	managed, err = repo.Get(ctx, managed.ID)
	if err != nil || managed.MaxConcurrent != account.DefaultWebHeavyMaxConcurrent || !managed.MaxConcurrentManaged {
		t.Fatalf("Heavy sync = %#v, err = %v", managed, err)
	}
	if err := repo.ReplaceQuotaWindows(ctx, managed.ID, account.WebTierBasic, now.Add(2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	managed, err = repo.Get(ctx, managed.ID)
	if err != nil || managed.MaxConcurrent != account.DefaultMaxConcurrent || !managed.MaxConcurrentManaged {
		t.Fatalf("Basic downgrade sync = %#v, err = %v", managed, err)
	}

	manualConcurrent := 3
	if _, err := repo.UpdateMany(ctx, account.ProviderWeb, []uint64{managed.ID}, repository.AccountUpdates{MaxConcurrent: &manualConcurrent}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceQuotaWindows(ctx, managed.ID, account.WebTierSuper, now.Add(3*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	managed, err = repo.Get(ctx, managed.ID)
	if err != nil || managed.MaxConcurrent != manualConcurrent || managed.MaxConcurrentManaged {
		t.Fatalf("manual concurrency after sync = %#v, err = %v", managed, err)
	}
}

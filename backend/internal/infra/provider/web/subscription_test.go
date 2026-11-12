package web

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"google.golang.org/protobuf/encoding/protowire"
)

func subscriptionBytes(field protowire.Number, value []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, field, protowire.BytesType), value)
}
func subscriptionTimestamp(value time.Time) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), uint64(value.Unix()))
}
func subscriptionFixture(entitlement string, start, end time.Time) []byte {
	// Sanitized observed GetSubscriptions layout; status 4 is present in
	// the canceled-but-unexpired Super and Premium captures.
	detail := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 4)
	detail = append(detail, subscriptionBytes(4, []byte(entitlement))...)
	detail = append(detail, subscriptionBytes(4, []byte("x.premium"))...)
	detail = append(detail, subscriptionBytes(8, subscriptionTimestamp(end))...)
	detail = append(detail, subscriptionBytes(11, subscriptionTimestamp(start))...)
	payload := subscriptionBytes(1, subscriptionBytes(2, detail))
	frame := make([]byte, 5)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	return append(frame, payload...)
}
func TestSubscriptionTierObservedEntitlements(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, entitlement string
		start, end        time.Time
		want              account.WebTier
	}{
		{"Super canceled renewal", "supergrok.plus", now.Add(-time.Hour), now.Add(time.Hour), account.WebTierSuper},
		{"Premium", "supergrok", now.Add(-time.Hour), now.Add(time.Hour), account.WebTierSuper},
		{"expired", "supergrok.plus", now.Add(-time.Hour), now, account.WebTierAuto},
		{"future", "supergrok.plus", now.Add(time.Hour), now.Add(2 * time.Hour), account.WebTierAuto},
		{"X only", "x.premium", now.Add(-time.Hour), now.Add(time.Hour), account.WebTierAuto},
		{"unknown plan", "unverified", now.Add(-time.Hour), now.Add(time.Hour), account.WebTierAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSubscriptionTier(subscriptionFixture(tc.entitlement, tc.start, tc.end), now)
			if err != nil || got != tc.want {
				t.Fatalf("tier=%s err=%v", got, err)
			}
		})
	}
	empty, _ := hex.DecodeString("0000000000800000000f677270632d7374617475733a300d0a")
	if got, err := parseSubscriptionTier(empty, now); err != nil || got != account.WebTierAuto {
		t.Fatalf("empty tier=%s err=%v", got, err)
	}
	if _, err := parseSubscriptionTier([]byte{0, 0, 0, 0, 2, 10}, now); err == nil {
		t.Fatal("truncated response accepted")
	}
}
func TestSyncQuotaSubscriptionOverridesLegacyCounters(t *testing.T) {
	weekly, _ := hex.DecodeString("00000000520a500d0000803f220c08d6fab5d7061080eba5c3022a0c08d6efdad7061080eba5c3023a070805150000803f421e0802120c08d6fab5d7061080eba5c3021a0c08d6efdad7061080eba5c302580162006801800000000f677270632d7374617475733a300d0a")
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case subscriptionsPath:
			if r.Method != http.MethodPost || r.Header.Get("x-grpc-web") != "1" {
				t.Error("incorrect subscription request")
			}
			w.Write(subscriptionFixture("supergrok.plus", now.Add(-time.Hour), now.Add(time.Hour)))
		case "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig":
			w.Write(weekly)
		default:
			t.Errorf("unexpected legacy quota call: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: server.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	snapshot, err := adapter.SyncQuota(context.Background(), account.Credential{ID: 1, WebTier: account.WebTierAuto, EncryptedAccessToken: encrypted})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tier != account.WebTierSuper || len(snapshot.Windows) != 1 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	window := snapshot.Windows[0]
	if window.Mode != "weekly" || window.UsagePercent != 1 || window.Remaining != 9900 || window.ResetAt.Unix() != 1794553814 {
		t.Fatalf("window=%#v", window)
	}
}

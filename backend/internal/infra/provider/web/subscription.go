package web

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"google.golang.org/protobuf/encoding/protowire"
)

const subscriptionsPath = "/commerce.subscription.v1.SubscriptionService/GetSubscriptions"

func (a *Adapter) syncSubscriptionTier(ctx context.Context, credential account.Credential) (account.WebTier, error) {
	body, err := a.fetchBillingMessage(ctx, credential, subscriptionsPath)
	if err != nil {
		return account.WebTierAuto, err
	}
	return parseSubscriptionTier(body, time.Now().UTC())
}

// subscriptionFields reads only the observed protobuf paths. Unknown fields are
// skipped; malformed known fields fail closed rather than becoming entitlements.
func subscriptionFields(message []byte, target protowire.Number) ([][]byte, error) {
	var result [][]byte
	for len(message) > 0 {
		number, kind, n := protowire.ConsumeTag(message)
		if n < 0 {
			return nil, fmt.Errorf("invalid subscription protobuf tag")
		}
		message = message[n:]
		size := protowire.ConsumeFieldValue(number, kind, message)
		if size < 0 {
			return nil, fmt.Errorf("invalid subscription protobuf field")
		}
		if number == target {
			if kind != protowire.BytesType {
				return nil, fmt.Errorf("invalid subscription field type")
			}
			value, n := protowire.ConsumeBytes(message)
			if n < 0 {
				return nil, fmt.Errorf("invalid subscription message")
			}
			result = append(result, value)
		}
		message = message[size:]
	}
	return result, nil
}

func parseSubscriptionTier(body []byte, now time.Time) (account.WebTier, error) {
	payload, err := firstGRPCWebMessage(body)
	if err != nil {
		return account.WebTierAuto, err
	}
	records, err := subscriptionFields(payload, 1)
	if err != nil {
		return account.WebTierAuto, err
	}
	tier := account.WebTierAuto
	for _, record := range records {
		details, err := subscriptionFields(record, 2)
		if err != nil {
			return account.WebTierAuto, err
		}
		for _, detail := range details {
			// Observed field 11 is the start and field 8 the subscription end.
			// Canceling renewal does not remove access before this end timestamp.
			starts, err := subscriptionFields(detail, 11)
			if err != nil {
				return account.WebTierAuto, err
			}
			ends, err := subscriptionFields(detail, 8)
			if err != nil {
				return account.WebTierAuto, err
			}
			if len(starts) != 1 || len(ends) != 1 {
				continue
			}
			start, err := parseProtoTimestamp(starts[0])
			if err != nil {
				return account.WebTierAuto, err
			}
			end, err := parseProtoTimestamp(ends[0])
			if err != nil {
				return account.WebTierAuto, err
			}
			if now.Before(start) || !now.Before(end) || !end.After(start) {
				continue
			}
			entitlements, err := subscriptionFields(detail, 4)
			if err != nil {
				return account.WebTierAuto, err
			}
			for _, entitlement := range entitlements {
				// Premium (supergrok) and Super (supergrok.plus) both use the
				// existing paid Web routing tier and shared weekly quota. Neither
				// establishes Heavy privileges. x.premium alone is insufficient.
				if string(entitlement) == "supergrok" || string(entitlement) == "supergrok.plus" {
					tier = account.WebTierSuper
				}
			}
		}
	}
	return tier, nil
}

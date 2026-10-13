package quota

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func quotaTestTime() time.Time {
	return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
}

func TestParseMapsAllPoolsAndWindowsByID(t *testing.T) {
	body := []byte(`{"groups":[
		{"displayName":"Claude and GPT models","buckets":[
			{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.4,"resetTime":"2026-10-13T04:42:09Z"},
			{"bucketId":"3p-5h","window":"5h","remainingFraction":0.7,"resetTime":"2026-10-06T16:31:11Z"}]},
		{"displayName":"Gemini Models","buckets":[
			{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.9,"resetTime":"2026-10-11T16:02:27Z"},
			{"bucketId":"gemini-5h","window":"5h","remainingFraction":0,"resetTime":"2026-10-06T16:30:59Z"},
			{"bucketId":"gemini-model-weekly","remainingFraction":1,"resetTime":"wrong"}]}]}`)
	snapshot, err := Parse(body, quotaTestTime())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != "retrieveUserQuotaSummary" || !snapshot.ObservedAt.Equal(quotaTestTime()) {
		t.Fatalf("observation metadata = %+v", snapshot)
	}
	if len(snapshot.Buckets) != 5 {
		t.Fatalf("expected 5 buckets, got %d: %+v", len(snapshot.Buckets), snapshot.Buckets)
	}

	cases := []struct {
		id        string
		groupName string
		window    string
		fraction  float64
		reset     string
	}{
		{"gemini-5h", "Gemini Models", "5h", 0, "2026-10-06T16:30:59Z"},
		{"gemini-weekly", "Gemini Models", "weekly", 0.9, "2026-10-11T16:02:27Z"},
		{"3p-5h", "Claude and GPT models", "5h", 0.7, "2026-10-06T16:31:11Z"},
		{"3p-weekly", "Claude and GPT models", "weekly", 0.4, "2026-10-13T04:42:09Z"},
	}
	for _, test := range cases {
		b, found := snapshot.Bucket(test.id)
		if !found {
			t.Fatalf("bucket %s not found", test.id)
		}
		if b.PoolDisplayName != test.groupName || b.Window != test.window || b.Status != StatusAvailable || b.UnavailableReason != "" {
			t.Fatalf("mapping %s = %+v", test.id, b)
		}
		if b.RemainingFraction == nil || *b.RemainingFraction != test.fraction {
			t.Fatalf("fraction %s = %+v", test.id, b)
		}
		if b.ResetAt == nil || b.ResetAt.Format(time.RFC3339) != test.reset || b.RemainingAmount != nil {
			t.Fatalf("authoritative reset/amount %s = %+v", test.id, b)
		}
	}

	modelB, found := snapshot.Bucket("gemini-model-weekly")
	if !found || modelB.Status != StatusUnavailable || modelB.UnavailableReason != "invalid_reset_time" {
		t.Fatalf("unrecognized bucket with bad reset time: %+v", modelB)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil || !strings.Contains(string(encoded), `"remaining_fraction":0`) {
		t.Fatalf("JSON preserves zero: %s, %v", encoded, err)
	}
}

func TestParseWindowAvailabilityAndInvalidData(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		status WindowStatus
		reason string
	}{
		{"missing values", ``, StatusUnavailable, "quota_not_reported"},
		{"null values", `,"remainingFraction":null,"remainingAmount":null,"resetTime":null`, StatusUnavailable, "quota_not_reported"},
		{"disabled", `,"disabled":true,"remainingFraction":0.8`, StatusDisabled, "upstream_disabled"},
		{"disabled malformed fraction", `,"disabled":true,"remainingFraction":"bad"`, StatusDisabled, "upstream_disabled"},
		{"string fraction", `,"remainingFraction":"0.5"`, StatusUnavailable, "invalid_remaining_fraction"},
		{"boolean fraction", `,"remainingFraction":true`, StatusUnavailable, "invalid_remaining_fraction"},
		{"object fraction", `,"remainingFraction":{}`, StatusUnavailable, "invalid_remaining_fraction"},
		{"negative fraction", `,"remainingFraction":-0.01`, StatusUnavailable, "invalid_remaining_fraction"},
		{"overfull fraction", `,"remainingFraction":1.01`, StatusUnavailable, "invalid_remaining_fraction"},
		{"overflow fraction", `,"remainingFraction":1e999`, StatusUnavailable, "invalid_remaining_fraction"},
		{"invalid reset", `,"remainingFraction":0.5,"resetTime":"tomorrow"`, StatusUnavailable, "invalid_reset_time"},
		{"numeric reset", `,"remainingFraction":0.5,"resetTime":123`, StatusUnavailable, "invalid_reset_time"},
		{"empty reset", `,"remainingFraction":0.5,"resetTime":""`, StatusUnavailable, "invalid_reset_time"},
		{"invalid disabled", `,"remainingFraction":0.5,"disabled":"false"`, StatusUnavailable, "invalid_disabled"},
		{"numeric amount", `,"remainingAmount":123`, StatusUnavailable, "invalid_remaining_amount"},
		{"fractional amount", `,"remainingAmount":"1.5"`, StatusUnavailable, "invalid_remaining_amount"},
		{"overflow amount", `,"remainingAmount":"9223372036854775808"`, StatusUnavailable, "invalid_remaining_amount"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := Parse([]byte(`{"buckets":[{"bucketId":"gemini-5h"`+test.fields+`}]}`), quotaTestTime())
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Buckets) != 1 {
				t.Fatalf("expected exactly 1 bucket, got %d", len(snapshot.Buckets))
			}
			b := snapshot.Buckets[0]
			if b.Status != test.status || b.UnavailableReason != test.reason || b.RemainingFraction != nil {
				t.Fatalf("unusable bucket = %+v", b)
			}
		})
	}
}

func TestParseAmountAndResetWithoutInventingPercentages(t *testing.T) {
	for _, amount := range []string{"0", "9007199254740993", "9223372036854775807", "-9223372036854775808"} {
		snapshot, err := Parse([]byte(`{"summary":{"buckets":[{"bucketId":"3p-weekly","remainingAmount":"`+amount+`","resetTime":"2026-10-13T04:42:09.123456789+02:00"}]}}`), quotaTestTime())
		if err != nil {
			t.Fatal(err)
		}
		b, found := snapshot.Bucket("3p-weekly")
		if !found || b.Status != StatusAvailable || b.RemainingAmount == nil || *b.RemainingAmount != amount || b.RemainingFraction != nil || b.ResetAt == nil || b.ResetAt.Format(time.RFC3339Nano) != "2026-10-13T04:42:09.123456789+02:00" {
			t.Fatalf("amount bucket = %+v", b)
		}
	}
	snapshot, err := Parse([]byte(`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":1},{"bucketId":"gemini-weekly","disabled":true,"remainingAmount":"7","resetTime":"2026-10-11T16:02:27Z"}]}`), quotaTestTime())
	if err != nil {
		t.Fatal(err)
	}
	g5h, _ := snapshot.Bucket("gemini-5h")
	if g5h.ResetAt != nil || g5h.RemainingFraction == nil || *g5h.RemainingFraction != 1 {
		t.Fatalf("gemini-5h lost full fraction: %+v", g5h)
	}
	gw, _ := snapshot.Bucket("gemini-weekly")
	if gw.Status != StatusDisabled || gw.ResetAt == nil || gw.RemainingAmount != nil {
		t.Fatalf("disabled reset metadata = %+v", gw)
	}
}

func TestParseWrappersDuplicatesAndMalformedEnvelopes(t *testing.T) {
	valid := []string{
		`{}`,
		`{"models":{"gemini-weekly":{"quotaInfo":{"remainingFraction":1}}}}`,
		`{"response":{"summary":{"buckets":[{"bucketId":"3p-5h","remainingFraction":0.5}]}}}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5},{"bucketId":"gemini-5h","remainingFraction":0.50}]}`,
		`{"buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5},{"bucketId":"gemini-5h","window":"5h","disabled":false,"remainingFraction":0.50,"remainingAmount":null}]}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5}],"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5}]}]}`,
		`{"buckets":[{"bucketId":"unrecognized","remainingFraction":"bad","resetTime":false}]}`,
	}
	for _, body := range valid {
		if _, err := Parse([]byte(body), quotaTestTime()); err != nil {
			t.Errorf("valid envelope %s: %v", body, err)
		}
	}
	invalid := []string{
		`null`, `[]`, `not JSON`, `{} {}`, `{"buckets":{}}`, `{"groups":null}`, `{"response":false}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5},{"bucketId":"gemini-5h","remainingFraction":0.6}]}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":"bad"},{"bucketId":"gemini-5h","remainingFraction":"worse"}]}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingAmount":"1"},{"bucketId":"gemini-5h","remainingAmount":"2"}]}`,
		`{"buckets":[{"bucketId":"gemini-5h","disabled":true},{"bucketId":"gemini-5h","disabled":false}]}`,
		`{"buckets":[{"bucketId":"gemini-5h","resetTime":"2026-10-06T12:00:00Z"},{"bucketId":"gemini-5h","resetTime":"2026-10-06T13:00:00Z"}]}`,
		`{"groups":[{"displayName":"one","buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5}]},{"displayName":"two","buckets":[{"bucketId":"gemini-weekly","remainingFraction":0.5}]}]}`,
		strings.Repeat(`{"response":`, 10) + `{}` + strings.Repeat(`}`, 10),
	}
	for _, body := range invalid {
		if _, err := Parse([]byte(body), quotaTestTime()); err == nil {
			t.Errorf("accepted invalid/conflicting envelope %s", body)
		}
	}
}

func TestParseSuccessiveStatesDoNotMutateEarlierObservation(t *testing.T) {
	first, err := Parse([]byte(`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0,"remainingAmount":"0","resetTime":"2026-10-06T16:30:59Z"}]}`), quotaTestTime())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(first)
	for _, body := range []string{
		`{"buckets":[{"bucketId":"gemini-5h","disabled":true}]}`,
		`{"buckets":[]}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":1}]}`,
	} {
		if _, err := Parse([]byte(body), quotaTestTime().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := json.Marshal(first)
	if string(before) != string(after) {
		t.Fatal("later observations mutated prior snapshot")
	}
	second, err := Parse([]byte(`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0}]}`), quotaTestTime())
	if err != nil {
		t.Fatal(err)
	}
	newFrac := 0.75
	second.Buckets[0].RemainingFraction = &newFrac
	firstB, _ := first.Bucket("gemini-5h")
	if *firstB.RemainingFraction != 0 {
		t.Fatal("independent snapshots share mutable values")
	}
}

func TestParseWeeklyOnlyTierProducesOnlyWeeklyBuckets(t *testing.T) {
	raw := `{
		"groups": [
			{
				"displayName": "Gemini Models",
				"buckets": [
					{
						"bucketId": "gemini-weekly",
						"window": "weekly",
						"remainingFraction": 0.85,
						"resetTime": "2026-10-20T11:13:52Z"
					}
				]
			},
			{
				"displayName": "Claude and GPT models",
				"buckets": [
					{
						"bucketId": "3p-weekly",
						"window": "weekly",
						"remainingFraction": 0.95,
						"resetTime": "2026-10-20T11:24:12Z"
					}
				]
			}
		]
	}`

	snapshot, err := Parse([]byte(raw), quotaTestTime())
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}

	// Must contain exactly 2 buckets: gemini-weekly and 3p-weekly
	if len(snapshot.Buckets) != 2 {
		t.Fatalf("expected exactly 2 buckets, got %d: %+v", len(snapshot.Buckets), snapshot.Buckets)
	}

	if _, found := snapshot.Bucket("gemini-5h"); found {
		t.Fatal("gemini-5h must NOT be synthesized when omitted by upstream")
	}
	if _, found := snapshot.Bucket("3p-5h"); found {
		t.Fatal("3p-5h must NOT be synthesized when omitted by upstream")
	}

	gw, found := snapshot.Bucket("gemini-weekly")
	if !found || gw.Status != StatusAvailable || *gw.RemainingFraction != 0.85 {
		t.Fatalf("unexpected gemini-weekly: %+v", gw)
	}

	tpw, found := snapshot.Bucket("3p-weekly")
	if !found || tpw.Status != StatusAvailable || *tpw.RemainingFraction != 0.95 {
		t.Fatalf("unexpected 3p-weekly: %+v", tpw)
	}
}

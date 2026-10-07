package quota

import (
	"encoding/json"
	"math"
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
	if snapshot.Pools.Gemini.ID != GeminiPool || snapshot.Pools.ThirdParty.ID != ThirdPartyPool || snapshot.Pools.Gemini.DisplayName != "Gemini Models" || snapshot.Pools.ThirdParty.DisplayName != "Claude and GPT models" {
		t.Fatalf("pool metadata = %+v", snapshot.Pools)
	}
	cases := []struct {
		window   Window
		id       string
		kind     string
		fraction float64
		reset    string
	}{
		{snapshot.Pools.Gemini.FiveHour, "gemini-5h", FiveHourWindow, 0, "2026-10-06T16:30:59Z"},
		{snapshot.Pools.Gemini.Weekly, "gemini-weekly", WeeklyWindow, 0.9, "2026-10-11T16:02:27Z"},
		{snapshot.Pools.ThirdParty.FiveHour, "3p-5h", FiveHourWindow, 0.7, "2026-10-06T16:31:11Z"},
		{snapshot.Pools.ThirdParty.Weekly, "3p-weekly", WeeklyWindow, 0.4, "2026-10-13T04:42:09Z"},
	}
	for _, test := range cases {
		w := test.window
		if w.BucketID != test.id || w.Window != test.kind || w.Status != "available" || w.Disabled || w.UnavailableReason != "" {
			t.Fatalf("mapping %s = %+v", test.id, w)
		}
		if w.RemainingFraction == nil || *w.RemainingFraction != test.fraction || w.RemainingPercent == nil || math.Abs(*w.RemainingPercent-test.fraction*100) > 1e-10 || w.UsedPercent == nil || math.Abs(*w.UsedPercent-(1-test.fraction)*100) > 1e-10 {
			t.Fatalf("percentages %s = %+v", test.id, w)
		}
		if w.ResetAt == nil || w.ResetAt.Format(time.RFC3339) != test.reset || w.RemainingAmount != nil {
			t.Fatalf("authoritative reset/amount %s = %+v", test.id, w)
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || !strings.Contains(string(encoded), `"remaining_fraction":0`) || !strings.Contains(string(encoded), `"remaining_amount":null`) {
		t.Fatalf("JSON preserves zero and absence: %s, %v", encoded, err)
	}
}

func TestParseWindowAvailabilityAndInvalidData(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		status string
		reason string
	}{
		{"missing values", ``, "unavailable", "quota_not_reported"},
		{"null values", `,"remainingFraction":null,"remainingAmount":null,"resetTime":null`, "unavailable", "quota_not_reported"},
		{"disabled", `,"disabled":true,"remainingFraction":0.8`, "disabled", "upstream_disabled"},
		{"disabled malformed fraction", `,"disabled":true,"remainingFraction":"bad"`, "disabled", "upstream_disabled"},
		{"string fraction", `,"remainingFraction":"0.5"`, "unavailable", "invalid_remaining_fraction"},
		{"boolean fraction", `,"remainingFraction":true`, "unavailable", "invalid_remaining_fraction"},
		{"object fraction", `,"remainingFraction":{}`, "unavailable", "invalid_remaining_fraction"},
		{"negative fraction", `,"remainingFraction":-0.01`, "unavailable", "invalid_remaining_fraction"},
		{"overfull fraction", `,"remainingFraction":1.01`, "unavailable", "invalid_remaining_fraction"},
		{"overflow fraction", `,"remainingFraction":1e999`, "unavailable", "invalid_remaining_fraction"},
		{"invalid reset", `,"remainingFraction":0.5,"resetTime":"tomorrow"`, "unavailable", "invalid_reset_time"},
		{"numeric reset", `,"remainingFraction":0.5,"resetTime":123`, "unavailable", "invalid_reset_time"},
		{"empty reset", `,"remainingFraction":0.5,"resetTime":""`, "unavailable", "invalid_reset_time"},
		{"wrong window", `,"remainingFraction":0.5,"window":"weekly"`, "unavailable", "invalid_window"},
		{"invalid disabled", `,"remainingFraction":0.5,"disabled":"false"`, "unavailable", "invalid_disabled"},
		{"numeric amount", `,"remainingAmount":123`, "unavailable", "invalid_remaining_amount"},
		{"fractional amount", `,"remainingAmount":"1.5"`, "unavailable", "invalid_remaining_amount"},
		{"overflow amount", `,"remainingAmount":"9223372036854775808"`, "unavailable", "invalid_remaining_amount"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := Parse([]byte(`{"buckets":[{"bucketId":"gemini-5h"`+test.fields+`}]}`), quotaTestTime())
			if err != nil {
				t.Fatal(err)
			}
			w := snapshot.Pools.Gemini.FiveHour
			if w.Status != test.status || w.UnavailableReason != test.reason || w.RemainingFraction != nil || w.RemainingPercent != nil || w.UsedPercent != nil {
				t.Fatalf("unusable window = %+v", w)
			}
			if w.Disabled != (test.status == "disabled") {
				t.Fatalf("disabled = %v", w.Disabled)
			}
			for _, missing := range []Window{snapshot.Pools.Gemini.Weekly, snapshot.Pools.ThirdParty.FiveHour, snapshot.Pools.ThirdParty.Weekly} {
				if missing.Status != "unavailable" || missing.UnavailableReason != "not_reported" || missing.BucketID == "" || missing.ResetAt != nil || missing.RemainingFraction != nil {
					t.Fatalf("missing bucket = %+v", missing)
				}
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
		w := snapshot.Pools.ThirdParty.Weekly
		if w.Status != "available" || w.RemainingAmount == nil || *w.RemainingAmount != amount || w.RemainingFraction != nil || w.RemainingPercent != nil || w.UsedPercent != nil || w.ResetAt == nil || w.ResetAt.Format(time.RFC3339Nano) != "2026-10-13T04:42:09.123456789+02:00" {
			t.Fatalf("amount-only window = %+v", w)
		}
	}
	snapshot, err := Parse([]byte(`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":1},{"bucketId":"gemini-weekly","disabled":true,"remainingAmount":"7","resetTime":"2026-10-11T16:02:27Z"}]}`), quotaTestTime())
	if err != nil {
		t.Fatal(err)
	}
	if w := snapshot.Pools.Gemini.FiveHour; w.ResetAt != nil || w.RemainingFraction == nil || *w.RemainingFraction != 1 || *w.UsedPercent != 0 {
		t.Fatalf("reset was invented or full fraction lost: %+v", w)
	}
	if w := snapshot.Pools.Gemini.Weekly; w.Status != "disabled" || w.ResetAt == nil || w.RemainingAmount != nil {
		t.Fatalf("disabled reset metadata = %+v", w)
	}
}

func TestParseWrappersDuplicatesAndMalformedEnvelopes(t *testing.T) {
	valid := []string{
		`{}`,
		`{"models":{"gemini-weekly":{"quotaInfo":{"remainingFraction":1}}}}`,
		`{"response":{"summary":{"buckets":[{"bucketId":"3p-5h","remainingFraction":0.5}]}}}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5},{"bucketId":"gemini-5h","remainingFraction":0.50}]}`,
		`{"buckets":[{"bucketId":"gemini-5h","remainingFraction":0.5},{"bucketId":"gemini-5h","window":"5h","disabled":false,"remainingFraction":0.50,"remainingAmount":null}]}`,
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
	legacy, err := Parse([]byte(valid[1]), quotaTestTime())
	if err != nil || legacy.Pools.Gemini.Weekly.Status != "unavailable" || legacy.Pools.Gemini.Weekly.RemainingFraction != nil {
		t.Fatalf("model quota substituted for pool quota: %+v, %v", legacy, err)
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
	*second.Pools.Gemini.FiveHour.RemainingFraction = 0.75
	if *first.Pools.Gemini.FiveHour.RemainingFraction != 0 {
		t.Fatal("independent snapshots share mutable values")
	}
}

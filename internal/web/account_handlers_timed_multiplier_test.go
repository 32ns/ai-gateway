package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseTimedMultiplierFormValuesFromListFields(t *testing.T) {
	form := url.Values{}
	form.Set("timed_multiplier_id", "tm_peak")
	form.Set("timed_multiplier_name_tm_peak", "Peak")
	form.Set("timed_multiplier_value_tm_peak", "1.75")
	form.Set("timed_multiplier_start_date_tm_peak", "2026-05-01")
	form.Set("timed_multiplier_end_date_tm_peak", "2026-05-07")
	form.Set("timed_multiplier_start_time_tm_peak", "22:00")
	form.Set("timed_multiplier_end_time_tm_peak", "02:00")
	form.Set("timed_multiplier_priority_tm_peak", "5")
	form.Add("timed_multiplier_weekday_tm_peak", "1")
	form.Add("timed_multiplier_weekday_tm_peak", "3")
	form.Add("timed_multiplier_enabled", "tm_peak")
	req := httptest.NewRequest("POST", "/admin/account-groups/group_plus/billing", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rules, err := parseTimedMultiplierFormValues(req)
	if err != nil {
		t.Fatalf("parseTimedMultiplierFormValues: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	rule := rules[0]
	if rule.ID != "tm_peak" || rule.Name != "Peak" || !rule.Enabled {
		t.Fatalf("rule identity = %#v", rule)
	}
	if rule.MultiplierBps != 17500 {
		t.Fatalf("multiplier = %d, want 17500", rule.MultiplierBps)
	}
	if rule.StartDate != "2026-05-01" || rule.EndDate != "2026-05-07" || rule.StartTime != "22:00" || rule.EndTime != "02:00" {
		t.Fatalf("rule window = %#v", rule)
	}
	if rule.Priority != 5 {
		t.Fatalf("priority = %d, want 5", rule.Priority)
	}
	if len(rule.Weekdays) != 2 || rule.Weekdays[0] != 1 || rule.Weekdays[1] != 3 {
		t.Fatalf("weekdays = %#v, want [1 3]", rule.Weekdays)
	}
}

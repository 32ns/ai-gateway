package controlplane

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const (
	dateOnlyLayout = "2006-01-02"
	timeOnlyLayout = "15:04"
)

func normalizeAccountGroupTimedMultipliers(rules []core.AccountGroupTimedMultiplier) ([]core.AccountGroupTimedMultiplier, error) {
	out := make([]core.AccountGroupTimedMultiplier, 0, len(rules))
	seen := map[string]struct{}{}
	for i, rule := range rules {
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" {
			rule.ID = fmt.Sprintf("tm_%d_%d", time.Now().UTC().UnixNano(), i)
		}
		if _, ok := seen[rule.ID]; ok {
			return nil, fmt.Errorf("timed multiplier rule ID %q is duplicated", rule.ID)
		}
		seen[rule.ID] = struct{}{}
		rule.Name = strings.TrimSpace(rule.Name)
		if rule.MultiplierBps < 0 {
			return nil, fmt.Errorf("timed multiplier must be zero or greater")
		}
		rule.Weekdays = normalizeWeekdays(rule.Weekdays)
		for _, weekday := range rule.Weekdays {
			if weekday < 1 || weekday > 7 {
				return nil, fmt.Errorf("timed multiplier weekday must be between 1 and 7")
			}
		}
		rule.StartDate = strings.TrimSpace(rule.StartDate)
		rule.EndDate = strings.TrimSpace(rule.EndDate)
		if rule.StartDate != "" {
			if _, err := time.Parse(dateOnlyLayout, rule.StartDate); err != nil {
				return nil, fmt.Errorf("timed multiplier start date must be YYYY-MM-DD")
			}
		}
		if rule.EndDate != "" {
			if _, err := time.Parse(dateOnlyLayout, rule.EndDate); err != nil {
				return nil, fmt.Errorf("timed multiplier end date must be YYYY-MM-DD")
			}
		}
		if rule.StartDate != "" && rule.EndDate != "" && rule.StartDate > rule.EndDate {
			return nil, fmt.Errorf("timed multiplier start date must be before end date")
		}
		rule.StartTime = strings.TrimSpace(rule.StartTime)
		rule.EndTime = strings.TrimSpace(rule.EndTime)
		if (rule.StartTime == "") != (rule.EndTime == "") {
			return nil, fmt.Errorf("timed multiplier time range requires both start and end time")
		}
		if rule.StartTime != "" {
			if _, err := time.Parse(timeOnlyLayout, rule.StartTime); err != nil {
				return nil, fmt.Errorf("timed multiplier start time must be HH:MM")
			}
			if _, err := time.Parse(timeOnlyLayout, rule.EndTime); err != nil {
				return nil, fmt.Errorf("timed multiplier end time must be HH:MM")
			}
		}
		if core.TimedMultiplierExpired(rule, time.Now()) {
			rule.Enabled = false
		}
		out = append(out, rule)
	}
	return out, nil
}

func normalizeWeekdays(in []int) []int {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

package core

import (
	"slices"
	"time"
)

const AccountGroupDefaultMultiplierBps = int64(10000)

func NormalizeAccountGroupBilling(group AccountGroup) AccountGroup {
	group.Type = NormalizeAccountGroupType(group.Type)
	if group.BillingMultiplierBps == 0 {
		group.BillingMultiplierBps = AccountGroupDefaultMultiplierBps
	}
	if group.PlanBillingMultiplierBps == 0 {
		group.PlanBillingMultiplierBps = group.BillingMultiplierBps
	}
	return group
}

func AccountGroupPlanBillingEnabled(group AccountGroup) bool {
	if group.PlanBillingEnabled == nil {
		return true
	}
	return *group.PlanBillingEnabled
}

func EffectiveAccountGroupMultiplier(group AccountGroup, now time.Time) int64 {
	group = NormalizeAccountGroupBilling(group)
	if rule, ok := ActiveAccountGroupTimedMultiplier(group, now); ok {
		return rule.MultiplierBps
	}
	return group.BillingMultiplierBps
}

func EffectiveAccountGroupPlanMultiplier(group AccountGroup, now time.Time) int64 {
	group = NormalizeAccountGroupBilling(group)
	return group.PlanBillingMultiplierBps
}

func ActiveAccountGroupTimedMultiplier(group AccountGroup, now time.Time) (AccountGroupTimedMultiplier, bool) {
	bestPriority := 0
	found := false
	var active AccountGroupTimedMultiplier
	for _, rule := range group.TimedMultipliers {
		if !timedMultiplierMatches(rule, now) {
			continue
		}
		if !found || rule.Priority > bestPriority {
			active = rule
			bestPriority = rule.Priority
			found = true
		}
	}
	return active, found
}

func TimedMultiplierExpired(rule AccountGroupTimedMultiplier, now time.Time) bool {
	if rule.EndDate == "" {
		return false
	}
	today := now.Format("2006-01-02")
	if today > rule.EndDate {
		return true
	}
	if today < rule.EndDate {
		return false
	}
	if rule.EndTime == "" {
		return false
	}
	if rule.StartTime != "" && rule.StartTime > rule.EndTime {
		return false
	}
	return now.Format("15:04") >= rule.EndTime
}

func timedMultiplierMatches(rule AccountGroupTimedMultiplier, now time.Time) bool {
	if !rule.Enabled || rule.MultiplierBps < 0 {
		return false
	}
	if TimedMultiplierExpired(rule, now) {
		return false
	}
	if len(rule.Weekdays) > 0 {
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		if !slices.Contains(rule.Weekdays, weekday) {
			return false
		}
	}
	today := now.Format("2006-01-02")
	if rule.StartDate != "" && today < rule.StartDate {
		return false
	}
	if rule.EndDate != "" && today > rule.EndDate {
		return false
	}
	if rule.StartTime != "" && rule.EndTime != "" {
		current := now.Format("15:04")
		if rule.StartTime <= rule.EndTime {
			return current >= rule.StartTime && current < rule.EndTime
		}
		return current >= rule.StartTime || current < rule.EndTime
	}
	return true
}

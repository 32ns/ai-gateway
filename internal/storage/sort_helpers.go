package storage

import (
	"slices"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

func sortAccounts(out []core.Account) []core.Account {
	slices.SortFunc(out, func(a, b core.Account) int {
		if a.Provider != b.Provider {
			if a.Provider < b.Provider {
				return -1
			}
			return 1
		}
		if a.Priority != b.Priority {
			return b.Priority - a.Priority
		}
		if createdAtOrder := a.CreatedAt.Compare(b.CreatedAt); createdAtOrder != 0 {
			return createdAtOrder
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

func sortAccountGroups(out []core.AccountGroup) []core.AccountGroup {
	slices.SortFunc(out, func(a, b core.AccountGroup) int {
		if strings.ToLower(a.Name) < strings.ToLower(b.Name) {
			return -1
		}
		if strings.ToLower(a.Name) > strings.ToLower(b.Name) {
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return out
}

func sortUsers(out []core.User) []core.User {
	slices.SortFunc(out, func(a, b core.User) int {
		if a.Role != b.Role {
			if a.Role == core.UserRoleAdmin {
				return -1
			}
			if b.Role == core.UserRoleAdmin {
				return 1
			}
		}
		left := strings.ToLower(a.Username)
		right := strings.ToLower(b.Username)
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
		if a.Username < b.Username {
			return -1
		}
		if a.Username > b.Username {
			return 1
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return out
}

func sortClients(out []core.APIClient) []core.APIClient {
	slices.SortFunc(out, func(a, b core.APIClient) int {
		if a.Enabled != b.Enabled {
			if a.Enabled {
				return -1
			}
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return out
}

func sortModels(out []core.ModelConfig) []core.ModelConfig {
	slices.SortFunc(out, func(a, b core.ModelConfig) int {
		if a.Enabled != b.Enabled {
			if a.Enabled {
				return -1
			}
			return 1
		}
		if a.Provider != b.Provider {
			if a.Provider < b.Provider {
				return -1
			}
			return 1
		}
		left := strings.ToLower(a.ID)
		right := strings.ToLower(b.ID)
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

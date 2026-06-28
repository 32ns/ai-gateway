package postgresrepo

import "github.com/32ns/ai-gateway/internal/storageerr"

var ErrNotFound = storageerr.ErrNotFound
var ErrInsufficientBalance = storageerr.ErrInsufficientBalance
var ErrPlanQuotaExhausted = storageerr.ErrPlanQuotaExhausted
var ErrClientSpendLimitExceeded = storageerr.ErrClientSpendLimitExceeded
var ErrBillingRequestConflict = storageerr.ErrBillingRequestConflict
var ErrAmountOverflow = storageerr.ErrAmountOverflow
var ErrBillingClientOwnerMismatch = storageerr.ErrBillingClientOwnerMismatch

package storageerr

import "errors"

var ErrNotFound = errors.New("not found")
var ErrInsufficientBalance = errors.New("insufficient balance")
var ErrPlanQuotaExhausted = errors.New("plan quota exhausted")
var ErrClientSpendLimitExceeded = errors.New("client spend limit exceeded")
var ErrBillingRequestConflict = errors.New("billing request fingerprint conflict")
var ErrAmountOverflow = errors.New("amount overflow")
var ErrBillingClientOwnerMismatch = errors.New("billing client owner mismatch")

package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/gateway"
)

const monitorSchedulerPollInterval = 15 * time.Second

func startMonitorScheduler(ctx context.Context, control *controlplane.Service, gatewayService *gateway.Service) func() error {
	if control == nil || gatewayService == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lastRun := map[string]time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			now := time.Now().UTC()
			for _, target := range control.ListMonitorTargets() {
				if !target.Enabled || target.ID == "" {
					continue
				}
				interval := time.Duration(target.IntervalSeconds) * time.Second
				if interval <= 0 {
					interval = time.Duration(controlplane.DefaultMonitorIntervalSeconds) * time.Second
				}
				if last := lastRun[target.ID]; !last.IsZero() && now.Sub(last) < interval {
					continue
				}
				if !control.BeginMonitorRun(target.ID) {
					continue
				}
				lastRun[target.ID] = now
				err := func() error {
					defer control.EndMonitorRun(target.ID)
					return RunMonitorProbe(ctx, control, gatewayService, target)
				}()
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					log.Printf("status monitor probe failed: target=%s group=%s model=%s err=%v", target.ID, target.AccountGroup, target.Model, err)
				}
			}
			if !sleepContext(ctx, monitorSchedulerPollInterval) {
				return
			}
		}
	}()
	return func() error {
		<-done
		return nil
	}
}

func RunMonitorProbe(ctx context.Context, control *controlplane.Service, gatewayService *gateway.Service, target core.MonitorTarget) error {
	if control == nil || gatewayService == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	timeout := time.Duration(target.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(controlplane.DefaultMonitorTimeoutSeconds) * time.Second
	}
	probe, err := gatewayService.ProbeMonitorTarget(ctx, gateway.MonitorProbeInput{
		TargetID:     target.ID,
		AccountGroup: target.AccountGroup,
		Model:        target.Model,
		Prompt:       target.Prompt,
		Timeout:      timeout,
	})
	if errors.Is(err, context.Canceled) {
		return err
	}
	result := core.MonitorResult{
		ID:           control.NewMonitorResultID(),
		TargetID:     target.ID,
		Status:       probe.Status,
		LatencyMS:    probe.LatencyMS,
		Provider:     probe.Provider,
		AccountID:    probe.AccountID,
		AccountLabel: probe.AccountLabel,
		Attempts:     probe.Attempts,
		ErrorCode:    probe.ErrorCode,
		ErrorMessage: probe.ErrorMessage,
		CheckedAt:    probe.CheckedAt,
	}
	if result.Status == "" {
		result.Status = core.MonitorStatusFailed
	}
	if result.CheckedAt.IsZero() {
		result.CheckedAt = time.Now().UTC()
	}
	if appendErr := control.AppendMonitorResult(result); appendErr != nil {
		return appendErr
	}
	return err
}

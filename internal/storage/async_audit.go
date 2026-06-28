package storage

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const defaultAsyncAuditBuffer = 16384
const maxAsyncAuditBatch = 256

type AsyncAuditRepository struct {
	Repository

	queue   chan core.AuditEvent
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
	dropped atomic.Uint64
}

func NewAsyncAuditRepository(base Repository, buffer int) *AsyncAuditRepository {
	if buffer <= 0 {
		buffer = defaultAsyncAuditBuffer
	}
	repo := &AsyncAuditRepository{
		Repository: base,
		queue:      make(chan core.AuditEvent, buffer),
		done:       make(chan struct{}),
	}
	repo.wg.Add(1)
	go repo.run()
	return repo
}

func (r *AsyncAuditRepository) AppendAudit(event core.AuditEvent) error {
	event = auditQueueSummaryEvent(event)
	if event.EffectiveKind() == core.AuditKindAdmin {
		select {
		case <-r.done:
			return r.Repository.AppendAudit(event)
		default:
		}
		select {
		case r.queue <- event:
			return nil
		case <-r.done:
			return r.Repository.AppendAudit(event)
		}
	}
	select {
	case r.queue <- event:
	default:
		r.dropped.Add(1)
	}
	return nil
}

func (r *AsyncAuditRepository) UpsertRuntimeAccount(account core.Account) error {
	repo, ok := r.Repository.(interface {
		UpsertRuntimeAccount(core.Account) error
	})
	if !ok {
		return r.Repository.UpsertAccount(account)
	}
	return repo.UpsertRuntimeAccount(account)
}

func (r *AsyncAuditRepository) TouchUserLastUsedAt(userID string, usedAt time.Time) error {
	repo, ok := r.Repository.(interface {
		TouchUserLastUsedAt(string, time.Time) error
	})
	if !ok {
		user, err := r.Repository.GetUser(userID)
		if err != nil {
			return err
		}
		if usedAt.IsZero() {
			usedAt = time.Now().UTC()
		} else {
			usedAt = usedAt.UTC()
		}
		if user.LastLoginAt != nil && !usedAt.After(*user.LastLoginAt) {
			return nil
		}
		user.LastLoginAt = &usedAt
		return r.Repository.UpdateUserMetadata(user)
	}
	return repo.TouchUserLastUsedAt(userID, usedAt)
}

func (r *AsyncAuditRepository) GetStartupSystemSettings() (core.SystemSettings, error) {
	return LoadStartupSystemSettings(r.Repository)
}

func (r *AsyncAuditRepository) DroppedAuditEvents() uint64 {
	return r.dropped.Load()
}

func (r *AsyncAuditRepository) ConfigRevision() uint64 {
	repo, ok := r.Repository.(interface{ ConfigRevision() uint64 })
	if !ok {
		return 0
	}
	return repo.ConfigRevision()
}

func (r *AsyncAuditRepository) AccountRevision() uint64 {
	repo, ok := r.Repository.(interface{ AccountRevision() uint64 })
	if !ok {
		return 0
	}
	return repo.AccountRevision()
}

func (r *AsyncAuditRepository) UserRevision() uint64 {
	repo, ok := r.Repository.(interface{ UserRevision() uint64 })
	if !ok {
		return 0
	}
	return repo.UserRevision()
}

func (r *AsyncAuditRepository) ModelRevision() uint64 {
	repo, ok := r.Repository.(interface{ ModelRevision() uint64 })
	if !ok {
		return 0
	}
	return repo.ModelRevision()
}

func (r *AsyncAuditRepository) ClientRevision() uint64 {
	repo, ok := r.Repository.(interface{ ClientRevision() uint64 })
	if !ok {
		return 0
	}
	return repo.ClientRevision()
}

func (r *AsyncAuditRepository) Close() error {
	r.once.Do(func() {
		close(r.done)
		r.wg.Wait()
	})
	return nil
}

func (r *AsyncAuditRepository) run() {
	defer r.wg.Done()
	batch := make([]core.AuditEvent, 0, maxAsyncAuditBatch)
	for {
		select {
		case event := <-r.queue:
			batch = r.collectBatch(batch[:0], event)
			_ = r.appendAuditEvents(batch)
		case <-r.done:
			r.drainAuditQueue(batch[:0])
			return
		}
	}
}

func (r *AsyncAuditRepository) collectBatch(events []core.AuditEvent, first core.AuditEvent) []core.AuditEvent {
	events = append(events, first)
	for len(events) < maxAsyncAuditBatch {
		select {
		case event := <-r.queue:
			events = append(events, event)
		default:
			return events
		}
	}
	return events
}

func (r *AsyncAuditRepository) drainAuditQueue(batch []core.AuditEvent) {
	for {
		select {
		case event := <-r.queue:
			batch = r.collectBatch(batch[:0], event)
			_ = r.appendAuditEvents(batch)
		default:
			return
		}
	}
}

type auditBatchAppender interface {
	AppendAuditBatch(events []core.AuditEvent) error
}

func (r *AsyncAuditRepository) appendAuditEvents(events []core.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	if batcher, ok := r.Repository.(auditBatchAppender); ok {
		return batcher.AppendAuditBatch(events)
	}
	for _, event := range events {
		if err := r.Repository.AppendAudit(event); err != nil {
			return err
		}
	}
	return nil
}
